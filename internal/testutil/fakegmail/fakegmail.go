// Package fakegmail runs an in-memory IMAP server that speaks enough of
// Gmail's dialect for imapstore tests: X-GM-EXT-1 (X-GM-RAW, X-GM-MSGID,
// X-GM-THRID, X-GM-LABELS), PREVIEW, special-use folders, and Gmail's folder
// layout, where labels are folders and All Mail holds everything but Trash
// and Spam. It is read-only. Like Gmail, it sets \Seen when a client fetches
// a body without PEEK, and it counts read-write SELECTs, so tests can prove
// chainmail does neither.
package fakegmail

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// Message is a message on the fake server.
type Message struct {
	Raw  string
	Date time.Time
	// Labels such as `\Inbox`, `\Sent`, `\Draft` and user labels. A user label is also a folder.
	Labels []string
	Flags  []imap.Flag
	// ThreadOf is the 1-based index of an earlier message whose thread this
	// one joins. 0 starts a new thread.
	ThreadOf int
	Trash    bool
	Spam     bool
	// Preview is the PREVIEW answer. Nil answers NIL.
	Preview *string

	msgID    uint64
	threadID uint64
}

// Server is a running fake.
type Server struct {
	Endpoint provider.Endpoint

	username, password string

	mu              sync.Mutex
	msgs            []*Message
	folders         []*folder
	lastRaw         []string
	writableSelects int
	logins          int
	conns           map[net.Conn]bool
}

type folder struct {
	name        string
	attrs       []imap.MailboxAttr
	uidValidity uint32
	uids        map[*Message]imap.UID
	order       []*Message
	nextUID     imap.UID
	member      func(*Message) bool
}

const firstMsgID = 1700000000000000000

// Start runs a fake for one test and returns it. Messages get X-GM-MSGIDs in order.
func Start(t testing.TB, ca *tlstest.CA, username, password string, msgs ...Message) *Server {
	t.Helper()
	s := &Server{username: username, password: password, conns: map[net.Conn]bool{}}
	for i := range msgs {
		m := msgs[i]
		m.Flags = slices.Clone(m.Flags) // a fetch appends \Seen; keep the caller's slice untouched
		m.msgID = firstMsgID + uint64(i)
		m.threadID = m.msgID
		if m.ThreadOf > 0 {
			m.threadID = s.msgs[m.ThreadOf-1].threadID
		}
		s.msgs = append(s.msgs, &m)
	}
	s.buildFolders()
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &session{s: s}, nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {}, imap.CapGmailExt1: {}, imap.CapPreview: {}, imap.CapSpecialUse: {}, imap.CapUIDPlus: {},
		},
		Logger: log.New(io.Discard, "", 0),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Track the raw TCP connections under the TLS layer: imapserver only allows
	// LOGIN on a connection it can see is a *tls.Conn.
	go func() { _ = srv.Serve(tls.NewListener(&trackingListener{Listener: ln, s: s}, ca.Server)) }() // Serve ends with an error once Close runs
	// Close ln as well: if Close runs before Serve registers it, Serve never closes it.
	t.Cleanup(func() { _, _ = srv.Close(), ln.Close() }) // nothing to do about close errors at test end
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	s.Endpoint = provider.Endpoint{Host: "127.0.0.1", Port: addr.Port, Security: provider.SecurityTLS}
	return s
}

func hasLabel(m *Message, label string) bool {
	return slices.ContainsFunc(m.Labels, func(l string) bool { return strings.EqualFold(l, label) })
}

func live(m *Message) bool { return !m.Trash && !m.Spam }

func (s *Server) buildFolders() {
	add := func(name string, member func(*Message) bool, attrs ...imap.MailboxAttr) {
		s.folders = append(s.folders, &folder{name: name, attrs: attrs, member: member, uidValidity: uint32(100 + len(s.folders))})
	}
	add("INBOX", func(m *Message) bool { return live(m) && hasLabel(m, `\Inbox`) })
	add("[Gmail]/All Mail", live, imap.MailboxAttrAll)
	add("[Gmail]/Sent Mail", func(m *Message) bool { return live(m) && hasLabel(m, `\Sent`) }, imap.MailboxAttrSent)
	add("[Gmail]/Drafts", func(m *Message) bool { return live(m) && hasLabel(m, `\Draft`) }, imap.MailboxAttrDrafts)
	add("[Gmail]/Trash", func(m *Message) bool { return m.Trash }, imap.MailboxAttrTrash)
	add("[Gmail]/Spam", func(m *Message) bool { return m.Spam }, imap.MailboxAttrJunk)
	var userLabels []string
	for _, m := range s.msgs {
		for _, l := range m.Labels {
			if !strings.HasPrefix(l, `\`) && !slices.Contains(userLabels, l) {
				userLabels = append(userLabels, l)
			}
		}
	}
	for _, l := range userLabels {
		add(l, func(m *Message) bool { return live(m) && hasLabel(m, l) })
	}
	for _, f := range s.folders {
		f.uids = map[*Message]imap.UID{}
		f.nextUID = 1
		for _, m := range s.msgs {
			if f.member(m) {
				f.uids[m] = f.nextUID
				f.order = append(f.order, m)
				f.nextUID++
			}
		}
	}
}

func (s *Server) folderLocked(name string) *folder {
	for _, f := range s.folders {
		if f.name == name || (strings.EqualFold(name, "INBOX") && f.name == "INBOX") {
			return f
		}
	}
	return nil
}

// MsgID returns the X-GM-MSGID of the i-th message, counting from 0.
func (s *Server) MsgID(i int) uint64 { return s.msgs[i].msgID }

// ThreadID returns the X-GM-THRID of the i-th message.
func (s *Server) ThreadID(i int) uint64 { return s.msgs[i].threadID }

// Flags returns the i-th message's flags.
func (s *Server) Flags(i int) []imap.Flag {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.msgs[i].Flags)
}

// LastRaw returns every X-GM-RAW string received, in order.
func (s *Server) LastRaw() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.lastRaw)
}

// WritableSelects counts SELECTs that were not EXAMINE.
func (s *Server) WritableSelects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writableSelects
}

// Logins counts successful logins.
func (s *Server) Logins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

// ResetAllMail gives All Mail a new UIDVALIDITY and new UIDs, as Gmail does
// rarely, and drops every connection, as a server would.
func (s *Server) ResetAllMail() {
	s.mu.Lock()
	f := s.folderLocked("[Gmail]/All Mail")
	f.uidValidity += 1000
	for m, uid := range f.uids {
		f.uids[m] = uid + 500
	}
	f.nextUID += 500
	s.mu.Unlock()
	s.DropConnections()
}

// DropConnections closes every open client connection.
func (s *Server) DropConnections() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close() // dropping is the point
	}
}

type trackingListener struct {
	net.Listener
	s *Server
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.s.mu.Lock()
	l.s.conns[c] = true
	l.s.mu.Unlock()
	return &trackedConn{Conn: c, s: l.s}, nil
}

type trackedConn struct {
	net.Conn
	s *Server
}

func (c *trackedConn) Close() error {
	c.s.mu.Lock()
	delete(c.s.conns, c.Conn)
	c.s.mu.Unlock()
	return c.Conn.Close()
}
