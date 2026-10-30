// Package fakegmail runs an in-memory IMAP server that speaks enough of
// Gmail's dialect for imapstore tests: X-GM-EXT-1 (X-GM-RAW, X-GM-MSGID,
// X-GM-THRID, X-GM-LABELS), ESEARCH, LIST-STATUS, special-use folders, and
// Gmail's folder layout, where labels are folders and All Mail holds
// everything but Trash and Spam. Like Gmail, it sets \Seen when a client
// fetches a body without PEEK, and it counts read-write SELECTs, so tests can
// prove chainmail does neither on its read paths.
//
// It accepts writes the way Gmail does with its default IMAP settings and
// expunge set to move to Trash:
//   - STORE of flags. Flags belong to the message and show in every folder,
//     except \Deleted, which belongs to the folder it was set in (a fixture's
//     \Deleted marks every folder the message starts in). \Flagged and the
//     \Starred label are one state, whichever a write changes. A STORE
//     naming no current message answers OK with no FETCH.
//   - STORE of X-GM-LABELS, in any folder. A user label the server lacks is
//     created, the folder names INBOX, Sent Mail and Drafts stand for their
//     system labels, and All Mail, Trash and Spam answer NO. The answer
//     carries UID and X-GM-LABELS unless SILENT.
//   - COPY and APPEND to a label folder (INBOX, Sent Mail, Drafts or a user
//     label) add that label; to Trash or Spam they move the message there; to
//     All Mail they add nothing. MOVE does the same and also drops the
//     source folder's label when the destination is a label folder or All
//     Mail. Both answer COPYUID; APPEND answers APPENDUID.
//   - EXPUNGE and UID EXPUNGE of messages marked \Deleted in the selected
//     folder: in Trash or Spam the message is gone for good, in All Mail it
//     moves to Trash, and in a label folder it loses that label.
//   - CREATE, RENAME (children follow) and DELETE of user labels. INBOX, the
//     [Gmail] folders and a RENAME beneath the label itself answer NO.
//   - CREATE, RENAME, DELETE, label STORE, X-GM-RAW and folder membership
//     match label names ignoring case with Unicode simple case folding, as
//     strings.EqualFold does, so Σ, σ and ς are one label and ß is not ss.
//   - In UID sets, * is the highest UID still in the folder.
//   - A message that leaves Trash or Spam gets a new UID in each folder it
//     returns to, as on Gmail. Moving to Trash keeps the message's labels, so
//     restoring it brings them back.
//
// The fake is meant to be no more lenient than Gmail, so that a client bug
// fails here rather than in production:
//   - It advertises Gmail's capabilities (measured live on 2026-10-29) except
//     COMPRESS=DEFLATE, CONDSTORE, ID, QUOTA and XLIST, which imapserver
//     cannot offer. Gmail has no PREVIEW, so neither does the fake unless
//     Options.Preview asks for it.
//   - SEARCH answers BAD for any criterion it does not implement, rather
//     than ignoring it.
//   - Each folder numbers its UIDs from its own base, so a UID from one folder
//     does not happen to name a message in another. All Mail counts from 1,
//     INBOX from 101, Sent Mail 201, Trash 301, Spam 401, Drafts 501, and user
//     labels from 1001 in steps of 100. The bases grow tenfold when there are
//     100 or more messages at start. A folder that gains more messages than
//     the step afterwards runs into the next folder's range.
//   - X-GM-MSGID and X-GM-THRID are unsigned 64-bit. The fake's are above
//     math.MaxInt64 (Gmail's are lower today), so a signed 64-bit slip fails.
//   - FETCH and STATUS answer BAD for items of extensions it does not
//     advertise, such as BINARY and CONDSTORE.
//   - STORE, MOVE and EXPUNGE answer NO in a folder opened with EXAMINE.
//
// It is stricter than Gmail in one place: X-GM-RAW under NOT or OR is
// refused. It is still looser in some: matching is by substring where Gmail
// tokenises, is: values other than read and unread mean starred, any has:
// means an attachment, and LIST patterns are case-sensitive even for INBOX.
// Another session learns of changes at its next command, as EXISTS and
// EXPUNGE, but not of flag changes. Sequence sets reach only the messages
// the last EXISTS announced, but a UID command can name a newer arrival, and
// its FETCH answer then comes before the EXISTS: imapserver lets a session
// write EXISTS only once the command is done.
package fakegmail

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

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
	gone     bool // expunged from Trash or Spam, so in no folder
}

// Options changes the fake's defaults, which match Gmail.
type Options struct {
	// Preview advertises PREVIEW and answers FETCH PREVIEW from
	// Message.Preview. Gmail has no PREVIEW, so leave it off unless a test
	// needs a server that does.
	Preview bool
}

// RawSearch is one X-GM-RAW string and the folder it was searched in.
type RawSearch struct {
	Folder string
	Raw    string
}

// Server is a running fake.
type Server struct {
	Endpoint provider.Endpoint

	username, password string
	opts               Options

	mu              sync.Mutex
	msgs            []*Message
	folders         []*folder
	step            imap.UID // the gap between folders' UID bases
	userFolders     int      // user label folders ever made, for their UID bases
	rawSearches     []RawSearch
	writableSelects int
	logins          int
	conns           map[net.Conn]bool
}

// folderKind says which messages a folder holds.
type folderKind int

const (
	kindLabel folderKind = iota // messages outside Trash and Spam that carry the folder's label
	kindAll                     // every message outside Trash and Spam
	kindTrash
	kindSpam
)

type folder struct {
	name        string
	attrs       []imap.MailboxAttr
	uidValidity uint32
	kind        folderKind
	label       string // for kindLabel: `\Inbox`, `\Sent`, `\Draft` or the user label
	uids        map[*Message]imap.UID
	order       []*Message // in UID order
	nextUID     imap.UID
	deleted     map[*Message]bool // \Deleted is per folder on Gmail
}

// firstMsgID is above math.MaxInt64, so an int64 anywhere in a client's
// X-GM-MSGID or X-GM-THRID path overflows.
const firstMsgID = 1<<63 + 1700000000000000

// appendLimit is the APPENDLIMIT Gmail advertises.
const appendLimit = 35651584

// Start runs a fake with Gmail's capabilities for one test and returns it.
// Messages get X-GM-MSGIDs in order.
func Start(t testing.TB, ca *tlstest.CA, username, password string, msgs ...Message) *Server {
	t.Helper()
	return StartWith(t, ca, Options{}, username, password, msgs...)
}

// StartWith is Start with options.
func StartWith(t testing.TB, ca *tlstest.CA, opts Options, username, password string, msgs ...Message) *Server {
	t.Helper()
	s := &Server{username: username, password: password, opts: opts, conns: map[net.Conn]bool{}}
	for i := range msgs {
		m := msgs[i]
		// Writes change flags and labels; keep the caller's slices untouched.
		m.Flags, m.Labels = slices.Clone(m.Flags), slices.Clone(m.Labels)
		// Gmail's star is one state: a fixture that names either form has both.
		if hasLabel(&m, labelStarred) || slices.Contains(m.Flags, imap.FlagFlagged) {
			addLabel(&m, labelStarred)
			starFromLabels(&m)
		}
		m.msgID = firstMsgID + uint64(i)
		m.threadID = m.msgID
		if m.ThreadOf > 0 {
			m.threadID = s.msgs[m.ThreadOf-1].threadID
		}
		s.msgs = append(s.msgs, &m)
	}
	s.buildFolders()
	// imapserver adds UNSELECT, ENABLE, IDLE, UTF8=ACCEPT, LITERAL- and
	// SASL-IR itself, and APPENDLIMIT=n from the session.
	caps := imap.CapSet{
		imap.CapIMAP4rev1: {}, imap.CapGmailExt1: {}, imap.CapSpecialUse: {}, imap.CapUIDPlus: {},
		imap.CapESearch: {}, imap.CapChildren: {}, imap.CapListExtended: {}, imap.CapListStatus: {},
		imap.CapMove: {}, imap.CapNamespace: {},
	}
	if opts.Preview {
		caps[imap.CapPreview] = struct{}{}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &session{s: s}, nil, nil
		},
		Caps:   caps,
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

// foldRune maps r to the smallest rune in its Unicode simple case folding
// orbit, so two runes share it exactly when strings.EqualFold matches them.
func foldRune(r rune) rune {
	low := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		low = min(low, f)
	}
	return low
}

// foldKey maps a label to a key that two labels share exactly when
// strings.EqualFold matches them. strings.ToLower is not such a key: it
// keeps σ and ς apart.
func foldKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(foldRune(r))
	}
	return b.String()
}

// cutFoldPrefix reports whether s begins with prefix, matched as
// strings.EqualFold matches, and returns the rest of s. The two can differ in
// byte length, as k and the Kelvin sign do, so s is never sliced by
// len(prefix).
func cutFoldPrefix(s, prefix string) (rest string, ok bool) {
	for _, pr := range prefix {
		if s == "" {
			return "", false
		}
		sr, n := utf8.DecodeRuneInString(s)
		if foldRune(sr) != foldRune(pr) {
			return "", false
		}
		s = s[n:]
	}
	return s, true
}

func hasLabel(m *Message, label string) bool {
	return slices.ContainsFunc(m.Labels, func(l string) bool { return strings.EqualFold(l, label) })
}

func addLabel(m *Message, label string) {
	if !hasLabel(m, label) {
		m.Labels = append(m.Labels, label)
	}
}

func removeLabel(m *Message, label string) {
	m.Labels = slices.DeleteFunc(m.Labels, func(l string) bool { return strings.EqualFold(l, label) })
}

// labelStarred is the label Gmail links to the \Flagged flag.
const labelStarred = `\Starred`

// starFromFlags gives m the \Starred label exactly when it is \Flagged, as
// Gmail does after a flag STORE.
func starFromFlags(m *Message) {
	if slices.Contains(m.Flags, imap.FlagFlagged) {
		addLabel(m, labelStarred)
	} else {
		removeLabel(m, labelStarred)
	}
}

// starFromLabels flags m exactly when it carries \Starred, as Gmail does
// after a label STORE.
func starFromLabels(m *Message) {
	switch flagged := slices.Contains(m.Flags, imap.FlagFlagged); {
	case hasLabel(m, labelStarred) && !flagged:
		m.Flags = append(m.Flags, imap.FlagFlagged)
	case !hasLabel(m, labelStarred) && flagged:
		m.Flags = slices.DeleteFunc(m.Flags, func(fl imap.Flag) bool { return fl == imap.FlagFlagged })
	}
}

func live(m *Message) bool { return !m.Trash && !m.Spam && !m.gone }

func (f *folder) member(m *Message) bool {
	switch f.kind {
	case kindAll:
		return live(m)
	case kindTrash:
		return m.Trash && !m.gone
	case kindSpam:
		return m.Spam && !m.gone
	default:
		return live(m) && hasLabel(m, f.label)
	}
}

// add gives m the folder's next UID.
func (f *folder) add(m *Message) imap.UID {
	uid := f.nextUID
	f.uids[m] = uid
	f.order = append(f.order, m)
	f.nextUID++
	return uid
}

// ensure returns m's UID in f, giving it one if it has none. A caller that
// files several messages calls it in source order, so COPYUID's sets line up.
func (f *folder) ensure(m *Message) imap.UID {
	if uid, ok := f.uids[m]; ok {
		return uid
	}
	return f.add(m)
}

// highestUID is what * stands for in f's UID sets: the highest UID still in
// the folder, or UIDNEXT when it is empty, as RFC 9051 says.
func (f *folder) highestUID() imap.UID {
	if n := len(f.order); n > 0 {
		return f.uids[f.order[n-1]] // order is in UID order
	}
	return f.nextUID
}

// flags returns m's flags as seen in f.
func (f *folder) flags(m *Message) []imap.Flag {
	flags := slices.Clone(m.Flags)
	if f.deleted[m] && !slices.Contains(flags, imap.FlagDeleted) {
		flags = append(flags, imap.FlagDeleted)
	}
	return flags
}

func (s *Server) addFolderLocked(name string, kind folderKind, label string, base imap.UID, attrs ...imap.MailboxAttr) *folder {
	f := &folder{
		name: name, attrs: attrs, kind: kind, label: label, nextUID: base, uidValidity: uint32(100 + len(s.folders)),
		uids: map[*Message]imap.UID{}, deleted: map[*Message]bool{},
	}
	s.folders = append(s.folders, f)
	return f
}

// addUserFolderLocked adds a folder for a user label, after the others.
func (s *Server) addUserFolderLocked(label string) *folder {
	f := s.addFolderLocked(label, kindLabel, label, imap.UID(10+s.userFolders)*s.step+1)
	s.userFolders++
	return f
}

func (s *Server) buildFolders() {
	// step keeps each folder's UID range clear of the next one's.
	s.step = imap.UID(100)
	for int(s.step) <= len(s.msgs) {
		s.step *= 10
	}
	step := s.step
	s.addFolderLocked("INBOX", kindLabel, `\Inbox`, step+1)
	s.addFolderLocked("[Gmail]/All Mail", kindAll, "", 1, imap.MailboxAttrAll)
	s.addFolderLocked("[Gmail]/Sent Mail", kindLabel, `\Sent`, 2*step+1, imap.MailboxAttrSent)
	s.addFolderLocked("[Gmail]/Drafts", kindLabel, `\Draft`, 5*step+1, imap.MailboxAttrDrafts)
	s.addFolderLocked("[Gmail]/Trash", kindTrash, "", 3*step+1, imap.MailboxAttrTrash)
	s.addFolderLocked("[Gmail]/Spam", kindSpam, "", 4*step+1, imap.MailboxAttrJunk)
	for _, m := range s.msgs {
		for _, l := range m.Labels {
			if !strings.HasPrefix(l, `\`) && s.folderFoldLocked(l) == nil {
				s.addUserFolderLocked(l)
			}
		}
	}
	s.refreshLocked()
	// A fixture's \Deleted marks the message in each folder it starts in,
	// where STORE and EXPUNGE look for it, rather than in the shared flags.
	for _, m := range s.msgs {
		if !slices.Contains(m.Flags, imap.FlagDeleted) {
			continue
		}
		m.Flags = slices.DeleteFunc(m.Flags, func(fl imap.Flag) bool { return fl == imap.FlagDeleted })
		for _, f := range s.folders {
			if _, ok := f.uids[m]; ok {
				f.deleted[m] = true
			}
		}
	}
}

// refreshLocked brings every folder's membership up to date after a change:
// messages that left lose their UID there, and messages that arrived get the
// next UIDs in message order. Callers that need UIDs in another order give
// them out first with folder.ensure.
func (s *Server) refreshLocked() {
	for _, f := range s.folders {
		f.order = slices.DeleteFunc(f.order, func(m *Message) bool {
			if f.member(m) {
				return false
			}
			delete(f.uids, m)
			delete(f.deleted, m)
			return true
		})
		for _, m := range s.msgs {
			if _, ok := f.uids[m]; !ok && f.member(m) {
				f.add(m)
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

// folderFoldLocked finds a folder ignoring case, as Gmail matches labels.
func (s *Server) folderFoldLocked(name string) *folder {
	for _, f := range s.folders {
		if strings.EqualFold(f.name, name) {
			return f
		}
	}
	return nil
}

// fileLocked puts m into f the way Gmail does for COPY and APPEND.
func fileLocked(m *Message, f *folder) {
	switch f.kind {
	case kindTrash:
		m.Trash, m.Spam = true, false
	case kindSpam:
		m.Trash, m.Spam = false, true
	case kindAll:
		m.Trash, m.Spam = false, false
	default:
		m.Trash, m.Spam = false, false
		addLabel(m, f.label)
	}
}

// moveLocked moves m from src to dest the way Gmail does for MOVE: it files m
// in dest and drops src's label when dest is a label folder or All Mail. A
// move to Trash or Spam keeps the labels for a later restore.
func moveLocked(m *Message, src, dest *folder) {
	if src != dest && src.kind == kindLabel && (dest.kind == kindLabel || dest.kind == kindAll) {
		removeLabel(m, src.label)
	}
	fileLocked(m, dest)
}

// expungeLocked expunges m from f the way Gmail does: for good from Trash or
// Spam, to Trash from All Mail, and by dropping the label elsewhere.
func expungeLocked(m *Message, f *folder) {
	switch f.kind {
	case kindTrash, kindSpam:
		m.gone = true
	case kindAll:
		m.Trash, m.Spam = true, false
	default:
		removeLabel(m, f.label)
	}
}

// messageIDRE finds the <id> tokens of Message-ID, In-Reply-To and References.
var messageIDRE = regexp.MustCompile(`<[^<>\s]+>`)

// newMessageLocked adds a message as Gmail does on delivery or APPEND. It
// joins the thread of an earlier message that its In-Reply-To or References
// name, as Gmail threads replies.
func (s *Server) newMessageLocked(raw string, date time.Time, flags []imap.Flag) *Message {
	m := &Message{Raw: raw, Date: date, Flags: flags, msgID: firstMsgID + uint64(len(s.msgs))}
	m.threadID = m.msgID
	refs := messageIDRE.FindAllString(header(m, "In-Reply-To")+" "+header(m, "References"), -1)
	for _, old := range s.msgs {
		if id := strings.TrimSpace(header(old, "Message-Id")); id != "" && slices.Contains(refs, id) {
			m.threadID = old.threadID
			break
		}
	}
	s.msgs = append(s.msgs, m)
	return m
}

// FileSent adds raw as Gmail files a message submitted over SMTP: read, with
// the \Sent label, in Sent Mail and All Mail.
func (s *Server) FileSent(raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.newMessageLocked(string(raw), time.Now(), []imap.Flag{imap.FlagSeen})
	addLabel(m, `\Sent`)
	s.refreshLocked()
}

// MsgID returns the X-GM-MSGID of the i-th message, counting from 0. Messages
// added by APPEND and FileSent count on from those given to Start.
func (s *Server) MsgID(i int) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgs[i].msgID
}

// ThreadID returns the X-GM-THRID of the i-th message.
func (s *Server) ThreadID(i int) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgs[i].threadID
}

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
	raws := make([]string, len(s.rawSearches))
	for i, r := range s.rawSearches {
		raws[i] = r.Raw
	}
	return raws
}

// RawSearches returns every X-GM-RAW string received, in order, with the
// folder that was selected when it arrived.
func (s *Server) RawSearches() []RawSearch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.rawSearches)
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
// rarely, and drops every connection, as a server would. The new UIDs are
// above every other folder's, so none of them names a message elsewhere.
func (s *Server) ResetAllMail() {
	s.mu.Lock()
	f := s.folderLocked("[Gmail]/All Mail")
	f.uidValidity += 1000
	var shift imap.UID
	for _, other := range s.folders {
		shift = max(shift, other.nextUID-1)
	}
	for m, uid := range f.uids {
		f.uids[m] = uid + shift
	}
	f.nextUID += shift
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
