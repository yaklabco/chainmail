package imapstore

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/fakegmail"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// lateArrival is a reply to the fixture's invoice, with an attachment, that
// another mail client adds to INBOX.
const lateArrival = "From: Alex Doe <alex@example.com>\r\nTo: Me <me@gmail.example>\r\nSubject: Arrival\r\n" +
	"Date: Wed, 28 Oct 2026 09:00:00 +0000\r\nMessage-ID: <late@example.com>\r\nIn-Reply-To: <inv@example.com>\r\n" +
	"References: <inv@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"b\"\r\n\r\n" +
	"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nOne more.\r\n" +
	"--b\r\nContent-Type: text/plain; name=\"note.txt\"\r\nContent-Disposition: attachment; filename=\"note.txt\"\r\n\r\n" +
	"late note\r\n--b--\r\n"

// arrivals searches s for lateArrival in INBOX and returns how many messages
// match. On Google accounts the search runs in All Mail.
func arrivals(t *testing.T, s *Store) (int, error) {
	t.Helper()
	tree, in := searchTree(t, "in:INBOX subject:Arrival")
	res, err := s.Search(t.Context(), mailstore.SearchRequest{Query: tree, In: in})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, src := range res {
		if src.Err != nil {
			return 0, src.Err
		}
		n += len(src.UIDs)
	}
	return n, nil
}

// TestReusedFolderSeesMailFromElsewhere: a pooled connection that keeps a
// folder open learns of a message another client added there once the
// freshness window has passed, so search finds it and every call on its
// ref works. The fake, like the Google Workspace account of the live run,
// tells a session of a new arrival only at a NOOP, so without one the
// message reads as gone. Under LateExists, as Gmail behaves for a message
// moved in, it tells of it only after the command after the NOOP, and a
// read that names the message opens the folder again and reads once more.
func TestReusedFolderSeesMailFromElsewhere(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// before runs just before the other client adds the message, so the
		// connection has the folder run then needs open.
		before func(t *testing.T, s *Store)
		run    func(t *testing.T, s *Store, r ref.Ref, srv *fakegmail.Server) error
		// rereads marks a read that names the message, so it works under
		// LateExists too.
		rereads bool
	}{
		{"search", func(t *testing.T, s *Store) {
			if n, err := arrivals(t, s); n != 0 || err != nil {
				t.Fatalf("search before the arrival = %d, %v", n, err)
			}
		}, func(t *testing.T, s *Store, _ ref.Ref, _ *fakegmail.Server) error {
			n, err := arrivals(t, s)
			if err == nil && n != 1 {
				t.Errorf("search found %d messages, want the arrival", n)
			}
			return err
		}, false},
		{"get", nil, func(t *testing.T, s *Store, r ref.Ref, _ *fakegmail.Server) error {
			return s.FetchMessages(t.Context(), []ref.Ref{r})[0].Err
		}, true},
		{"attachment", nil, func(t *testing.T, s *Store, r ref.Ref, _ *fakegmail.Server) error {
			_, rc, err := s.StreamPart(t.Context(), r, "2")
			if err != nil {
				return err
			}
			data, err := io.ReadAll(rc)
			if cerr := rc.Close(); err == nil {
				err = cerr
			}
			if err == nil && string(data) != "late note" {
				t.Errorf("attachment = %q, want %q", data, "late note")
			}
			return err
		}, true},
		{"thread", nil, func(t *testing.T, s *Store, r ref.Ref, _ *fakegmail.Server) error {
			refs, err := s.Thread(t.Context(), r)
			if err == nil && len(refs) != 3 {
				t.Errorf("thread = %v, want the invoice, the reply and the arrival", refs)
			}
			return err
		}, true},
		{"flag", nil, func(t *testing.T, s *Store, r ref.Ref, srv *fakegmail.Server) error {
			if err := one(t, s.StoreFlags(t.Context(), []ref.Ref{r}, []string{`\Flagged`}, nil)).Err; err != nil {
				return err
			}
			if !slices.Contains(srv.Flags(3), imap.FlagFlagged) {
				t.Errorf("arrival flags = %v, want \\Flagged", srv.Flags(3))
			}
			return nil
		}, false},
	}
	modes := []struct {
		name string
		opts fakegmail.Options
	}{
		{"late arrivals", fakegmail.Options{LateArrivals: true}},
		{"late exists", fakegmail.Options{LateExists: true}},
	}
	for _, tc := range cases {
		for _, mode := range modes {
			if mode.opts.LateExists && !tc.rereads {
				continue
			}
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				t.Parallel()
				reusedFolderSeesMailFromElsewhere(t, mode.opts, tc.before, tc.run)
			})
		}
	}
}

// reusedFolderSeesMailFromElsewhere runs one case of
// TestReusedFolderSeesMailFromElsewhere on a fake with opts.
func reusedFolderSeesMailFromElsewhere(t *testing.T, opts fakegmail.Options, before func(*testing.T, *Store),
	run func(*testing.T, *Store, ref.Ref, *fakegmail.Server) error,
) {
	t.Helper()
	ca := tlstest.New(t)
	srv := fakegmail.StartWith(t, ca, opts, "gmail@example.test", "pw",
		fakegmail.Message{Raw: invoiceRaw([]byte("pdf")), Date: gmailDay(26), Labels: []string{`\Inbox`}},
		fakegmail.Message{Raw: gmailReply, Date: gmailDay(27), Labels: []string{`\Sent`}, ThreadOf: 1},
		fakegmail.Message{Raw: gmailTrashed, Date: gmailDay(20), Trash: true},
	)
	clock := &testClock{t: time.Unix(0, 0)}
	acct := testAccount("gmail", srv.Endpoint, true)
	creds := passwordCreds("gmail@example.test", "pw")
	// One connection, so every call reuses the one that has the folder open.
	s := New(acct, creds, Options{RootCAs: ca.Pool, Backoff: noBackoff, PoolSize: 1, Now: clock.now})
	t.Cleanup(func() { _ = s.Close() })
	other := New(acct, creds, Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = other.Close() })

	// Leave INBOX open read-write, and looked at since the write.
	found, err := s.FindByMessageID(t.Context(), "INBOX", "inv@example.com")
	if err != nil || len(found) != 1 {
		t.Fatalf("FindByMessageID = %v, %v", found, err)
	}
	invoice := found[0]
	if err := one(t, s.StoreFlags(t.Context(), []ref.Ref{invoice}, []string{`\Seen`}, nil)).Err; err != nil {
		t.Fatal(err)
	}
	if err := s.FetchMessages(t.Context(), []ref.Ref{invoice})[0].Err; err != nil {
		t.Fatal(err)
	}
	if before != nil {
		before(t, s)
	}

	r, err := other.Append(t.Context(), "INBOX", nil, gmailDay(28), []byte(lateArrival))
	if err != nil || r.UID == 0 {
		t.Fatalf("Append = %v, %v", r, err)
	}
	clock.advance(freshFor + 100*time.Millisecond)
	if err := run(t, s, r, srv); err != nil {
		t.Errorf("on a message another client added: %v", err)
	}
}

// TestReusedFolderNoopsOncePerWindow: a connection reusing its open folder
// sends NOOP only once the freshness window since its last SELECT, EXAMINE
// or NOOP has passed, so a run of calls costs one NOOP a window. Other
// commands do not count: a server may answer them from what it last told
// the session.
func TestReusedFolderNoopsOncePerWindow(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1)}})
	clock := &testClock{t: time.Unix(0, 0)}
	s := writeStore(t, srv, Options{PoolSize: 1, Now: clock.now})
	steps := []struct {
		advance time.Duration
		noops   int
	}{
		{0, 0},                             // EXAMINE
		{freshFor / 2, 0},                  // inside the window from the EXAMINE
		{freshFor, 1},                      // past it
		{freshFor / 2, 1},                  // inside the window from the NOOP
		{freshFor/2 + time.Millisecond, 2}, // past it, though a search came between
		{time.Millisecond, 2},              // inside the window again
	}
	for i, st := range steps {
		clock.advance(st.advance)
		if _, err := s.FindByMessageID(t.Context(), "INBOX", "1@x"); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if n := srv.sent("NOOP"); n != st.noops {
			t.Fatalf("step %d: %d NOOPs, want %d; commands %q", i, n, st.noops, srv.received())
		}
	}
	if n := srv.sent("EXAMINE"); n != 1 {
		t.Errorf("%d EXAMINEs, want 1: the connection should keep INBOX open", n)
	}
}

// TestGetAfterTrashOnAnotherConnection: a connection that had Trash open
// before another connection moved a message there reads the message at once
// through the ref Trash gave. Gmail, like the fake under LateExists, tells a
// session that kept a folder open of a message another session moved there
// only after the command that follows its NOOP, so a NOOP before the read
// is not enough. A SELECT or EXAMINE shows the message at once.
func TestGetAfterTrashOnAnotherConnection(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	srv := fakegmail.StartWith(t, ca, fakegmail.Options{LateExists: true}, "gw@example.test", "pw",
		fakegmail.Message{Raw: gwRaw("inbox"), Date: gmailDay(1), Labels: []string{`\Inbox`}},
		fakegmail.Message{Raw: gwRaw("trashed"), Date: gmailDay(4), Trash: true},
	)
	clock := &testClock{t: time.Unix(0, 0)}
	s := New(testAccount("gmail", srv.Endpoint, true), passwordCreds("gw@example.test", "pw"),
		Options{RootCAs: ca.Pool, Backoff: noBackoff, PoolSize: 2, Now: clock.now})
	t.Cleanup(func() { _ = s.Close() })
	w := gmailWrites{store: s, srv: srv, ca: ca}

	// A stream holds one connection in Trash while the other trashes the message.
	_, rc, err := s.StreamPart(t.Context(), w.ref(t, gmailTrash, 301), "")
	if err != nil {
		t.Fatal(err)
	}
	res := one(t, s.Trash(t.Context(), []ref.Ref{w.ref(t, allMail, 1)}))
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if res.Err != nil || res.Ref.Folder != gmailTrash {
		t.Fatalf("Trash = %+v, want a ref in Trash", res)
	}
	got := s.FetchMessages(t.Context(), []ref.Ref{res.Ref})[0]
	if got.Err != nil || got.Message.Summary.Subject != "inbox" {
		t.Errorf("get of the trashed message = %+v, want the message", got)
	}
}

// TestReopenAfterAWriteSeesANewUIDValidity: the SELECT that reopens a
// folder after a write takes the UIDVALIDITY it answers, so a ref issued
// before the folder was rebuilt reads as stale rather than naming whatever
// now has its UID.
func TestReopenAfterAWriteSeesANewUIDValidity(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1)}})
	clock := &testClock{t: time.Unix(0, 0)}
	s := writeStore(t, srv, Options{PoolSize: 1, Now: clock.now})
	r := hostRef("INBOX", 7, 1)
	if got := one(t, s.StoreFlags(t.Context(), []ref.Ref{r}, []string{`\Seen`}, nil)); got.Err != nil {
		t.Fatal(got.Err)
	}
	srv.mu.Lock()
	srv.folder("INBOX").validity = 8
	srv.mu.Unlock()
	if got := s.FetchMessages(t.Context(), []ref.Ref{r})[0]; !errors.Is(got.Err, mailstore.ErrStaleRef) {
		t.Errorf("get under the old UIDVALIDITY = %+v, want %v", got, mailstore.ErrStaleRef)
	}
	if n := srv.sent("SELECT"); n != 2 {
		t.Errorf("%d SELECTs, want 2: one for the write and one for the read after it; commands %q", n, srv.received())
	}
}

// opens lists the EXAMINE and UID FETCH commands srv received, each cut to
// its folder or UID set.
func opens(srv *writeServer) []string {
	var out []string
	for _, c := range srv.received() {
		f := strings.Fields(c)
		switch {
		case f[0] == "EXAMINE":
			out = append(out, f[0]+" "+f[1])
		case len(f) > 2 && f[0]+" "+f[1] == "UID FETCH":
			out = append(out, "UID FETCH "+f[2])
		}
	}
	return out
}

// TestReadOfAGoneMessageReopensOnce: a read that names a message the server
// leaves out of its answer opens a reused folder again and asks once more
// before it reports the ref stale, since the server may not yet have told
// the connection of the message. On a folder the read opened itself the
// answer is final, and costs no second EXAMINE.
func TestReadOfAGoneMessageReopensOnce(t *testing.T) {
	t.Parallel()
	reads := []struct {
		name string
		read func(t *testing.T, s *Store, r ref.Ref) error
	}{
		{"get", func(t *testing.T, s *Store, r ref.Ref) error {
			return s.FetchMessages(t.Context(), []ref.Ref{r})[0].Err
		}},
		{"attachment", func(t *testing.T, s *Store, r ref.Ref) error {
			_, rc, err := s.StreamPart(t.Context(), r, "1")
			if err == nil {
				_ = rc.Close()
			}
			return err
		}},
		{"thread", func(t *testing.T, s *Store, r ref.Ref) error {
			_, err := s.Thread(t.Context(), r)
			return err
		}},
	}
	for _, rd := range reads {
		for _, reused := range []bool{true, false} {
			name, want := rd.name+"/fresh", []string{"EXAMINE INBOX", "UID FETCH 9"}
			if reused {
				name, want = rd.name+"/reused", []string{"EXAMINE INBOX", "UID FETCH 9", "EXAMINE INBOX", "UID FETCH 9"}
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				srv := startWriteServer(t, &writeServer{folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1)}})
				clock := &testClock{t: time.Unix(0, 0)}
				s := writeStore(t, srv, Options{PoolSize: 1, Now: clock.now})
				if reused {
					if _, err := s.FindByMessageID(t.Context(), "INBOX", "1@x"); err != nil {
						t.Fatal(err)
					}
				}
				if err := rd.read(t, s, hostRef("INBOX", 7, 9)); !errors.Is(err, mailstore.ErrStaleRef) {
					t.Errorf("%s of a gone message = %v, want %v", rd.name, err, mailstore.ErrStaleRef)
				}
				if got := opens(srv); !slices.Equal(got, want) {
					t.Errorf("commands %q, want %q; all %q", got, want, srv.received())
				}
			})
		}
	}
}

// TestGetRereadsOnlyTheLeftOutMessages: a batch read on a reused folder
// whose answer leaves out a message the server had not yet told the
// connection of opens the folder again, asks for that message alone, and
// returns them all.
func TestGetRereadsOnlyTheLeftOutMessages(t *testing.T) {
	t.Parallel()
	armed := false
	srv := startWriteServer(t, &writeServer{
		folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1)},
		// Message 2 shows just after the first UID FETCH of the batch, as
		// if the server told the connection of it late.
		after: func(srv *writeServer, kind string) {
			if armed && kind == "UID FETCH" {
				armed = false
				f := srv.folder("INBOX")
				f.msgs = append(f.msgs, wsMsg{uid: 2, id: "2@x"})
			}
		},
	})
	clock := &testClock{t: time.Unix(0, 0)}
	s := writeStore(t, srv, Options{PoolSize: 1, Now: clock.now})
	if _, err := s.FindByMessageID(t.Context(), "INBOX", "1@x"); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	armed = true
	srv.mu.Unlock()
	refs := []ref.Ref{hostRef("INBOX", 7, 1), hostRef("INBOX", 7, 2)}
	for i, got := range s.FetchMessages(t.Context(), refs) {
		if got.Err != nil || got.Message.Summary.Ref != refs[i] {
			t.Errorf("ref %d: %+v, want the message", i, got)
		}
	}
	want := []string{"EXAMINE INBOX", "UID FETCH 1:2", "EXAMINE INBOX", "UID FETCH 2"}
	if got := opens(srv); !slices.Equal(got, want) {
		t.Errorf("commands %q, want %q; all %q", got, want, srv.received())
	}
}

// gmailHostRef is hostRef with an X-GM-MSGID, as a Google account issues it.
func gmailHostRef(folder string, validity, uid uint32, msgID uint64) ref.Ref {
	r := hostRef(folder, validity, uid)
	r.GmailMsgID = msgID
	return r
}

// readsSince lists the EXAMINE, UID SEARCH and UID FETCH commands srv
// received after the first from, each cut to its folder or UID set, and
// UID SEARCH to its name.
func readsSince(srv *writeServer, from int) []string {
	var out []string
	for _, c := range srv.received()[from:] {
		f := strings.Fields(c)
		switch {
		case f[0] == "EXAMINE":
			out = append(out, f[0]+" "+f[1])
		case len(f) > 2 && f[0]+" "+f[1] == "UID FETCH":
			out = append(out, "UID FETCH "+f[2])
		case len(f) > 1 && f[0]+" "+f[1] == "UID SEARCH":
			out = append(out, "UID SEARCH")
		}
	}
	return out
}

// TestGetAfterUIDValidityChangesMidBatch: when a batch read's answer on a
// reused folder leaves a message out, and the folder then opens again under
// another UIDVALIDITY, the answers given under the old one are dropped and
// every ref is resolved afresh: a standard IMAP ref reads as stale, and a
// Google ref is found by X-GM-MSGID under its new UID. A ref named twice
// gets the same result both times.
func TestGetAfterUIDValidityChangesMidBatch(t *testing.T) {
	t.Parallel()
	for _, gmail := range []bool{false, true} {
		name := "standard"
		if gmail {
			name = "gmail"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			armed := false
			srv := startWriteServer(t, &writeServer{
				folders: []*wsFolder{wsMailbox("INBOX", "", 7, 1)},
				// Just after the batch's first UID FETCH, message 2
				// arrives and the folder is rebuilt under UIDVALIDITY 8,
				// with every UID moved up by 10.
				after: func(srv *writeServer, kind string) {
					if armed && kind == "UID FETCH" {
						armed = false
						f := srv.folder("INBOX")
						f.msgs = append(f.msgs, wsMsg{uid: 2, id: "2@x"})
						for i := range f.msgs {
							f.msgs[i].uid += 10
						}
						f.validity = 8
					}
				},
			})
			clock := &testClock{t: time.Unix(0, 0)}
			s := writeStoreFor(t, srv, Options{PoolSize: 1, Now: clock.now}, gmail)
			mk := func(uid uint32) ref.Ref {
				if gmail {
					return gmailHostRef("INBOX", 7, uid, uint64(uid))
				}
				return hostRef("INBOX", 7, uid)
			}
			if got := s.FetchMessages(t.Context(), []ref.Ref{mk(1)})[0]; got.Err != nil {
				t.Fatal(got.Err)
			}
			srv.mu.Lock()
			armed = true
			srv.mu.Unlock()
			from := len(srv.received())
			wantUIDs := []uint32{11, 12, 11}
			for i, got := range s.FetchMessages(t.Context(), []ref.Ref{mk(1), mk(2), mk(1)}) {
				switch {
				case !gmail && !errors.Is(got.Err, mailstore.ErrStaleRef):
					t.Errorf("ref %d: %+v, want %v", i, got, mailstore.ErrStaleRef)
				case gmail && (got.Err != nil || got.Message.Summary.Ref.UIDValidity != 8 || got.Message.Summary.Ref.UID != wantUIDs[i]):
					t.Errorf("ref %d: %+v, want the message with UID %d under UIDVALIDITY 8", i, got, wantUIDs[i])
				}
			}
			want := []string{"UID FETCH 1:2", "EXAMINE INBOX"}
			if gmail {
				want = append(want, "UID SEARCH", "UID SEARCH", "UID SEARCH", "UID FETCH 11:12")
			}
			if got := readsSince(srv, from); !slices.Equal(got, want) {
				t.Errorf("commands %q, want %q; all %q", got, want, srv.received())
			}
		})
	}
}
