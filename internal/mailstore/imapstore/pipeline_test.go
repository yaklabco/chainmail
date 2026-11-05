package imapstore

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// pipelineMode is how the scripted server answers the body FETCHes a client
// pipelines, one per message.
type pipelineMode int

const (
	// inOrder answers each FETCH in full, data then completion, as it arrives.
	inOrder pipelineMode = iota
	// dataFirst waits for every FETCH, then sends all their data and only
	// then all their completions. Dovecot does this when it runs pipelined
	// commands in parallel.
	dataFirst
	// dataThenStall is dataFirst without the completions: the server stops
	// answering mid-FETCH.
	dataThenStall
	// slowly answers each FETCH in order, waiting delay before each one.
	slowly
)

// scriptServer is an IMAP server over implicit TLS with one folder, INBOX,
// holding n single-part text messages with UIDs 1 to n, unless structure
// gives their body structure. It answers just
// enough of IMAP for the Store's reads.
type scriptServer struct {
	n     int
	mode  pipelineMode
	delay time.Duration
	// endless answers every partial body FETCH with as many bytes as were
	// asked for, as a broken server might, so a stream never runs short.
	endless bool
	// dropAfter, when set, hangs up instead of answering body FETCH number
	// dropAfter+1 on a connection.
	dropAfter int
	// refuse names a command, SEARCH or FETCH, that is answered with refusal.
	refuse, refusal string
	// refuseText, when set, refuses the body FETCH of this one UID with NO.
	refuseText int
	// refuseTextWith, when set, is the refusal refuseText sends instead of
	// NO [SERVERBUG].
	refuseTextWith string
	// encoding is every message's Content-Transfer-Encoding, 7bit if unset,
	// and text, when set, is every message's body instead of scriptBody.
	encoding, text string
	// condstore advertises CONDSTORE, and SELECT and EXAMINE report a
	// HIGHESTMODSEQ and a UIDNEXT.
	condstore bool
	// stateDelay is how long a state FETCH, one with no body items, waits
	// before answering for each message.
	stateDelay time.Duration
	// stallAfter, when set, makes a state FETCH go silent after answering
	// for that many messages, until the client hangs up.
	stallAfter int
	// stallBeforeDone makes a state FETCH go silent after answering for
	// every message, before the tagged OK, until the client hangs up.
	stallBeforeDone bool
	// structure, when set, is every message's BODYSTRUCTURE in a summary
	// FETCH instead of a text/plain part.
	structure string
	// badStructure, when set, is the UID whose BODYSTRUCTURE a summary
	// FETCH sends in a form the client cannot parse, and badEnvelope the
	// UID whose ENVELOPE it sends so.
	badStructure, badEnvelope int
	// badSelect answers SELECT and EXAMINE in a form the client cannot
	// parse.
	badSelect bool
	// dropSummaries is how many summary FETCHes, counting across
	// connections, hang up partway through their answer.
	dropSummaries atomic.Int32

	conns, searches, fetches atomic.Int32

	// summarySets counts the summary FETCHes by the UID set they asked
	// for, with " without BODYSTRUCTURE" after a set that left it out.
	mu          sync.Mutex
	summarySets map[string]int

	ca *tlstest.CA
	ep provider.Endpoint
}

const scriptUIDValidity = 7

// scriptModSeq is the HIGHESTMODSEQ a CONDSTORE script server reports.
const scriptModSeq = 9

func startScriptServer(t *testing.T, srv *scriptServer) *scriptServer {
	t.Helper()
	srv.ca = tlstest.New(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv.ca.Server)
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close() // ends Accept; nothing to do about a close error at test end
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			srv.conns.Add(1)
			wg.Go(func() {
				defer func() { _ = conn.Close() }() // the client may already have hung up
				stop := context.AfterFunc(t.Context(), func() { _ = conn.Close() })
				defer stop()
				srv.serve(conn)
			})
		}
	})
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	srv.ep = provider.Endpoint{Host: "127.0.0.1", Port: addr.Port, Security: provider.SecurityTLS}
	return srv
}

func scriptStore(t *testing.T, srv *scriptServer, opts Options) *Store {
	t.Helper()
	opts.RootCAs = srv.ca.Pool
	if opts.Backoff == nil {
		opts.Backoff = noBackoff
	}
	s := New(testAccount("host", srv.ep, false), passwordCreds("host@example.test", "pw"), opts)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func scriptHeader(uid int) string {
	return fmt.Sprintf("From: a@example.com\r\nSubject: m%d\r\nMessage-ID: <m%d@x>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n", uid, uid)
}

func scriptBody(uid int) string { return fmt.Sprintf("body %d\r\n", uid) }

// bodyOf is the body of message uid: text when set, else scriptBody.
func (srv *scriptServer) bodyOf(uid int) string {
	if srv.text != "" {
		return srv.text
	}
	return scriptBody(uid)
}

var (
	uidFetch    = regexp.MustCompile(`^UID FETCH (\S+) `)
	peekSection = regexp.MustCompile(`BODY\.PEEK\[([^\]]*)\](?:<(\d+)\.(\d+)>)?`)
)

func (srv *scriptServer) serve(conn net.Conn) {
	w := bufio.NewWriter(conn)
	send := func(s string) bool {
		if _, err := w.WriteString(s); err != nil {
			return false
		}
		return w.Flush() == nil
	}
	if !send("* OK [CAPABILITY " + srv.caps() + "] ready\r\n") {
		return
	}
	r := bufio.NewReader(conn)
	var held []string // body FETCH commands not yet answered
	bodies := 0       // body FETCHes seen on this connection
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		cmd, _, _ := strings.Cut(rest, " ")
		var reply string
		switch strings.ToUpper(cmd) {
		case "LOGIN":
			reply = tag + " OK logged in\r\n"
		case "CAPABILITY":
			reply = "* CAPABILITY " + srv.caps() + "\r\n" + tag + " OK done\r\n"
		case "EXAMINE", "SELECT":
			reply = fmt.Sprintf("* %d EXISTS\r\n* OK [UIDVALIDITY %d] ok\r\n", srv.n, scriptUIDValidity)
			if srv.badSelect {
				reply = fmt.Sprintf("* %d EXISTS\r\n* OK [UIDVALIDITY x] ok\r\n", srv.n)
			}
			if srv.condstore {
				reply += fmt.Sprintf("* OK [UIDNEXT %d] ok\r\n* OK [HIGHESTMODSEQ %d] ok\r\n", srv.n+1, scriptModSeq)
			}
			reply += tag + " OK [READ-ONLY] done\r\n"
		case "LOGOUT":
			send("* BYE bye\r\n" + tag + " OK bye\r\n")
			return
		case "UID":
			kind := "FETCH"
			if strings.HasPrefix(strings.ToUpper(rest), "UID SEARCH") {
				kind = "SEARCH"
				srv.searches.Add(1)
			} else {
				srv.fetches.Add(1)
			}
			switch {
			case kind == srv.refuse:
				reply = tag + " " + srv.refusal + "\r\n"
			case kind == "FETCH" && (strings.Contains(rest, "ENVELOPE") || strings.Contains(rest, "BODYSTRUCTURE")):
				srv.countSummary(rest)
				if srv.dropSummaries.Add(-1) >= 0 {
					send("* 1 FETCH (UID 1 ")
					return
				}
				reply = srv.summaries(rest) + tag + " OK done\r\n"
			case kind == "FETCH" && !strings.Contains(strings.ToUpper(rest), "BODY"):
				if !srv.states(tag, rest, send, r) {
					return
				}
				continue
			case kind == "SEARCH":
				reply = "* SEARCH 1\r\n" + tag + " OK done\r\n"
			default:
				bodies++
				if srv.dropAfter > 0 && bodies > srv.dropAfter {
					return
				}
				var ok bool
				if reply, ok = srv.body(tag+" "+rest, &held); !ok {
					continue
				}
			}
		default:
			reply = tag + " BAD unknown command\r\n"
		}
		if !send(reply) {
			return
		}
	}
}

// body queues one body FETCH and returns what to send for the queue so far,
// or false to wait for more.
func (srv *scriptServer) body(cmd string, held *[]string) (string, bool) {
	*held = append(*held, cmd)
	if srv.mode == inOrder || srv.mode == slowly {
		if srv.mode == slowly {
			time.Sleep(srv.delay)
		}
		data, done := srv.answer((*held)[0])
		*held = nil
		return data + done, true
	}
	if len(*held) < srv.n {
		return "", false
	}
	var data, done strings.Builder
	for _, h := range *held {
		d, ok := srv.answer(h)
		data.WriteString(d)
		done.WriteString(ok)
	}
	*held = nil
	if srv.mode == dataFirst {
		data.WriteString(done.String())
	}
	return data.String(), true
}

// summaries answers the summary FETCH for each message of the UID set it
// asks for, echoing the body section asked for with the whole header. It
// sends BODYSTRUCTURE only when asked, and badStructure's and
// badEnvelope's messages in a form the client cannot parse.
func (srv *scriptServer) summaries(rest string) string {
	sec := "HEADER"
	if m := peekSection.FindStringSubmatch(rest); m != nil {
		sec = m[1]
	}
	encoding := srv.encoding
	if encoding == "" {
		encoding = "7bit"
	}
	asked := scriptUIDSet(rest, srv.n)
	var b strings.Builder
	for uid := 1; uid <= srv.n; uid++ {
		if !asked[uid] {
			continue
		}
		h := scriptHeader(uid)
		structure := fmt.Sprintf("(\"text\" \"plain\" (\"charset\" \"utf-8\") NIL NIL \"%s\" %d 1 NIL NIL NIL NIL)", encoding, len(srv.bodyOf(uid)))
		if srv.structure != "" {
			structure = srv.structure
		}
		if uid == srv.badStructure {
			structure = `("TEXT" NIL)` // a body type without its subtype
		}
		if strings.Contains(rest, "BODYSTRUCTURE") {
			structure = "BODYSTRUCTURE " + structure + " "
		} else {
			structure = ""
		}
		envelope := fmt.Sprintf("(\"Mon, 26 Oct 2026 09:00:00 +0000\" \"m%d\" ((NIL NIL \"a\" \"example.com\")) ((NIL NIL \"a\" \"example.com\")) "+
			"((NIL NIL \"a\" \"example.com\")) ((NIL NIL \"me\" \"example.test\")) NIL NIL NIL \"<m%d@x>\")", uid, uid)
		if uid == srv.badEnvelope {
			envelope = "(NIL)" // a date and nothing after it
		}
		fmt.Fprintf(&b, "* %d FETCH (UID %d INTERNALDATE \"20-Oct-2026 09:00:00 +0000\" RFC822.SIZE %d FLAGS () "+
			"%sENVELOPE %s BODY[%s] {%d}\r\n%s)\r\n",
			uid, uid, len(h)+len(srv.bodyOf(uid)), structure, envelope, sec, len(h), h)
	}
	return b.String()
}

// countSummary counts a summary FETCH in summarySets.
func (srv *scriptServer) countSummary(rest string) {
	key := "?"
	if m := uidFetch.FindStringSubmatch(rest); m != nil {
		key = m[1]
	}
	if !strings.Contains(rest, "BODYSTRUCTURE") {
		key += " without BODYSTRUCTURE"
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.summarySets == nil {
		srv.summarySets = map[string]int{}
	}
	srv.summarySets[key]++
}

// summaryFetches returns how many summary FETCHes asked for set.
func (srv *scriptServer) summaryFetches(set string) int {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.summarySets[set]
}

// scriptUIDSet reads the UID set of a UID FETCH, such as 1:4,7, into the
// UIDs from 1 to n it names. A set it cannot read names every one.
func scriptUIDSet(rest string, n int) map[int]bool {
	out := map[int]bool{}
	m := uidFetch.FindStringSubmatch(rest)
	if m == nil {
		for uid := 1; uid <= n; uid++ {
			out[uid] = true
		}
		return out
	}
	for _, r := range strings.Split(m[1], ",") {
		lo, hi, isRange := strings.Cut(r, ":")
		start, err := strconv.Atoi(lo)
		if err != nil {
			start = n
		}
		stop := start
		if isRange {
			if stop, err = strconv.Atoi(hi); err != nil {
				stop = n
			}
		}
		for uid := min(start, stop); uid <= max(start, stop); uid++ {
			out[uid] = true
		}
	}
	return out
}

// answer returns the data and the completion for one body FETCH.
func (srv *scriptServer) answer(cmd string) (data, done string) {
	tag, rest, _ := strings.Cut(cmd, " ")
	done = tag + " OK done\r\n"
	m := uidFetch.FindStringSubmatch(rest)
	sec := peekSection.FindStringSubmatch(rest)
	if m == nil || sec == nil {
		return "", tag + " BAD unexpected FETCH\r\n"
	}
	uid, err := strconv.Atoi(m[1])
	if err != nil {
		return "", tag + " BAD unexpected FETCH\r\n"
	}
	if uid == srv.refuseText {
		refusal := srv.refuseTextWith
		if refusal == "" {
			refusal = "NO [SERVERBUG] cannot read this message"
		}
		return "", tag + " " + refusal + "\r\n"
	}
	body := srv.bodyOf(uid)
	if sec[1] == "" {
		body = scriptHeader(uid) + body // the whole message
	}
	origin := ""
	if sec[2] != "" {
		off, _ := strconv.Atoi(sec[2])  // the pattern only matches digits
		size, _ := strconv.Atoi(sec[3]) // likewise
		switch {
		case srv.endless:
			body = strings.Repeat("x", size)
		case off >= len(body):
			body = ""
		default:
			body = body[off:min(off+size, len(body))]
		}
		origin = "<" + sec[2] + ">"
	}
	return fmt.Sprintf("* %d FETCH (UID %d BODY[%s]%s {%d}\r\n%s)\r\n", uid, uid, sec[1], origin, len(body), body), done
}

func scriptRefs(n int) []ref.Ref {
	refs := make([]ref.Ref, n)
	for i := range refs {
		refs[i] = ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: scriptUIDValidity, UID: uint32(i + 1)}
	}
	return refs
}

// fetchWithin runs FetchMessages and fails the test if it is still running
// after limit.
func fetchWithin(t *testing.T, s *Store, refs []ref.Ref, limit time.Duration) []mailstore.MessageResult {
	t.Helper()
	got := make(chan []mailstore.MessageResult, 1)
	go func() { got <- s.FetchMessages(t.Context(), refs) }()
	select {
	case res := <-got:
		return res
	case <-time.After(limit):
		t.Fatalf("FetchMessages still running after %v", limit)
		return nil
	}
}

func TestFetchMessagesReadsPipelinedRepliesInAnyOrder(t *testing.T) {
	t.Parallel()
	srv := &scriptServer{n: 3, mode: dataFirst}
	s := scriptStore(t, startScriptServer(t, srv), Options{CommandTimeout: 5 * time.Second})
	res := fetchWithin(t, s, scriptRefs(srv.n), 3*time.Second)
	for i, r := range res {
		if r.Err != nil {
			t.Fatalf("ref %d: %v", i, r.Err)
		}
		if !strings.Contains(string(r.Message.Raw), scriptBody(i+1)) {
			t.Errorf("ref %d raw = %q, want its body", i, r.Message.Raw)
		}
	}
}

func TestFetchMessagesGivesUpWhenTheServerStallsMidFetch(t *testing.T) {
	t.Parallel()
	srv := &scriptServer{n: 2, mode: dataThenStall}
	const timeout = 200 * time.Millisecond
	s := scriptStore(t, startScriptServer(t, srv), Options{CommandTimeout: timeout})
	start := time.Now()
	res := fetchWithin(t, s, scriptRefs(srv.n), 5*time.Second)
	for i, r := range res {
		if !errors.Is(r.Err, mailstore.ErrUnavailable) {
			t.Errorf("ref %d: %+v, want ErrUnavailable", i, r)
		}
	}
	// A timed-out command is retried once on a fresh connection, not three times.
	if n := srv.conns.Load(); n != 2 {
		t.Errorf("%d connections, want 2: one try and one retry", n)
	}
	if elapsed := time.Since(start); elapsed > 10*timeout {
		t.Errorf("FetchMessages took %v with a %v command timeout", elapsed, timeout)
	}
}

func TestFetchMessagesBoundsEachPipelinedCommand(t *testing.T) {
	t.Parallel()
	// Each reply comes well within the timeout, but all four together do not.
	srv := &scriptServer{n: 4, mode: slowly, delay: 150 * time.Millisecond}
	s := scriptStore(t, startScriptServer(t, srv), Options{CommandTimeout: 400 * time.Millisecond, Retries: -1})
	res := fetchWithin(t, s, scriptRefs(srv.n), 5*time.Second)
	for i, r := range res {
		if r.Err != nil {
			t.Errorf("ref %d: %v", i, r.Err)
		}
	}
}

func TestFetchSummariesReadsPipelinedSamplesInAnyOrder(t *testing.T) {
	t.Parallel()
	srv := &scriptServer{n: 3, mode: dataFirst}
	s := scriptStore(t, startScriptServer(t, srv), Options{CommandTimeout: 5 * time.Second})
	type result struct {
		sums []mailstore.Summary
		err  error
	}
	got := make(chan result, 1)
	go func() {
		sums, err := s.FetchSummaries(t.Context(), mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, []uint32{3, 2, 1}, mailstore.SummaryOptions{Snippets: true})
		got <- result{sums, err}
	}()
	var r result
	select {
	case r = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("FetchSummaries still running after 3s")
	}
	if r.err != nil || len(r.sums) != srv.n {
		t.Fatalf("FetchSummaries = %+v, %v", r.sums, r.err)
	}
	for _, sum := range r.sums {
		if sum.Sample == nil || string(sum.Sample.Data) != scriptBody(int(sum.Ref.UID)) {
			t.Errorf("UID %d sample = %+v, want its body", sum.Ref.UID, sum.Sample)
		}
	}
}

func TestServerRefusalsAreNotRetried(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ refuse, refusal string }{
		"search NO":  {"SEARCH", "NO [SERVERBUG] search failed"},
		"search BAD": {"SEARCH", "BAD unsupported criterion"},
		"fetch NO":   {"FETCH", "NO [SERVERBUG] fetch failed"},
		"fetch BAD":  {"FETCH", "BAD unsupported item"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: 1, refuse: tc.refuse, refusal: tc.refusal})
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
			var err error
			sent := &srv.fetches
			if tc.refuse == "SEARCH" {
				sent = &srv.searches
				var res []mailstore.SourceResult
				res, err = s.Search(t.Context(), mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{"INBOX": {UIDValidity: scriptUIDValidity}}})
				if err == nil && len(res) == 1 {
					err = res[0].Err
				}
			} else {
				err = fetchWithin(t, s, scriptRefs(1), 5*time.Second)[0].Err
			}
			if !errors.Is(err, mailstore.ErrUnavailable) || errors.Is(err, errConnLost) {
				t.Errorf("err = %v, want ErrUnavailable from the refusal", err)
			}
			if n, c := sent.Load(), srv.conns.Load(); n != 1 || c != 1 {
				t.Errorf("%s sent %d times on %d connections, want once on one: a refusal is final", tc.refuse, n, c)
			}
		})
	}
}

// TestFetchMessagesRefusalOfOneMessageIsPerItem checks that a server refusing
// the text of one message fails that message alone, as unreadable rather
// than unavailable, so a thread can leave it out.
func TestFetchMessagesRefusalOfOneMessageIsPerItem(t *testing.T) {
	t.Parallel()
	srv := startScriptServer(t, &scriptServer{n: 3, refuseText: 2})
	s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
	res := fetchWithin(t, s, scriptRefs(srv.n), 5*time.Second)
	for i, r := range res {
		if i == 1 {
			if r.Err == nil || errors.Is(r.Err, mailstore.ErrUnavailable) || errors.Is(r.Err, mailstore.ErrNotFound) || errors.Is(r.Err, mailstore.ErrStaleRef) {
				t.Errorf("ref 2: %+v, want a permanent error of its own", r)
			}
			continue
		}
		if r.Err != nil {
			t.Errorf("ref %d: %v", i+1, r.Err)
		}
	}
	if n := srv.conns.Load(); n != 1 {
		t.Errorf("%d connections, want 1: a refusal is final", n)
	}
}

// caps is what the server advertises.
func (srv *scriptServer) caps() string {
	if srv.condstore {
		return "IMAP4rev1 CONDSTORE"
	}
	return "IMAP4rev1"
}

// scriptMsgID is the X-GM-MSGID of message uid.
func scriptMsgID(uid int) uint64 { return 1000 + uint64(uid) }

// states streams the answer to a state FETCH, one with no body items: UID
// for every message, with FLAGS and X-GM-MSGID when asked, each sent on its
// own after stateDelay. With stallAfter set it goes silent after that many
// messages, and with stallBeforeDone after the last one, before the tagged
// OK, and waits for the client to hang up. It reports whether the
// connection is still usable.
//
// It answers for every message whatever UID set and CHANGEDSINCE were asked
// for: the tests that use it bound timing, not which messages a pass asks
// about.
func (srv *scriptServer) states(tag, rest string, send func(string) bool, r *bufio.Reader) bool {
	items := strings.ToUpper(rest)
	for uid := 1; uid <= srv.n; uid++ {
		if srv.stallAfter > 0 && uid > srv.stallAfter {
			_, _ = io.Copy(io.Discard, r) // silent until the client hangs up; its error is that hang-up
			return false
		}
		time.Sleep(srv.stateDelay)
		data := fmt.Sprintf("UID %d", uid)
		if strings.Contains(items, "FLAGS") {
			data += ` FLAGS (\Seen)`
		}
		if strings.Contains(items, "X-GM-MSGID") {
			data += fmt.Sprintf(" X-GM-MSGID %d", scriptMsgID(uid))
		}
		if !send(fmt.Sprintf("* %d FETCH (%s)\r\n", uid, data)) {
			return false
		}
	}
	if srv.stallBeforeDone {
		_, _ = io.Copy(io.Discard, r) // silent until the client hangs up; its error is that hang-up
		return false
	}
	return send(tag + " OK done\r\n")
}
