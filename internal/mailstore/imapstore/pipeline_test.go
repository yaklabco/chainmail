package imapstore

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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
// holding n single-part text messages with UIDs 1 to n. It answers just
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
	// encoding is every message's Content-Transfer-Encoding, 7bit if unset,
	// and text, when set, is every message's body instead of scriptBody.
	encoding, text string

	conns, searches, fetches atomic.Int32

	ca *tlstest.CA
	ep provider.Endpoint
}

const scriptUIDValidity = 7

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
	if !send("* OK [CAPABILITY IMAP4rev1] ready\r\n") {
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
			reply = "* CAPABILITY IMAP4rev1\r\n" + tag + " OK done\r\n"
		case "EXAMINE", "SELECT":
			reply = fmt.Sprintf("* %d EXISTS\r\n* OK [UIDVALIDITY %d] ok\r\n%s OK [READ-ONLY] done\r\n", srv.n, scriptUIDValidity, tag)
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
			case kind == "SEARCH":
				reply = "* SEARCH 1\r\n" + tag + " OK done\r\n"
			case strings.Contains(rest, "BODYSTRUCTURE"):
				reply = srv.summaries(rest) + tag + " OK done\r\n"
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

// summaries answers the summary FETCH for every message, echoing the body
// section asked for with the whole header.
func (srv *scriptServer) summaries(rest string) string {
	sec := "HEADER"
	if m := peekSection.FindStringSubmatch(rest); m != nil {
		sec = m[1]
	}
	encoding := srv.encoding
	if encoding == "" {
		encoding = "7bit"
	}
	var b strings.Builder
	for uid := 1; uid <= srv.n; uid++ {
		h := scriptHeader(uid)
		fmt.Fprintf(&b, "* %d FETCH (UID %d INTERNALDATE \"20-Oct-2026 09:00:00 +0000\" RFC822.SIZE %d FLAGS () "+
			"BODYSTRUCTURE (\"text\" \"plain\" (\"charset\" \"utf-8\") NIL NIL \"%s\" %d 1 NIL NIL NIL NIL) "+
			"ENVELOPE (\"Mon, 26 Oct 2026 09:00:00 +0000\" \"m%d\" ((NIL NIL \"a\" \"example.com\")) ((NIL NIL \"a\" \"example.com\")) "+
			"((NIL NIL \"a\" \"example.com\")) ((NIL NIL \"me\" \"example.test\")) NIL NIL NIL \"<m%d@x>\") BODY[%s] {%d}\r\n%s)\r\n",
			uid, uid, len(h)+len(srv.bodyOf(uid)), encoding, len(srv.bodyOf(uid)), uid, uid, sec, len(h), h)
	}
	return b.String()
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
