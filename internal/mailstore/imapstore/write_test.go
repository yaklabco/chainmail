package imapstore

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// writeServer is a scripted IMAP server for the write paths, over implicit
// TLS. It answers just enough IMAP for the Store's writes: LOGIN, LIST,
// STATUS, SELECT and EXAMINE, UID FETCH, UID SEARCH by Message-ID, UID
// STORE, UID COPY, UID MOVE, UID EXPUNGE and APPEND. It reads commands as
// they arrive, apart from answering them, and logs both, so a test can see
// which commands a client pipelined.
type writeServer struct {
	// caps follow IMAP4rev1 in CAPABILITY, such as "MOVE UIDPLUS".
	caps    string
	folders []*wsFolder
	// holdFrom and holdCount hold back the replies from the first command
	// of kind holdFrom on until holdCount commands are held, then answer
	// them in order. A client that waits for each reply before sending the
	// next command stalls until its command timeout.
	holdFrom  string
	holdCount int
	// slowKind's replies wait slow first.
	slowKind string
	slow     time.Duration
	// dropOn hangs up when a command of that kind arrives.
	dropOn string
	// noCopyUID leaves COPYUID out of COPY and MOVE replies, even with
	// UIDPLUS advertised.
	noCopyUID bool
	// refuse answers commands of that kind NO.
	refuse string
	// after runs once each reply is made, with mu held, so a test can
	// change the folders between two pipelined commands, as another client
	// would.
	after func(srv *writeServer, kind string)
	// delim is the hierarchy delimiter LIST reports. Empty means "/".
	delim string

	mu     sync.Mutex
	events []string // "recv UID MOVE 1:2 Archive", "sent UID MOVE"

	conns, active, peak atomic.Int32

	ca *tlstest.CA
	ep provider.Endpoint
}

type wsFolder struct {
	name, attr string
	validity   uint32
	next       uint32
	msgs       []wsMsg
}

type wsMsg struct {
	uid uint32
	id  string // Message-ID without brackets
}

// wsMailbox makes a folder holding messages with the given UIDs, each with
// Message-ID <uid@x>.
func wsMailbox(name, attr string, validity uint32, uids ...uint32) *wsFolder {
	f := &wsFolder{name: name, attr: attr, validity: validity, next: 100}
	for _, u := range uids {
		f.msgs = append(f.msgs, wsMsg{uid: u, id: fmt.Sprintf("%d@x", u)})
	}
	return f
}

func startWriteServer(t *testing.T, srv *writeServer) *writeServer {
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
				srv.notePeak(srv.active.Add(1))
				defer srv.active.Add(-1)
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

func writeStore(t *testing.T, srv *writeServer, opts Options) *Store {
	t.Helper()
	opts.RootCAs = srv.ca.Pool
	if opts.Backoff == nil {
		opts.Backoff = noBackoff
	}
	if opts.CommandTimeout == 0 {
		opts.CommandTimeout = 2 * time.Second
	}
	s := New(testAccount("host", srv.ep, false), passwordCreds("host@example.test", "pw"), opts)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// notePeak raises peak to n, the connections open now, if n is higher.
func (srv *writeServer) notePeak(n int32) {
	for {
		p := srv.peak.Load()
		if n <= p || srv.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

func (srv *writeServer) log(e string) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.events = append(srv.events, e)
}

// received returns the commands received, without tags, in order.
func (srv *writeServer) received() []string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	var out []string
	for _, e := range srv.events {
		if c, ok := strings.CutPrefix(e, "recv "); ok {
			out = append(out, c)
		}
	}
	return out
}

// eventIndex returns the position of the first event with prefix, or -1.
func (srv *writeServer) eventIndex(prefix string) int {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return slices.IndexFunc(srv.events, func(e string) bool { return strings.HasPrefix(e, prefix) })
}

// sent counts the received commands that start with prefix.
func (srv *writeServer) sent(prefix string) int {
	n := 0
	for _, c := range srv.received() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (srv *writeServer) folder(name string) *wsFolder {
	for _, f := range srv.folders {
		if f.name == name {
			return f
		}
	}
	return nil
}

type wsCmd struct {
	tag, kind, rest string
	literal         []byte
}

var wsLiteral = regexp.MustCompile(`\{(\d+)(\+?)\}$`)

// kindOf is a command's name: "UID MOVE", "SELECT".
func kindOf(rest string) string {
	f := strings.Fields(strings.ToUpper(rest))
	switch {
	case len(f) == 0:
		return ""
	case f[0] == "UID" && len(f) > 1:
		return "UID " + f[1]
	default:
		return f[0]
	}
}

func (srv *writeServer) serve(conn net.Conn) {
	var wmu sync.Mutex
	w := bufio.NewWriter(conn)
	send := func(s string) bool {
		wmu.Lock()
		defer wmu.Unlock()
		if _, err := w.WriteString(s); err != nil {
			return false
		}
		return w.Flush() == nil
	}
	if !send("* OK [" + srv.capability() + "] ready\r\n") {
		return
	}
	cmds := make(chan wsCmd, 64)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(cmds)
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			var lit []byte
			if m := wsLiteral.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[1]) // the pattern only matches digits
				if m[2] == "" && !send("+ go\r\n") {
					return
				}
				lit = make([]byte, n)
				if _, err := io.ReadFull(r, lit); err != nil {
					return
				}
				tail, err := r.ReadString('\n')
				if err != nil {
					return
				}
				line = line[:len(line)-len(m[0])] + "{literal}" + strings.TrimRight(tail, "\r\n")
			}
			tag, rest, _ := strings.Cut(line, " ")
			srv.log("recv " + rest)
			select {
			case cmds <- wsCmd{tag: tag, kind: kindOf(rest), rest: rest, literal: lit}:
			case <-done:
				return
			}
		}
	}()
	var st wsSession
	var held []wsCmd
	for c := range cmds {
		if c.kind == "LOGOUT" {
			send("* BYE bye\r\n" + c.tag + " OK bye\r\n")
			return
		}
		if srv.dropOn != "" && c.kind == srv.dropOn {
			return
		}
		batch := []wsCmd{c}
		if srv.holdFrom != "" && (len(held) > 0 || c.kind == srv.holdFrom) {
			held = append(held, c)
			if len(held) < srv.holdCount {
				continue
			}
			batch, held = held, nil
		}
		for _, b := range batch {
			if b.kind == srv.slowKind {
				time.Sleep(srv.slow)
			}
			// Logged before it goes out, so a command the client sends on
			// hearing this reply is always logged after it.
			srv.log("sent " + b.kind)
			if !send(srv.reply(&st, b)) {
				return
			}
		}
	}
}

type wsSession struct {
	selected *wsFolder
}

func wsUnquote(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}

// wsSet parses a UID set such as 1:3,7.
func wsSet(s string) func(uint32) bool {
	type rng struct{ lo, hi uint32 }
	var rs []rng
	for part := range strings.SplitSeq(s, ",") {
		lo, hi, ok := strings.Cut(part, ":")
		if !ok {
			hi = lo
		}
		a, _ := strconv.ParseUint(lo, 10, 32) // malformed sets match nothing
		b, _ := strconv.ParseUint(hi, 10, 32)
		rs = append(rs, rng{uint32(min(a, b)), uint32(max(a, b))})
	}
	return func(u uint32) bool {
		return slices.ContainsFunc(rs, func(r rng) bool { return u >= r.lo && u <= r.hi })
	}
}

func joinUIDs(uids []uint32) string {
	s := make([]string, len(uids))
	for i, u := range uids {
		s[i] = strconv.FormatUint(uint64(u), 10)
	}
	return strings.Join(s, ",")
}

var wsHeaderSearch = regexp.MustCompile(`(?i)HEADER "?Message-ID"? "?<?([^">]*)>?"?`)

// capability is the CAPABILITY list.
func (srv *writeServer) capability() string {
	return strings.TrimSpace("CAPABILITY IMAP4rev1 " + srv.caps)
}

func (srv *writeServer) uidplus() bool { return strings.Contains(srv.caps, "UIDPLUS") }

func (srv *writeServer) reply(st *wsSession, c wsCmd) string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.after != nil {
		defer srv.after(srv, c.kind) // runs before the unlock
	}
	ok := c.tag + " OK done\r\n"
	if c.kind == srv.refuse {
		return c.tag + " NO refused\r\n"
	}
	f := strings.Fields(c.rest)
	switch c.kind {
	case "LOGIN", "NOOP":
		return ok
	case "CREATE":
		srv.folders = append(srv.folders, wsMailbox(wsUnquote(f[1]), "", 99))
		return ok
	case "CAPABILITY":
		return "* " + srv.capability() + "\r\n" + ok
	case "LIST":
		delim := cmp.Or(srv.delim, "/")
		var b strings.Builder
		for _, fo := range srv.folders {
			fmt.Fprintf(&b, "* LIST (\\HasNoChildren%s) %q %q\r\n", strings.TrimRight(" "+fo.attr, " "), delim, fo.name)
		}
		return b.String() + ok
	case "STATUS":
		fo := srv.folder(wsUnquote(f[1]))
		if fo == nil {
			return c.tag + " NO [NONEXISTENT] no such mailbox\r\n"
		}
		return fmt.Sprintf("* STATUS %q (MESSAGES %d UNSEEN 0 UIDNEXT %d UIDVALIDITY %d)\r\n%s", fo.name, len(fo.msgs), fo.next+1, fo.validity, ok)
	case "SELECT", "EXAMINE":
		fo := srv.folder(wsUnquote(f[1]))
		st.selected = fo
		if fo == nil {
			return c.tag + " NO [NONEXISTENT] no such mailbox\r\n"
		}
		mode := "READ-WRITE"
		if c.kind == "EXAMINE" {
			mode = "READ-ONLY"
		}
		return fmt.Sprintf("* %d EXISTS\r\n* OK [UIDVALIDITY %d] ok\r\n%s OK [%s] done\r\n", len(fo.msgs), fo.validity, c.tag, mode)
	case "APPEND":
		fo := srv.folder(wsUnquote(f[1]))
		if fo == nil {
			return c.tag + " NO [TRYCREATE] no such mailbox\r\n"
		}
		id := ""
		if m := regexp.MustCompile(`(?i)Message-ID:\s*<([^>]*)>`).FindSubmatch(c.literal); m != nil {
			id = string(m[1])
		}
		fo.next++
		fo.msgs = append(fo.msgs, wsMsg{uid: fo.next, id: id})
		if srv.uidplus() {
			return fmt.Sprintf("%s OK [APPENDUID %d %d] done\r\n", c.tag, fo.validity, fo.next)
		}
		return ok
	}
	if st.selected == nil {
		return c.tag + " BAD no folder selected\r\n"
	}
	src := st.selected
	switch c.kind {
	case "UID FETCH":
		in := wsSet(f[2])
		var b strings.Builder
		for i, m := range src.msgs {
			if !in(m.uid) {
				continue
			}
			if strings.Contains(c.rest, "ENVELOPE") {
				id := "NIL"
				if m.id != "" {
					id = `"<` + m.id + `>"`
				}
				fmt.Fprintf(&b, "* %d FETCH (UID %d ENVELOPE (\"Mon, 26 Oct 2026 09:00:00 +0000\" \"s\" NIL NIL NIL NIL NIL NIL NIL %s))\r\n", i+1, m.uid, id)
			} else {
				fmt.Fprintf(&b, "* %d FETCH (UID %d)\r\n", i+1, m.uid)
			}
		}
		return b.String() + ok
	case "UID SEARCH":
		m := wsHeaderSearch.FindStringSubmatch(c.rest)
		var hits []string
		for _, msg := range src.msgs {
			if m != nil && msg.id == m[1] {
				hits = append(hits, strconv.FormatUint(uint64(msg.uid), 10))
			}
		}
		return strings.TrimSpace("* SEARCH "+strings.Join(hits, " ")) + "\r\n" + ok
	case "UID STORE":
		return ok
	case "UID EXPUNGE":
		in := wsSet(f[2])
		src.msgs = slices.DeleteFunc(src.msgs, func(m wsMsg) bool { return in(m.uid) })
		return ok
	case "UID COPY", "UID MOVE":
		dst := srv.folder(wsUnquote(strings.Join(f[3:], " ")))
		if dst == nil {
			return c.tag + " NO [TRYCREATE] no such mailbox\r\n"
		}
		in := wsSet(f[2])
		var from, to []uint32
		for _, m := range src.msgs {
			if in(m.uid) {
				dst.next++
				dst.msgs = append(dst.msgs, wsMsg{uid: dst.next, id: m.id})
				from, to = append(from, m.uid), append(to, dst.next)
			}
		}
		code := ""
		if srv.uidplus() && !srv.noCopyUID && len(from) > 0 {
			code = fmt.Sprintf("[COPYUID %d %s %s] ", dst.validity, joinUIDs(from), joinUIDs(to))
		}
		if c.kind == "UID COPY" {
			return c.tag + " OK " + code + "done\r\n"
		}
		src.msgs = slices.DeleteFunc(src.msgs, func(m wsMsg) bool { return in(m.uid) })
		untagged := ""
		if code != "" {
			untagged = "* OK " + code + "moved\r\n"
		}
		return untagged + ok
	default:
		return c.tag + " BAD unknown command\r\n"
	}
}

func hostRef(folder string, validity, uid uint32) ref.Ref {
	return ref.Ref{Account: "host", Folder: folder, UIDValidity: validity, UID: uid}
}

func TestMoveMapsCOPYUIDToNewRefs(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{caps: "MOVE UIDPLUS", folders: []*wsFolder{
		wsMailbox("INBOX", "", 7, 1, 2), wsMailbox("Archive", "\\Archive", 20),
	}})
	s := writeStore(t, srv, Options{})
	refs := []ref.Ref{
		hostRef("INBOX", 7, 1), hostRef("INBOX", 7, 2), hostRef("INBOX", 7, 1),
		hostRef("INBOX", 7, 9), // gone, but in the acknowledged MOVE: success, no new ref
		hostRef("INBOX", 6, 2), // stale UIDVALIDITY
		{Account: "other", Folder: "INBOX", UIDValidity: 7, UID: 1},
	}
	res := s.Move(t.Context(), refs, "Archive")
	want := []mailstore.RefResult{
		{Ref: hostRef("Archive", 20, 101)}, {Ref: hostRef("Archive", 20, 102)}, {Ref: hostRef("Archive", 20, 101)}, {},
	}
	for i, w := range want {
		if res[i] != w {
			t.Errorf("ref %d: %+v, want %+v", i, res[i], w)
		}
	}
	for i, wantErr := range map[int]error{4: mailstore.ErrStaleRef, 5: mailstore.ErrNotFound} {
		if !errors.Is(res[i].Err, wantErr) || res[i].Ref != (ref.Ref{}) {
			t.Errorf("ref %d: %+v, want %v", i, res[i], wantErr)
		}
	}
	if got := srv.sent("UID MOVE"); got != 1 {
		t.Errorf("%d UID MOVE commands, want one for the folder", got)
	}
	if i := srv.eventIndex("recv UID MOVE 1:2,9 \"Archive\""); i < 0 {
		t.Errorf("commands %q, want UID MOVE 1:2,9 \"Archive\"", srv.received())
	}
	if i := srv.eventIndex("recv SELECT INBOX"); i < 0 {
		t.Errorf("commands %q, want INBOX opened read-write", srv.received())
	}
}

func TestMoveWithoutMOVECopiesThenPipelinesStoreAndExpunge(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{
		caps:    "UIDPLUS",
		folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1, 2, 3), wsMailbox("Work", "", 30)},
		// The STORE is answered only once the UID EXPUNGE has arrived too.
		holdFrom: "UID STORE", holdCount: 2,
		// The COPY is answered slowly, so a STORE sent before its reply shows.
		slowKind: "UID COPY", slow: 100 * time.Millisecond,
	})
	s := writeStore(t, srv, Options{})
	res := s.Move(t.Context(), []ref.Ref{hostRef("INBOX", 7, 3), hostRef("INBOX", 7, 1), hostRef("INBOX", 7, 8)}, "Work")
	if res[0] != (mailstore.RefResult{Ref: hostRef("Work", 30, 102)}) || res[1] != (mailstore.RefResult{Ref: hostRef("Work", 30, 101)}) {
		t.Errorf("results %+v, want the COPYUID UIDs", res)
	}
	if res[2] != (mailstore.RefResult{}) {
		t.Errorf("gone ref: %+v, want success with no new ref: it was in the acknowledged COPY", res[2])
	}
	copyRecv, copySent := srv.eventIndex("recv UID COPY 1,3,8 \"Work\""), srv.eventIndex("sent UID COPY")
	store, expunge := srv.eventIndex("recv UID STORE 1,3 +FLAGS.SILENT (\\Deleted)"), srv.eventIndex("recv UID EXPUNGE 1,3")
	if copyRecv < 0 || store < 0 || expunge < 0 || copySent > store || store > expunge {
		t.Errorf("commands %q: want UID COPY answered before STORE and EXPUNGE of the copied UIDs only, pipelined", srv.events)
	}
	if n := srv.sent("UID FETCH"); n != 0 {
		t.Errorf("%d UID FETCH commands; COPYUID names the moved messages", n)
	}
}

func TestMoveWithoutUIDPLUSSucceedsWithNoNewRefs(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{caps: "MOVE", folders: []*wsFolder{
		wsMailbox("INBOX", "", 7, 4, 5), wsMailbox("Work", "", 30),
	}})
	s := writeStore(t, srv, Options{})
	res := s.Move(t.Context(), []ref.Ref{hostRef("INBOX", 7, 5), hostRef("INBOX", 7, 4), hostRef("INBOX", 7, 6)}, "Work")
	for i, r := range res {
		if r != (mailstore.RefResult{}) {
			t.Errorf("ref %d: %+v, want success with no new ref: without UIDPLUS no COPYUID names it", i, r)
		}
	}
	if srv.sent("UID MOVE 4:6 \"Work\"") != 1 || srv.sent("UID FETCH")+srv.sent("UID SEARCH")+srv.sent("STATUS") != 0 {
		t.Errorf("commands %q, want the one UID MOVE and no lookups", srv.received())
	}
}

func TestMoveWithNeitherMOVENorUIDPLUSIsUnsupported(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1), wsMailbox("Work", "", 30)}})
	s := writeStore(t, srv, Options{})
	res := s.Move(t.Context(), []ref.Ref{hostRef("INBOX", 7, 1)}, "Work")
	if !errors.Is(res[0].Err, mailstore.ErrUnsupported) {
		t.Errorf("result %+v, want ErrUnsupported", res[0])
	}
	if n := srv.sent("UID COPY") + srv.sent("UID STORE") + srv.sent("EXPUNGE"); n != 0 {
		t.Errorf("commands %q: nothing may change without UID EXPUNGE", srv.received())
	}
}

func TestMoveToAMissingFolderIsNotFound(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{caps: "MOVE UIDPLUS", folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1)}})
	s := writeStore(t, srv, Options{})
	res := s.Move(t.Context(), []ref.Ref{hostRef("INBOX", 7, 1)}, "Nope")
	if !errors.Is(res[0].Err, mailstore.ErrNotFound) {
		t.Errorf("result %+v, want ErrNotFound", res[0])
	}
	res = s.Move(t.Context(), []ref.Ref{hostRef("Gone", 7, 1)}, "INBOX")
	if !errors.Is(res[0].Err, mailstore.ErrNotFound) {
		t.Errorf("from a missing folder: %+v, want ErrNotFound", res[0])
	}
}

func TestMoveIsNotRetriedOnceSent(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{caps: "MOVE UIDPLUS", dropOn: "UID MOVE", folders: []*wsFolder{
		wsMailbox("INBOX", "", 7, 1), wsMailbox("Work", "", 30),
	}})
	s := writeStore(t, srv, Options{})
	res := s.Move(t.Context(), []ref.Ref{hostRef("INBOX", 7, 1)}, "Work")
	if !errors.Is(res[0].Err, mailstore.ErrUnavailable) || errors.Is(res[0].Err, errConnLost) {
		t.Errorf("result %+v, want ErrUnavailable", res[0])
	}
	if n, c := srv.sent("UID MOVE"), srv.conns.Load(); n != 1 || c != 1 {
		t.Errorf("UID MOVE sent %d times on %d connections, want once: a write is never resent", n, c)
	}
}

func TestCopyMapsCOPYUIDAndKeepsTheOriginal(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{caps: "MOVE UIDPLUS", folders: []*wsFolder{
		wsMailbox("INBOX", "", 7, 1, 2), wsMailbox("Work", "", 30),
	}})
	s := writeStore(t, srv, Options{})
	res := s.Copy(t.Context(), []ref.Ref{hostRef("INBOX", 7, 2), hostRef("INBOX", 7, 1)}, "Work")
	if res[0] != (mailstore.RefResult{Ref: hostRef("Work", 30, 102)}) || res[1] != (mailstore.RefResult{Ref: hostRef("Work", 30, 101)}) {
		t.Errorf("results %+v, want the copies' refs", res)
	}
	if srv.sent(`UID COPY 1:2 "Work"`) != 1 || srv.sent("UID STORE")+srv.sent("UID EXPUNGE")+srv.sent("UID MOVE") != 0 {
		t.Errorf("commands %q, want one UID COPY and nothing else that changes mail", srv.received())
	}
	// Copying into the folder a message is in makes a second copy.
	res = s.Copy(t.Context(), []ref.Ref{hostRef("INBOX", 7, 1)}, "INBOX")
	if res[0].Err != nil || res[0].Ref.Folder != "INBOX" || res[0].Ref.UID == 1 {
		t.Errorf("copy into its own folder: %+v, want a new ref there", res[0])
	}
}

func TestExpungePipelinesStoreAndUIDExpunge(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{
		caps: "UIDPLUS", folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1, 2, 3)},
		holdFrom: "UID STORE", holdCount: 2,
	})
	s := writeStore(t, srv, Options{})
	res := s.Expunge(t.Context(), []ref.Ref{hostRef("INBOX", 7, 3), hostRef("INBOX", 7, 1), hostRef("INBOX", 7, 9)})
	if res[0] != (mailstore.RefResult{}) || res[1] != (mailstore.RefResult{}) {
		t.Errorf("results %+v, want zero refs and no errors", res)
	}
	if !errors.Is(res[2].Err, mailstore.ErrStaleRef) {
		t.Errorf("gone ref: %+v, want ErrStaleRef", res[2])
	}
	store, expunge := srv.eventIndex("recv UID STORE 1,3,9 +FLAGS.SILENT (\\Deleted)"), srv.eventIndex("recv UID EXPUNGE 1,3,9")
	if store < 0 || expunge < 0 || store > expunge {
		t.Errorf("commands %q, want UID STORE +FLAGS.SILENT (\\Deleted) then UID EXPUNGE of the set", srv.received())
	}
	if srv.sent("EXPUNGE") != 0 {
		t.Errorf("commands %q: a bare EXPUNGE would remove other \\Deleted mail", srv.received())
	}
}

func TestExpungeWithoutUIDPLUSIsUnsupported(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{caps: "MOVE", folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1)}})
	s := writeStore(t, srv, Options{})
	res := s.Expunge(t.Context(), []ref.Ref{hostRef("INBOX", 7, 1)})
	if !errors.Is(res[0].Err, mailstore.ErrUnsupported) {
		t.Errorf("result %+v, want ErrUnsupported", res[0])
	}
	if n := srv.sent("UID STORE") + srv.sent("EXPUNGE"); n != 0 {
		t.Errorf("commands %q, want nothing changed", srv.received())
	}
}

func TestStoreFlagsPipelinesAddAndRemove(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{
		folders:  []*wsFolder{wsMailbox("INBOX", "", 7, 1, 2)},
		holdFrom: "UID STORE", holdCount: 2,
	})
	s := writeStore(t, srv, Options{})
	refs := []ref.Ref{hostRef("INBOX", 7, 2), hostRef("INBOX", 7, 1), hostRef("INBOX", 7, 5)}
	res := s.StoreFlags(t.Context(), refs, []string{"\\Flagged", "$Work"}, []string{"\\Seen"})
	for i := range 2 {
		if res[i] != (mailstore.RefResult{Ref: refs[i]}) {
			t.Errorf("ref %d: %+v, want it unchanged", i, res[i])
		}
	}
	if !errors.Is(res[2].Err, mailstore.ErrStaleRef) {
		t.Errorf("gone ref: %+v, want ErrStaleRef", res[2])
	}
	add, del := srv.eventIndex("recv UID STORE 1:2,5 +FLAGS.SILENT (\\Flagged $Work)"), srv.eventIndex("recv UID STORE 1:2,5 -FLAGS.SILENT (\\Seen)")
	if add < 0 || del < 0 {
		t.Errorf("commands %q, want +FLAGS.SILENT and -FLAGS.SILENT over the set", srv.received())
	}
}

func TestStoreFlagsRefusesInvalidFlags(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1)}})
	s := writeStore(t, srv, Options{})
	for _, bad := range []string{"", "two words", "\\Seen)", "x(", "\\", "\\Recent", "\\*", "café"} {
		res := s.StoreFlags(t.Context(), []ref.Ref{hostRef("INBOX", 7, 1)}, []string{bad}, nil)
		if !errors.Is(res[0].Err, mailstore.ErrUnsupported) {
			t.Errorf("flag %q: %+v, want ErrUnsupported", bad, res[0])
		}
	}
	if n := srv.sent("UID STORE"); n != 0 {
		t.Errorf("%d UID STORE commands, want none", n)
	}
}

func TestAppendReturnsTheAPPENDUIDRef(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{caps: "UIDPLUS", folders: []*wsFolder{wsMailbox("INBOX", "", 7), wsMailbox("Drafts", "\\Drafts", 40)}})
	s := writeStore(t, srv, Options{})
	raw := []byte("Message-ID: <draft@x>\r\nSubject: hi\r\n\r\nbody\r\n")
	got, err := s.Append(t.Context(), "Drafts", []string{"\\Draft", "\\Seen"}, time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC), raw)
	if err != nil || got != hostRef("Drafts", 40, 101) {
		t.Errorf("Append = %+v, %v; want Drafts UID 101", got, err)
	}
	if srv.sent(`APPEND "Drafts" (\Draft \Seen) "30-Oct-2026 09:00:00 +0000" {literal}`)+srv.sent(`APPEND Drafts (\Draft \Seen) "30-Oct-2026 09:00:00 +0000" {literal}`) != 1 {
		t.Errorf("commands %q, want one APPEND with flags and date", srv.received())
	}
	if _, err := s.Append(t.Context(), "Nope", nil, time.Time{}, raw); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("Append to a missing folder = %v, want ErrNotFound", err)
	}
	if _, err := s.Append(t.Context(), "Drafts", []string{"bad flag"}, time.Time{}, raw); !errors.Is(err, mailstore.ErrUnsupported) {
		t.Errorf("Append with a bad flag = %v, want ErrUnsupported", err)
	}
}

func TestAppendWithoutAPPENDUIDGivesAZeroRef(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{folders: []*wsFolder{wsMailbox("INBOX", "", 7), wsMailbox("Drafts", "", 40)}})
	s := writeStore(t, srv, Options{})
	for _, raw := range []string{"Message-Id:  <d2@x> \r\n\r\nbody\r\n", "Subject: no id\r\n\r\nbody\r\n"} {
		got, err := s.Append(t.Context(), "Drafts", nil, time.Time{}, []byte(raw))
		if err != nil || got != (ref.Ref{}) {
			t.Errorf("Append of %q = %+v, %v; want a zero ref and no error: without UIDPLUS no APPENDUID names it", raw, got, err)
		}
	}
	if srv.sent("APPEND") != 2 || srv.sent("STATUS")+srv.sent("UID SEARCH")+srv.sent("EXAMINE")+srv.sent("SELECT") != 0 {
		t.Errorf("commands %q, want the two APPENDs and no lookups", srv.received())
	}
}

func TestFindByMessageID(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{folders: []*wsFolder{wsMailbox("INBOX", "", 7, 3, 8)}})
	s := writeStore(t, srv, Options{})
	for _, id := range []string{"8@x", "<8@x>", " <8@x> "} {
		got, err := s.FindByMessageID(t.Context(), "INBOX", id)
		if err != nil || !slices.Equal(got, []ref.Ref{hostRef("INBOX", 7, 8)}) {
			t.Errorf("FindByMessageID(%q) = %+v, %v", id, got, err)
		}
	}
	if got, err := s.FindByMessageID(t.Context(), "INBOX", "none@x"); err != nil || len(got) != 0 {
		t.Errorf("unknown Message-ID = %+v, %v; want nothing", got, err)
	}
	if _, err := s.FindByMessageID(t.Context(), "Nope", "8@x"); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("missing folder = %v, want ErrNotFound", err)
	}
	if srv.eventIndex("recv SELECT") >= 0 {
		t.Errorf("commands %q: a search opens folders read-only", srv.received())
	}
}

func roleFolders() []*wsFolder {
	return []*wsFolder{
		wsMailbox("INBOX", "", 7, 1, 2), wsMailbox("Trash", "\\Trash", 50, 4),
		wsMailbox("Archive", "\\Archive", 60), wsMailbox("Saved", "", 70),
	}
}

func TestTrashMovesToTheTrashRoleAndLeavesTrashAlone(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{caps: "MOVE UIDPLUS", folders: roleFolders()})
	s := writeStore(t, srv, Options{})
	res := s.Trash(t.Context(), []ref.Ref{hostRef("INBOX", 7, 1), hostRef("Trash", 50, 4)})
	if res[0] != (mailstore.RefResult{Ref: hostRef("Trash", 50, 101)}) {
		t.Errorf("INBOX ref: %+v, want it moved to Trash", res[0])
	}
	if res[1] != (mailstore.RefResult{Ref: hostRef("Trash", 50, 4)}) {
		t.Errorf("Trash ref: %+v, want it unchanged", res[1])
	}
	if n := srv.sent("UID MOVE"); n != 1 {
		t.Errorf("commands %q, want only the INBOX ref moved", srv.received())
	}
}

func TestArchiveUsesTheConfiguredFolderOrTheRole(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{caps: "MOVE UIDPLUS", folders: roleFolders()})
	res := writeStore(t, srv, Options{}).Archive(t.Context(), []ref.Ref{hostRef("INBOX", 7, 1)})
	if res[0] != (mailstore.RefResult{Ref: hostRef("Archive", 60, 101)}) {
		t.Errorf("by role: %+v, want Archive", res[0])
	}
	res = writeStore(t, srv, Options{ArchiveFolder: "Saved"}).Archive(t.Context(), []ref.Ref{hostRef("INBOX", 7, 2)})
	if res[0] != (mailstore.RefResult{Ref: hostRef("Saved", 70, 101)}) {
		t.Errorf("configured: %+v, want Saved", res[0])
	}

	bare := startWriteServer(t, &writeServer{caps: "MOVE UIDPLUS", folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1)}})
	res = writeStore(t, bare, Options{}).Archive(t.Context(), []ref.Ref{hostRef("INBOX", 7, 1)})
	if !errors.Is(res[0].Err, mailstore.ErrNotFound) {
		t.Errorf("no archive folder: %+v, want ErrNotFound", res[0])
	}
	if bare.sent("UID MOVE") != 0 {
		t.Errorf("commands %q, want nothing moved", bare.received())
	}
}

func TestWriteBatchesFanOutWithinThePool(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{
		caps: "MOVE UIDPLUS", slowKind: "UID MOVE", slow: 50 * time.Millisecond,
		folders: []*wsFolder{
			wsMailbox("INBOX", "", 7, 1), wsMailbox("A", "", 8, 1), wsMailbox("B", "", 9, 1), wsMailbox("Dest", "", 10),
		},
	})
	s := writeStore(t, srv, Options{PoolSize: 2})
	met := meetAtDial(s, 2)
	refs := []ref.Ref{hostRef("INBOX", 7, 1), hostRef("A", 8, 1), hostRef("B", 9, 1)}
	res := s.Move(t.Context(), refs, "Dest")
	for i, r := range res {
		if r.Err != nil || r.Ref.Folder != "Dest" {
			t.Errorf("ref %d: %+v", i, r)
		}
	}
	if !met.Load() {
		t.Error("the folders never had two connections being opened at once; want them run concurrently")
	}
	if n, p := srv.conns.Load(), srv.peak.Load(); n > 2 || p > 2 {
		t.Errorf("%d connections, %d at once; want at most the pool's 2", n, p)
	}
	if n := srv.sent("UID MOVE"); n != 3 {
		t.Errorf("%d UID MOVE commands, want one per folder", n)
	}
}

func TestStoreLabelsIsUnsupportedOffGmail(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1)}})
	res := writeStore(t, srv, Options{}).StoreLabels(t.Context(), []ref.Ref{hostRef("INBOX", 7, 1)}, []string{"x"}, nil)
	if !errors.Is(res[0].Err, mailstore.ErrUnsupported) {
		t.Errorf("StoreLabels = %+v, want ErrUnsupported", res[0])
	}
}

// memStore is a Store on imaptest's in-memory server with one connection,
// so every call shares it.
func memStore(t *testing.T, u imaptest.User, opts Options) *Store {
	t.Helper()
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, u)
	opts.RootCAs, opts.Backoff = ca.Pool, noBackoff
	if opts.PoolSize == 0 {
		opts.PoolSize = 1
	}
	s := New(testAccount("host", ep, false), passwordCreds(u.Username, u.Password), opts)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

const memMessage = "From: a@example.com\r\nSubject: hi\r\nMessage-ID: <m1@x>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhello\r\n"

func memRef(t *testing.T, s *Store) ref.Ref {
	t.Helper()
	res, err := s.Search(t.Context(), mailstore.SearchRequest{In: "INBOX"})
	if err != nil || len(res) != 1 || len(res[0].UIDs) != 1 {
		t.Fatalf("Search = %+v, %v", res, err)
	}
	return ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: res[0].Source.UIDValidity, UID: res[0].UIDs[0]}
}

func TestWriteReselectsAFolderOpenReadOnly(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var selects []string
	s := memStore(t, imaptest.User{
		Username: "mode@example.test", Password: "pw", Mail: map[string][]string{"INBOX": {memMessage}},
		OnSelect: func(name string, readOnly bool) {
			mu.Lock()
			defer mu.Unlock()
			selects = append(selects, fmt.Sprintf("%s read-only=%t", name, readOnly))
		},
	}, Options{})
	r := memRef(t, s)
	if res := s.FetchMessages(t.Context(), []ref.Ref{r}); res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	if res := s.StoreFlags(t.Context(), []ref.Ref{r}, []string{"\\Flagged"}, nil); res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	if res := s.FetchMessages(t.Context(), []ref.Ref{r}); res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	mu.Lock()
	got := slices.Clone(selects)
	mu.Unlock()
	want := []string{"INBOX read-only=true", "INBOX read-only=false"}
	if !slices.Equal(got, want) {
		t.Errorf("selects = %q, want %q: EXAMINE for the read, SELECT for the write, and the read after reusing it", got, want)
	}
}

func TestReadAfterWriteStillPeeks(t *testing.T) {
	t.Parallel()
	s := memStore(t, imaptest.User{Username: "peek@example.test", Password: "pw", Mail: map[string][]string{"INBOX": {memMessage}}}, Options{})
	r := memRef(t, s)
	if res := s.StoreFlags(t.Context(), []ref.Ref{r}, []string{"\\Flagged"}, nil); res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	// The one connection now has INBOX open read-write.
	if res := s.FetchMessages(t.Context(), []ref.Ref{r}); res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	_, rc, err := s.StreamPart(t.Context(), r, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	sums, err := s.FetchSummaries(t.Context(), mailstore.Source{Folder: "INBOX", UIDValidity: r.UIDValidity}, []uint32{r.UID}, mailstore.SummaryOptions{Snippets: true})
	if err != nil || len(sums) != 1 {
		t.Fatalf("FetchSummaries = %+v, %v", sums, err)
	}
	if !slices.Contains(sums[0].Flags, string(imap.FlagFlagged)) || slices.Contains(sums[0].Flags, string(imap.FlagSeen)) {
		t.Errorf("flags = %q, want \\Flagged and not \\Seen", sums[0].Flags)
	}
}

func TestStoreCallsOnConnLimitAfterShrinking(t *testing.T) {
	t.Parallel()
	limit := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "Too many simultaneous connections"}
	var mu sync.Mutex
	var got []int
	s := memStore(t, imaptest.User{Username: "limit@example.test", Password: "pw", LoginError: limit}, Options{
		PoolSize: 3,
		OnConnLimit: func(n int) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, n)
		},
	})
	if _, err := s.ListFolders(t.Context()); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Fatalf("ListFolders = %v, want ErrUnavailable", err)
	}
	if res := s.Move(t.Context(), []ref.Ref{hostRef("INBOX", 1, 1)}, "Work"); !errors.Is(res[0].Err, mailstore.ErrUnavailable) {
		t.Fatalf("Move = %+v, want ErrUnavailable", res[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, []int{2, 1}) || s.PoolSize() != 1 {
		t.Errorf("OnConnLimit heard %v, PoolSize %d; want [2 1] and 1", got, s.PoolSize())
	}
}

func TestProbeDialLogsInOutsideThePool(t *testing.T) {
	t.Parallel()
	var logins atomic.Int32
	s := memStore(t, imaptest.User{Username: "probe@example.test", Password: "pw", Logins: &logins}, Options{})
	held, err := s.borrow(t.Context(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release(held)
	// The pool's one connection is out, so a pooled dial would wait.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := s.ProbeDial(ctx)
	if err != nil {
		t.Fatalf("ProbeDial: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if n := logins.Load(); n != 2 || s.PoolSize() != 1 {
		t.Errorf("logins %d, PoolSize %d; want 2 and 1", n, s.PoolSize())
	}
}

func TestProbeDialLeavesThePoolAloneOnALimit(t *testing.T) {
	t.Parallel()
	limit := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "Too many simultaneous connections"}
	called := false
	s := memStore(t, imaptest.User{Username: "plimit@example.test", Password: "pw", LoginError: limit}, Options{
		PoolSize: 3, OnConnLimit: func(int) { called = true },
	})
	c, err := s.ProbeDial(t.Context())
	if err == nil {
		_ = c.Close()
		t.Fatal("ProbeDial succeeded against a refusing server")
	}
	if !errors.Is(err, mailstore.ErrUnavailable) || called || s.PoolSize() != 3 {
		t.Errorf("err %v, OnConnLimit called %t, PoolSize %d; want ErrUnavailable, false and 3", err, called, s.PoolSize())
	}
}

// wsMessages makes a folder holding msgs, for tests that choose the Message-IDs.
func wsMessages(name string, validity uint32, msgs ...wsMsg) *wsFolder {
	return &wsFolder{name: name, validity: validity, next: 100, msgs: msgs}
}

// uidsIn returns the UIDs folder holds, in order.
func (srv *writeServer) uidsIn(folder string) []uint32 {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	var out []uint32
	for _, m := range srv.folder(folder).msgs {
		out = append(out, m.uid)
	}
	return out
}

// dropMsgs removes from folder the messages drop picks, for an after hook
// standing in for another client. srv.mu is held.
func (srv *writeServer) dropMsgs(folder string, drop func(wsMsg) bool) {
	f := srv.folder(folder)
	f.msgs = slices.DeleteFunc(f.msgs, drop)
}

func TestMoveByCopyWithoutCOPYUIDKeepsEveryOriginal(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{
		caps: "UIDPLUS", noCopyUID: true,
		folders: []*wsFolder{
			wsMessages("INBOX", 7, wsMsg{1, "same@x"}, wsMsg{2, "solo@x"}),
			wsMessages("Work", 30, wsMsg{50, "same@x"}),
		},
		// Another client removes the new copy of same@x before anything can
		// look for it, which leaves only Work's older message with that ID.
		after: func(srv *writeServer, kind string) {
			if kind == "UID COPY" {
				srv.dropMsgs("Work", func(m wsMsg) bool { return m.uid == 101 })
			}
		},
	})
	s := writeStore(t, srv, Options{})
	res := s.Move(t.Context(), []ref.Ref{hostRef("INBOX", 7, 1), hostRef("INBOX", 7, 2)}, "Work")
	for i, r := range res {
		if !errors.Is(r.Err, mailstore.ErrUnavailable) || r.Ref != (ref.Ref{}) ||
			!strings.Contains(r.Err.Error(), "copied, but the original was kept: the server did not confirm the copy") {
			t.Errorf("ref %d: %+v, want ErrUnavailable: without COPYUID the original stays", i, r)
		}
	}
	if got := srv.uidsIn("INBOX"); !slices.Equal(got, []uint32{1, 2}) {
		t.Errorf("INBOX holds %v, want every original kept", got)
	}
	if n := srv.sent("UID STORE") + srv.sent("UID EXPUNGE") + srv.sent("EXPUNGE"); n != 0 {
		t.Errorf("commands %q: nothing may be deleted without COPYUID naming the copy", srv.received())
	}
	if n := srv.sent("UID SEARCH") + srv.sent("UID FETCH"); n != 0 {
		t.Errorf("commands %q: a search cannot confirm a copy, so none is made", srv.received())
	}
	if n := srv.sent("UID COPY"); n != 1 {
		t.Errorf("%d UID COPY commands, want one", n)
	}
}

func TestTransferWithoutCOPYUIDSucceedsWithNoNewRef(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		caps      string
		noCopyUID bool
		copy      bool
	}{
		{name: "move, UIDPLUS without COPYUID", caps: "MOVE UIDPLUS", noCopyUID: true},
		{name: "move without UIDPLUS", caps: "MOVE"},
		{name: "copy, UIDPLUS without COPYUID", caps: "UIDPLUS", noCopyUID: true, copy: true},
		{name: "copy without UIDPLUS", copy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := startWriteServer(t, &writeServer{
				caps: tc.caps, noCopyUID: tc.noCopyUID,
				// Two source folders hold different messages with one
				// Message-ID, and Work already holds an older one with it.
				folders: []*wsFolder{
					wsMessages("INBOX", 7, wsMsg{1, "same@x"}, wsMsg{2, "solo@x"}),
					wsMessages("Sent", 8, wsMsg{5, "same@x"}),
					wsMessages("Work", 30, wsMsg{50, "same@x"}),
				},
				// Once a transfer is answered, another client expunges every
				// original, in both folders.
				after: func(srv *writeServer, kind string) {
					if kind == "UID COPY" || kind == "UID MOVE" {
						srv.dropMsgs("INBOX", func(wsMsg) bool { return true })
						srv.dropMsgs("Sent", func(wsMsg) bool { return true })
					}
				},
			})
			s := writeStore(t, srv, Options{})
			refs := []ref.Ref{hostRef("INBOX", 7, 1), hostRef("INBOX", 7, 2), hostRef("Sent", 8, 5), hostRef("INBOX", 7, 9)}
			transfer, verb := s.Move, "UID MOVE"
			if tc.copy {
				transfer, verb = s.Copy, "UID COPY"
			}
			res := transfer(t.Context(), refs, "Work")
			for i, r := range res {
				if r != (mailstore.RefResult{}) {
					t.Errorf("ref %d: %+v, want success with no new ref: the server acknowledged the %s", i, r, verb)
				}
			}
			if n := srv.sent(verb); n != 2 {
				t.Errorf("commands %q, want one %s per source folder", srv.received(), verb)
			}
			if n := srv.sent("UID FETCH") + srv.sent("UID SEARCH") + srv.sent("STATUS") + srv.sent("UID STORE") + srv.sent("UID EXPUNGE"); n != 0 {
				t.Errorf("commands %q, want nothing but the transfer: new refs come only from COPYUID", srv.received())
			}
		})
	}
}

// When the COPY worked but removing the originals did not, the messages are
// in both folders. That is not a move, so those refs fail, naming the kept
// original, rather than report a success that leaves a duplicate behind.
func TestMoveByCopyReportsKeptOriginalsWhenTheExpungeFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		refuse, dropOn string
	}{
		{name: "expunge refused", refuse: "UID EXPUNGE"},
		{name: "connection lost", dropOn: "UID STORE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := startWriteServer(t, &writeServer{
				caps: "UIDPLUS", refuse: tc.refuse, dropOn: tc.dropOn,
				folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1, 2), wsMailbox("Work", "", 30)},
			})
			s := writeStore(t, srv, Options{})
			res := s.Move(t.Context(), []ref.Ref{hostRef("INBOX", 7, 2), hostRef("INBOX", 7, 1), hostRef("INBOX", 7, 9)}, "Work")
			for i := range 2 {
				if !errors.Is(res[i].Err, mailstore.ErrUnavailable) || !strings.Contains(res[i].Err.Error(), "original") {
					t.Errorf("ref %d: %+v, want ErrUnavailable naming the kept original", i, res[i])
				}
			}
			if res[2] != (mailstore.RefResult{}) {
				t.Errorf("ref 2: %+v, want a success with no new ref: COPYUID left it out, so it was not there", res[2])
			}
			if n, c := srv.sent("UID COPY"), srv.conns.Load(); n != 1 || c != 1 {
				t.Errorf("UID COPY sent %d times on %d connections, want once: nothing is retried", n, c)
			}
		})
	}
}

func TestPairsAtTheLargestUID(t *testing.T) {
	t.Parallel()
	const top = imap.UID(4294967295)
	cases := []struct {
		name     string
		src, dst imap.UIDSet
		want     map[uint32]uint32
	}{
		{"singleton", imap.UIDSet{{Start: 5, Stop: 5}}, imap.UIDSet{{Start: top, Stop: top}}, map[uint32]uint32{5: 4294967295}},
		{"range ending there", imap.UIDSet{{Start: 5, Stop: 7}}, imap.UIDSet{{Start: top - 2, Stop: top}}, map[uint32]uint32{5: 4294967293, 6: 4294967294, 7: 4294967295}},
		{"source there", imap.UIDSet{{Start: top, Stop: top}}, imap.UIDSet{{Start: 9, Stop: 9}}, map[uint32]uint32{4294967295: 9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			done := make(chan struct{})
			var got map[uint32]uint32
			var ok bool
			go func() {
				defer close(done)
				got, ok = copyUID{validity: 1, src: tc.src, dst: tc.dst}.pairs(len(tc.want))
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("pairs did not return")
			}
			if !ok || !maps.Equal(got, tc.want) {
				t.Errorf("pairs = %v, %t; want %v", got, ok, tc.want)
			}
		})
	}
}

// meetAtDial makes each of s's dials wait, up to a second, for n dials to
// be under way at once, and reports whether they ever were. Batches that
// ran their folders one after another would never get there.
func meetAtDial(s *Store, n int32) *atomic.Bool {
	var waiting atomic.Int32
	var met atomic.Bool
	inner := s.pool.dial
	s.pool.dial = func(ctx context.Context) (*conn, error) {
		if waiting.Add(1) >= n {
			met.Store(true)
		}
		for deadline := time.Now().Add(time.Second); !met.Load() && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		waiting.Add(-1)
		return inner(ctx)
	}
	return &met
}

// TestReadAfterAWriteSyncsAReusedFolder: a connection that had the
// destination open before another connection moved mail into it sends NOOP
// before it reads, so the server tells it of the new message. Dovecot
// answers a UID FETCH or SEARCH only for messages the session has been told
// of, so without it a ref a move just gave reads as gone. Reads with no
// write since pay nothing.
func TestReadAfterAWriteSyncsAReusedFolder(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{
		caps:    "MOVE UIDPLUS",
		folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1), wsMailbox("Archive", "\\Archive", 20, 5)},
		// A slow search holds one connection in Archive while a write opens
		// INBOX read-write on the other.
		slowKind: "UID SEARCH", slow: 300 * time.Millisecond,
	})
	s := writeStore(t, srv, Options{PoolSize: 2})
	inbox := hostRef("INBOX", 7, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := s.FindByMessageID(t.Context(), "Archive", "5@x"); err != nil {
			t.Error(err)
		}
	})
	for srv.sent("UID SEARCH") == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := one(t, s.StoreFlags(t.Context(), []ref.Ref{inbox}, []string{`\Seen`}, nil)); got.Err != nil {
		t.Fatal(got.Err)
	}
	wg.Wait()
	if n := srv.sent("NOOP"); n != 0 || srv.conns.Load() != 2 {
		t.Fatalf("%d NOOPs on %d connections before a read follows a write, want none on 2", n, srv.conns.Load())
	}
	got := one(t, s.Move(t.Context(), []ref.Ref{inbox}, "Archive"))
	if got != (mailstore.RefResult{Ref: hostRef("Archive", 20, 101)}) {
		t.Fatalf("Move = %+v", got)
	}
	found, err := s.FindByMessageID(t.Context(), "Archive", "1@x")
	if err != nil || len(found) != 1 || found[0] != got.Ref {
		t.Errorf("FindByMessageID after the move = %v, %v; want %v", found, err, got.Ref)
	}
	cmds := srv.received()
	after := cmds[slices.IndexFunc(cmds, func(c string) bool { return strings.HasPrefix(c, "UID MOVE") })+1:]
	if !slices.Equal(after, []string{"NOOP", `UID SEARCH HEADER "Message-ID" "<1@x>"`}) {
		t.Errorf("commands after the move %q, want NOOP on the connection with Archive open, then the search", after)
	}
}
