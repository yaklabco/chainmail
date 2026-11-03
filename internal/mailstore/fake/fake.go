// Package fake is an in-memory mailstore.Store for daemon component tests.
// Its query evaluation is simple substring matching: it exists to test the
// daemon's fan-out, merging, cursors and errors, not search semantics. Its
// folder model, sources, thread IDs, sizes and stale-ref handling follow
// internal/mailstore/imapstore, so daemon tests see what a server would give.
package fake

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"math"
	"mime"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// Attachment is an attachment part of a fake message.
type Attachment struct {
	Part        string
	Filename    string
	ContentType string
	Data        []byte
}

// Message is a fake message. Add assigns its UID and, on Gmail accounts, any
// GmailMsgID or GmailThreadID left zero. Bcc is what a message in Sent or
// Drafts carries; bcc: matches it, as IMAP SEARCH BCC does, and a received
// message seeded without one never matches bcc:.
type Message struct {
	Folder      string
	Date        time.Time
	From        []mailstore.Address
	To          []mailstore.Address
	Cc          []mailstore.Address
	Bcc         []mailstore.Address
	Subject     string
	Body        string
	Flags       []string
	Labels      []string
	MessageID   string
	InReplyTo   []string
	References  []string
	Attachments []Attachment
	// GmailMsgID is the X-GM-MSGID. When it is zero, Add gives the message
	// the next unused one from 1000.
	GmailMsgID uint64
	// GmailThreadID is Gmail's thread ID, from X-GM-EXT-1. When it is
	// zero, Add puts the message in the thread of an earlier message with the
	// same root Message-ID, or else in a new thread.
	GmailThreadID uint64
	// InternalDate is when the server received the message. Zero means Date.
	InternalDate time.Time

	uid uint32
	// views are the UIDs a Gmail message has in label folders, by folder,
	// given out by refInLocked.
	views map[string]uint32
	// raw is the message as Append received it. When set, it is what the
	// store streams, instead of a message built from the fields.
	raw []byte
	// modseq is the MODSEQ of the message's last change in its folder:
	// arriving there, or a flag or label edit.
	modseq uint64
}

// Store is an in-memory account.
type Store struct {
	mu      sync.Mutex
	account string
	gmail   bool
	now     func() time.Time
	// folders are kept INBOX first, then by name.
	folders     []mailstore.Folder
	uidValidity map[string]uint32
	nextUID     map[string]uint32
	msgs        []*Message
	msgIDs      map[uint64]bool
	threadIDs   map[uint64]bool
	nextMsgID   uint64
	fail        map[string]error
	folderFail  map[string]error
	streamErr   error
	streamAfter int
	calls       map[string]int
	closed      bool
	// everValidity is the highest UIDVALIDITY each folder name has had, so a
	// folder deleted and created again, or renamed onto an old name, gets a
	// new one and old refs into it go stale, as on a server.
	everValidity map[string]uint32
	// Writer state and test hooks, in write.go.
	writeFail     map[string]error
	ops           []string
	appended      [][]byte
	archiveFolder string
	// nsPrefix and nsDelim are the namespace SetNamespace gives.
	nsPrefix, nsDelim string
	// Watch state and test hooks, in watch.go. modseq is each folder's
	// HIGHESTMODSEQ, condstore whether the server advertises CONDSTORE,
	// and watchFail the error FailWatch set for the next pass.
	modseq    map[string]uint64
	condstore bool
	watchFail error
	// idle is whether the server advertises IDLE, after the timer Idle
	// waits on, watches the open watch connections, and idleStarted the
	// channel IdleStarted returns. quietIdle keeps changes from waking a
	// waiting Idle, as SetQuietIdle says.
	idle        bool
	quietIdle   bool
	after       func(time.Duration) <-chan time.Time
	watches     map[*watchConn]bool
	idleStarted chan string
}

var _ mailstore.Store = (*Store)(nil)

// New returns an empty account. Gmail accounts get Gmail's folders and
// accept has:, filename: and label:. On Gmail, messages live in All Mail,
// Trash or Spam, and INBOX, Sent Mail and any label folder are views of All
// Mail by label, as on the server.
func New(account string, gmail bool) *Store {
	s := &Store{
		account: account, gmail: gmail, now: time.Now,
		uidValidity: map[string]uint32{}, nextUID: map[string]uint32{},
		msgIDs: map[uint64]bool{}, threadIDs: map[uint64]bool{}, nextMsgID: 1000,
		fail: map[string]error{}, folderFail: map[string]error{}, calls: map[string]int{},
		everValidity: map[string]uint32{}, writeFail: map[string]error{},
		modseq: map[string]uint64{}, condstore: true, idle: true, after: time.After,
		watches: map[*watchConn]bool{}, idleStarted: make(chan string, idleStartedCap),
	}
	if gmail {
		s.AddFolder("INBOX", mailstore.RoleInbox)
		s.AddFolder("[Gmail]/All Mail", mailstore.RoleAll)
		s.AddFolder("[Gmail]/Sent Mail", mailstore.RoleSent)
		s.AddFolder("[Gmail]/Trash", mailstore.RoleTrash)
		s.AddFolder("[Gmail]/Spam", mailstore.RoleSpam)
		return s
	}
	for _, f := range []struct {
		name string
		role mailstore.Role
	}{
		{"INBOX", mailstore.RoleInbox},
		{"Sent", mailstore.RoleSent},
		{"Drafts", mailstore.RoleDrafts},
		{"Archive", mailstore.RoleArchive},
		{"Trash", mailstore.RoleTrash},
		{"Junk", mailstore.RoleSpam},
	} {
		s.AddFolder(f.name, f.role)
	}
	return s
}

// AddFolder adds a selectable folder with UIDVALIDITY 1, or one more than
// the name had before when it was deleted or renamed away. On Gmail a folder
// other than All Mail, Trash and Spam is a label: it lists the All Mail
// messages with that label, or \Inbox, \Sent or \Draft for those roles. It
// panics when the folder already exists.
func (s *Store) AddFolder(name string, role mailstore.Role) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.uidValidity[name]; ok {
		panic(fmt.Sprintf("fake: folder %q already exists", name))
	}
	s.addFolderLocked(name, role, 1, 1)
}

// addFolderLocked adds a selectable folder with UIDVALIDITY v, raised past
// any the name had before, and next UID next.
func (s *Store) addFolderLocked(name string, role mailstore.Role, v, next uint32) {
	if v <= s.everValidity[name] {
		v = s.everValidity[name] + 1
	}
	s.folders = append(s.folders, mailstore.Folder{Name: name, Role: role, Selectable: true})
	slices.SortStableFunc(s.folders, func(a, b mailstore.Folder) int { return compareFolders(a.Name, b.Name) })
	s.uidValidity[name] = v
	s.everValidity[name] = v
	s.nextUID[name] = next
	// A new folder starts at HIGHESTMODSEQ 1.
	s.modseq[name] = 1
}

// removeFolderLocked forgets a folder. Its messages must already be gone.
func (s *Store) removeFolderLocked(name string) {
	s.folders = slices.DeleteFunc(s.folders, func(f mailstore.Folder) bool { return f.Name == name })
	delete(s.uidValidity, name)
	delete(s.nextUID, name)
	delete(s.modseq, name)
	delete(s.folderFail, name)
}

// compareFolders orders INBOX first, then by name ignoring case, as imapstore does.
func compareFolders(a, b string) int {
	if ai, bi := strings.EqualFold(a, "INBOX"), strings.EqualFold(b, "INBOX"); ai != bi {
		if ai {
			return -1
		}
		return 1
	}
	return cmp.Compare(strings.ToLower(a), strings.ToLower(b))
}

// Add stores a copy of m and returns its ref. InternalDate defaults to Date.
// Add panics, as a test helper should, when m.Folder does not exist, when a
// Gmail message is not in All Mail, Trash or Spam, or when an explicit
// GmailMsgID is already in use.
func (s *Store) Add(m Message) ref.Ref {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.uidValidity[m.Folder]; !ok {
		panic(fmt.Sprintf("fake: no folder %q; call AddFolder first", m.Folder))
	}
	m = cloneMessage(m)
	if s.gmail {
		switch s.roleLocked(m.Folder) {
		case mailstore.RoleAll, mailstore.RoleTrash, mailstore.RoleSpam:
		default:
			panic(fmt.Sprintf("fake: Gmail messages live in All Mail, Trash or Spam, not %q; use Labels", m.Folder))
		}
		s.assignGmailIDsLocked(&m)
	} else {
		m.GmailMsgID, m.GmailThreadID = 0, 0
	}
	if m.InternalDate.IsZero() {
		m.InternalDate = m.Date
	}
	m.uid = s.nextUID[m.Folder]
	s.nextUID[m.Folder]++
	s.msgs = append(s.msgs, &m)
	s.touchLocked(&m)
	return s.refLocked(&m)
}

// assignGmailIDsLocked fills in a zero X-GM-MSGID and thread ID. Gmail
// names a new thread after its first message, so a fresh thread ID is the
// message's own X-GM-MSGID unless another thread already has it.
func (s *Store) assignGmailIDsLocked(m *Message) {
	switch {
	case m.GmailMsgID == 0:
		for s.msgIDs[s.nextMsgID] {
			s.nextMsgID++
		}
		m.GmailMsgID = s.nextMsgID
		s.nextMsgID++
	case s.msgIDs[m.GmailMsgID]:
		panic(fmt.Sprintf("fake: X-GM-MSGID %d is already in use", m.GmailMsgID))
	}
	s.msgIDs[m.GmailMsgID] = true
	if m.GmailThreadID == 0 {
		if root := rootOf(m); root != "" {
			for _, o := range s.msgs {
				if rootOf(o) == root {
					m.GmailThreadID = o.GmailThreadID
					break
				}
			}
		}
	}
	if m.GmailThreadID == 0 {
		id := m.GmailMsgID
		for s.threadIDs[id] {
			id++
		}
		m.GmailThreadID = id
	}
	s.threadIDs[m.GmailThreadID] = true
}

// SetNow replaces the clock used for newer_than: and older_than:.
func (s *Store) SetNow(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// SetUIDValidity changes a folder's UIDVALIDITY, as a server reset would.
func (s *Store) SetUIDValidity(folder string, v uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustHaveFolderLocked(folder)
	s.uidValidity[folder] = v
	s.everValidity[folder] = max(s.everValidity[folder], v)
}

// Fail makes method (such as "Search" or "FetchMessages") return err. A nil err clears it.
func (s *Store) Fail(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[method] = err
}

// FailFolder makes Search fail in one folder. A nil err clears it.
func (s *Store) FailFolder(folder string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustHaveFolderLocked(folder)
	s.folderFail[folder] = err
}

// FailStreamAfter makes StreamPart readers fail with err after n bytes.
func (s *Store) FailStreamAfter(n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamAfter, s.streamErr = n, err
}

// Calls returns how many times method ran.
func (s *Store) Calls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

// Closed reports whether Close ran.
func (s *Store) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Flags returns the flags of the message r points at.
func (s *Store) Flags(r ref.Ref) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.findLocked(r.Folder, r.UID); m != nil {
		return slices.Clone(m.Flags)
	}
	return nil
}

// RawMessage returns what StreamPart(r, "") streams.
func (s *Store) RawMessage(r ref.Ref) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.findLocked(r.Folder, r.UID); m != nil {
		return rawMessage(m, true)
	}
	return nil
}

func (s *Store) begin(method string) error {
	s.calls[method]++
	return s.fail[method]
}

// ListFolders implements mailstore.Store.
func (s *Store) ListFolders(context.Context) ([]mailstore.Folder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin("ListFolders"); err != nil {
		return nil, err
	}
	out := slices.Clone(s.folders)
	for i := range out {
		for _, m := range s.msgs {
			if !s.inFolderLocked(out[i], m) {
				continue
			}
			out[i].Messages++
			if !slices.Contains(m.Flags, `\Seen`) {
				out[i].Unseen++
			}
		}
	}
	return out, nil
}

// inFolderLocked reports whether m shows in f. On Gmail only All Mail, Trash
// and Spam hold messages, and every other folder is an All Mail label.
func (s *Store) inFolderLocked(f mailstore.Folder, m *Message) bool {
	if !s.gmail {
		return m.Folder == f.Name
	}
	switch f.Role {
	case mailstore.RoleAll, mailstore.RoleTrash, mailstore.RoleSpam:
		return m.Folder == f.Name
	}
	if s.roleLocked(m.Folder) != mailstore.RoleAll {
		return false
	}
	return hasLabel(m, labelOf(f))
}

// labelOf is the Gmail label a label folder lists: \Inbox, \Sent or \Draft
// for those roles, else the folder's name.
func labelOf(f mailstore.Folder) string {
	switch f.Role {
	case mailstore.RoleInbox:
		return `\Inbox`
	case mailstore.RoleSent:
		return `\Sent`
	case mailstore.RoleDrafts:
		return `\Draft`
	}
	return f.Name
}

// Search implements mailstore.Store.
func (s *Store) Search(_ context.Context, req mailstore.SearchRequest) ([]mailstore.SourceResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin("Search"); err != nil {
		return nil, err
	}
	if err := s.checkSupported(req.Query); err != nil {
		return nil, err
	}
	var names []string
	if req.Bounds != nil {
		names = slices.Sorted(maps.Keys(req.Bounds))
	} else {
		var err error
		if names, err = s.sourcesLocked(req.In); err != nil {
			return nil, err
		}
	}
	keep := s.inFilter(req.In)
	out := make([]mailstore.SourceResult, 0, len(names))
	for _, name := range names {
		out = append(out, s.searchSourceLocked(name, req, keep))
	}
	return out, nil
}

func (s *Store) searchSourceLocked(name string, req mailstore.SearchRequest, keep func(*Message) bool) mailstore.SourceResult {
	v, exists := s.uidValidity[name]
	res := mailstore.SourceResult{Source: mailstore.Source{Folder: name, UIDValidity: v}}
	bound, continuing := req.Bounds[name]
	switch {
	case !exists:
		res.Err = fmt.Errorf("%w: no folder %q", mailstore.ErrNotFound, name)
	case s.folderFail[name] != nil:
		res.Err = s.folderFail[name]
	case continuing && bound.UIDValidity != v:
		res.Err = fmt.Errorf("%w: %s changed since the first page; search again", mailstore.ErrStaleRef, name)
	default:
		for _, m := range s.msgs {
			if m.Folder != name || (continuing && bound.Below > 0 && m.uid >= bound.Below) || !keep(m) || !s.match(req.Query, m) {
				continue
			}
			res.UIDs = append(res.UIDs, m.uid)
		}
		slices.SortFunc(res.UIDs, func(a, b uint32) int { return cmp.Compare(b, a) })
		res.Count = uint32(len(res.UIDs))
		if req.CountOnly {
			res.UIDs = nil
		}
	}
	return res
}

// FetchSummaries implements mailstore.Store.
func (s *Store) FetchSummaries(_ context.Context, src mailstore.Source, uids []uint32, opts mailstore.SummaryOptions) ([]mailstore.Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin("FetchSummaries"); err != nil {
		return nil, err
	}
	if v, ok := s.uidValidity[src.Folder]; !ok {
		return nil, fmt.Errorf("%w: no folder %q", mailstore.ErrNotFound, src.Folder)
	} else if v != src.UIDValidity {
		return nil, fmt.Errorf("%w: %s changed", mailstore.ErrStaleRef, src.Folder)
	}
	var out []mailstore.Summary
	for _, uid := range uids {
		if m := s.findLocked(src.Folder, uid); m != nil {
			out = append(out, s.summaryLocked(m, s.refInLocked(m, src.Folder), opts))
		}
	}
	return out, nil
}

// FetchMessages implements mailstore.Store.
func (s *Store) FetchMessages(_ context.Context, refs []ref.Ref) []mailstore.MessageResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mailstore.MessageResult, len(refs))
	err := s.begin("FetchMessages")
	for i, r := range refs {
		if err != nil {
			out[i].Err = err
			continue
		}
		m, ferr := s.resolveLocked(r)
		if ferr != nil {
			out[i].Err = ferr
			continue
		}
		out[i].Message = &mailstore.Message{
			Summary: s.summaryLocked(m, s.refInLocked(m, r.Folder), mailstore.SummaryOptions{}), Cc: slices.Clone(m.Cc), MessageID: m.MessageID,
			InReplyTo: slices.Clone(m.InReplyTo), References: slices.Clone(m.References),
			Raw: rawMessage(m, false), Attachments: attachments(m),
		}
	}
	return out
}

// StreamPart implements mailstore.Store.
func (s *Store) StreamPart(_ context.Context, r ref.Ref, part string) (mailstore.PartInfo, io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin("StreamPart"); err != nil {
		return mailstore.PartInfo{}, nil, err
	}
	m, err := s.resolveLocked(r)
	if err != nil {
		return mailstore.PartInfo{}, nil, err
	}
	var info mailstore.PartInfo
	var data []byte
	if part == "" {
		data = rawMessage(m, true)
		info = mailstore.PartInfo{ContentType: "message/rfc822", Size: int64(len(data))}
	} else {
		i := slices.IndexFunc(m.Attachments, func(a Attachment) bool { return a.Part == part })
		if i < 0 {
			return mailstore.PartInfo{}, nil, fmt.Errorf("%w: part %s", mailstore.ErrNotFound, part)
		}
		a := m.Attachments[i]
		data = slices.Clone(a.Data)
		info = mailstore.PartInfo{Filename: a.Filename, ContentType: a.ContentType, Size: encodedSize(a.Data)}
	}
	var rd io.Reader = bytes.NewReader(data)
	if s.streamErr != nil {
		rd = io.MultiReader(io.LimitReader(rd, int64(s.streamAfter)), errReader{s.streamErr})
	}
	return info, io.NopCloser(rd), nil
}

// Thread implements mailstore.Store. On Gmail it groups All Mail by
// GmailThreadID. Elsewhere it takes the messages outside Trash and Junk
// whose Message-ID is the root or whose References contain it. Either way r
// is included, and the result is oldest first by date.
func (s *Store) Thread(_ context.Context, r ref.Ref) ([]ref.Ref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin("Thread"); err != nil {
		return nil, err
	}
	seed, err := s.resolveLocked(r)
	if err != nil {
		return nil, err
	}
	root := rootOf(seed)
	members := []*Message{seed}
	for _, m := range s.msgs {
		if m != seed && s.inThreadLocked(seed, root, m) {
			members = append(members, m)
		}
	}
	slices.SortStableFunc(members, func(a, b *Message) int { return dateOf(a).Compare(dateOf(b)) })
	out := make([]ref.Ref, len(members))
	for i, m := range members {
		out[i] = s.refLocked(m)
	}
	return out, nil
}

func (s *Store) inThreadLocked(seed *Message, root string, m *Message) bool {
	role := s.roleLocked(m.Folder)
	if s.gmail {
		return role == mailstore.RoleAll && m.GmailThreadID == seed.GmailThreadID
	}
	if root == "" || role == mailstore.RoleTrash || role == mailstore.RoleSpam {
		return false
	}
	return trimID(m.MessageID) == root || slices.Contains(mailstore.MessageIDs(m.References), root)
}

// Close implements mailstore.Store. It closes every open watch connection
// without waiting for its owner, so a watcher sees ErrUnavailable and
// reconnects through a replacement store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.closeWatchesLocked()
	return nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func (s *Store) refLocked(m *Message) ref.Ref {
	return ref.Ref{Account: s.account, Folder: m.Folder, UIDValidity: s.uidValidity[m.Folder], UID: m.uid, GmailMsgID: m.GmailMsgID}
}

// refInLocked is m's ref in folder. On Gmail a label folder lists m under a
// UID of its own, given the first time a ref there is needed and kept while
// m stays in the folder, as Gmail numbers each folder's messages.
func (s *Store) refInLocked(m *Message, folder string) ref.Ref {
	if !s.gmail || folder == m.Folder {
		return s.refLocked(m)
	}
	uid, ok := m.views[folder]
	if !ok {
		if m.views == nil {
			m.views = map[string]uint32{}
		}
		uid = s.nextUID[folder]
		s.nextUID[folder]++
		m.views[folder] = uid
	}
	return ref.Ref{Account: s.account, Folder: folder, UIDValidity: s.uidValidity[folder], UID: uid, GmailMsgID: m.GmailMsgID}
}

// inNamedLocked reports whether m shows in the folder called name.
func (s *Store) inNamedLocked(name string, m *Message) bool {
	i := slices.IndexFunc(s.folders, func(f mailstore.Folder) bool { return f.Name == name })
	return i >= 0 && s.inFolderLocked(s.folders[i], m)
}

// pruneViewsLocked forgets the label folder UIDs of messages that left
// those folders, so a message that returns gets a new UID there, as on
// Gmail, and an old ref into the folder goes stale.
func (s *Store) pruneViewsLocked() {
	for _, m := range s.msgs {
		maps.DeleteFunc(m.views, func(name string, _ uint32) bool { return !s.inNamedLocked(name, m) })
	}
}

func (s *Store) findLocked(folder string, uid uint32) *Message {
	for _, m := range s.msgs {
		if m.Folder == folder && m.uid == uid {
			return m
		}
	}
	if !s.gmail {
		return nil
	}
	for _, m := range s.msgs {
		if v, ok := m.views[folder]; ok && v == uid && s.inNamedLocked(folder, m) {
			return m
		}
	}
	return nil
}

// resolveLocked finds the message r points at. On Gmail a changed
// UIDVALIDITY is recovered by X-GM-MSGID, as imapstore does.
func (s *Store) resolveLocked(r ref.Ref) (*Message, error) {
	if r.Account != s.account {
		return nil, fmt.Errorf("%w: ref is for account %s", mailstore.ErrNotFound, r.Account)
	}
	v, ok := s.uidValidity[r.Folder]
	if !ok {
		return nil, fmt.Errorf("%w: no folder %q", mailstore.ErrNotFound, r.Folder)
	}
	if v != r.UIDValidity {
		if s.gmail && r.GmailMsgID != 0 {
			for _, m := range s.msgs {
				if m.GmailMsgID == r.GmailMsgID && s.inNamedLocked(r.Folder, m) {
					return m, nil
				}
			}
			return nil, fmt.Errorf("%w: message is gone; search again", mailstore.ErrStaleRef)
		}
		return nil, fmt.Errorf("%w: %s changed; search again", mailstore.ErrStaleRef, r.Folder)
	}
	if m := s.findLocked(r.Folder, r.UID); m != nil {
		return m, nil
	}
	return nil, fmt.Errorf("%w: message is gone; search again", mailstore.ErrStaleRef)
}

func (s *Store) roleLocked(folder string) mailstore.Role {
	for _, f := range s.folders {
		if f.Name == folder {
			return f.Role
		}
	}
	return mailstore.RoleNone
}

func (s *Store) byRoleLocked(role mailstore.Role) string {
	for _, f := range s.folders {
		if f.Selectable && f.Role == role {
			return f.Name
		}
	}
	return ""
}

// sourcesLocked picks the folders a search runs in, as imapstore's
// gmailSources and standardSources do.
func (s *Store) sourcesLocked(in string) ([]string, error) {
	if s.gmail {
		return s.gmailSourcesLocked(in)
	}
	return s.standardSourcesLocked(in)
}

// gmailSourcesLocked searches All Mail, and adds Trash and Spam only for
// in:trash, in:spam and in:anywhere, since they are not part of All Mail.
func (s *Store) gmailSourcesLocked(in string) ([]string, error) {
	want := []mailstore.Role{mailstore.RoleAll}
	switch strings.ToLower(in) {
	case "trash":
		want = []mailstore.Role{mailstore.RoleTrash}
	case "spam", "junk":
		want = []mailstore.Role{mailstore.RoleSpam}
	case "anywhere":
		want = []mailstore.Role{mailstore.RoleAll, mailstore.RoleTrash, mailstore.RoleSpam}
	}
	var out []string
	for _, r := range want {
		if name := s.byRoleLocked(r); name != "" {
			out = append(out, name)
		}
	}
	if slices.Contains(want, mailstore.RoleAll) && s.byRoleLocked(mailstore.RoleAll) == "" {
		return nil, fmt.Errorf("%w: no All Mail folder", mailstore.ErrNotFound)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no folder for in:%s", mailstore.ErrNotFound, in)
	}
	return out, nil
}

// standardSourcesLocked searches every folder but Trash, Junk and \All, or
// the folders in: names by role or by exact name. in:anywhere adds Trash and
// Junk. A \All folder copies every message, so only naming it searches it.
func (s *Store) standardSourcesLocked(in string) ([]string, error) {
	lower := strings.ToLower(in)
	var out []string
	switch lower {
	case "", "anywhere":
		for _, f := range s.folders {
			if !f.Selectable || f.Role == mailstore.RoleAll {
				continue
			}
			if lower == "anywhere" || (f.Role != mailstore.RoleTrash && f.Role != mailstore.RoleSpam) {
				out = append(out, f.Name)
			}
		}
		return out, nil
	case "inbox", "sent", "drafts", "archive", "trash", "spam", "junk":
		role := mailstore.Role(lower)
		if lower == "junk" {
			role = mailstore.RoleSpam
		}
		for _, f := range s.folders {
			if f.Selectable && f.Role == role {
				out = append(out, f.Name)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%w: no folder has the %s role", mailstore.ErrNotFound, role)
		}
		return out, nil
	default:
		for _, f := range s.folders {
			if f.Selectable && f.Name == in {
				return []string{f.Name}, nil
			}
		}
		return nil, fmt.Errorf("%w: no folder named %q", mailstore.ErrNotFound, in)
	}
}

// gmailLabels are the system labels Gmail's in: names. X-GM-LABELS reports
// them with a backslash.
var gmailLabels = map[string]string{
	"inbox": `\Inbox`, "sent": `\Sent`, "drafts": `\Draft`, "starred": `\Starred`, "important": `\Important`,
}

// inFilter is what Gmail's own in: does within a source, where imapstore
// passes it through in X-GM-RAW. in:archive is All Mail without \Inbox, and
// any other name is a label. Standard accounts choose folders instead, so
// every message passes.
func (s *Store) inFilter(in string) func(*Message) bool {
	lower := strings.ToLower(in)
	switch {
	case !s.gmail, lower == "", lower == "trash", lower == "spam", lower == "junk", lower == "anywhere":
		return func(*Message) bool { return true }
	case lower == "archive":
		return func(m *Message) bool { return !hasLabel(m, `\Inbox`) }
	}
	label := in
	if l, ok := gmailLabels[lower]; ok {
		label = l
	}
	return func(m *Message) bool { return hasLabel(m, label) }
}

func hasLabel(m *Message, label string) bool {
	return slices.ContainsFunc(m.Labels, func(l string) bool { return strings.EqualFold(l, label) })
}

// minDate and maxSize are the bounds imapstore sends in IMAP SEARCH: dates
// from 1970 and 32-bit sizes.
var minDate = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

const maxSize = math.MaxUint32

// checkSupported refuses on standard accounts what imapstore's standard
// dialect refuses: the Gmail-only operators, sizes past 32 bits and dates
// before 1970. Gmail takes them all.
func (s *Store) checkSupported(n *query.Node) error {
	if s.gmail {
		return nil
	}
	for _, t := range n.Terms() {
		switch t.Field {
		case query.FieldHas, query.FieldFilename, query.FieldLabel:
			return fmt.Errorf("%w: %s: works on Gmail accounts only until the local index arrives", mailstore.ErrUnsupportedQuery, t.Field)
		case query.FieldLarger, query.FieldSmaller:
			if size, _ := query.ParseSize(t.Value); size > maxSize {
				return fmt.Errorf("%w: %s:%s is past %d bytes", mailstore.ErrUnsupportedQuery, t.Field, t.Value, int64(maxSize))
			}
		case query.FieldAfter, query.FieldBefore, query.FieldNewerThan, query.FieldOlderThan:
			if s.dateBoundLocked(t).Before(minDate) {
				return fmt.Errorf("%w: %s:%s reaches back before 1970-01-01", mailstore.ErrUnsupportedQuery, t.Field, t.Value)
			}
		}
	}
	return nil
}

// dateBoundLocked is the date an after:, before:, newer_than: or older_than:
// term compares the internal date with. On standard accounts an age is
// day-granular, as IMAP SINCE and BEFORE are: newer_than:1d is since the
// start of yesterday. Gmail compares the time itself.
func (s *Store) dateBoundLocked(n *query.Node) time.Time {
	if n.Field == query.FieldAfter || n.Field == query.FieldBefore {
		d, _ := query.ParseDate(n.Value) // the tree was validated
		return d
	}
	now := s.now()
	if !s.gmail {
		y, m, d := now.Date()
		now = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	return ageCutoff(now, n.Value)
}

func (s *Store) match(n *query.Node, m *Message) bool {
	if n == nil {
		return true
	}
	switch n.Op {
	case query.OpAnd:
		for _, c := range n.Children {
			if !s.match(c, m) {
				return false
			}
		}
		return true
	case query.OpOr:
		for _, c := range n.Children {
			if s.match(c, m) {
				return true
			}
		}
		return false
	case query.OpNot:
		return !s.match(n.Children[0], m)
	default:
		return s.matchTerm(n, m)
	}
}

func (s *Store) matchTerm(n *query.Node, m *Message) bool {
	v := strings.ToLower(n.Value)
	contains := func(text string) bool { return strings.Contains(strings.ToLower(text), v) }
	addrs := func(list []mailstore.Address) bool {
		return slices.ContainsFunc(list, func(a mailstore.Address) bool { return contains(a.Name) || contains(a.Email) })
	}
	switch n.Field {
	case query.FieldText:
		return contains(m.Subject) || contains(m.Body)
	case query.FieldFrom:
		return addrs(m.From)
	case query.FieldTo:
		return addrs(m.To)
	case query.FieldCc:
		return addrs(m.Cc)
	case query.FieldBcc:
		// IMAP SEARCH BCC matches the Bcc header, which only a message
		// in Sent or Drafts has.
		return addrs(m.Bcc)
	case query.FieldSubject:
		return contains(m.Subject)
	case query.FieldIs:
		switch v {
		case "unread":
			return !slices.Contains(m.Flags, `\Seen`)
		case "read":
			return slices.Contains(m.Flags, `\Seen`)
		default:
			return slices.Contains(m.Flags, `\Flagged`)
		}
	case query.FieldHas:
		return len(m.Attachments) > 0
	case query.FieldFilename:
		return slices.ContainsFunc(m.Attachments, func(a Attachment) bool { return contains(a.Filename) })
	case query.FieldLabel:
		return hasLabel(m, n.Value)
	case query.FieldAfter, query.FieldBefore, query.FieldNewerThan, query.FieldOlderThan:
		// IMAP SINCE and BEFORE compare the internal date.
		cut := s.dateBoundLocked(n)
		if n.Field == query.FieldAfter || n.Field == query.FieldNewerThan {
			return !m.InternalDate.Before(cut)
		}
		return m.InternalDate.Before(cut)
	case query.FieldLarger, query.FieldSmaller:
		// RFC822.SIZE, the same value as Summary.Size.
		size, _ := query.ParseSize(n.Value)
		raw := int64(len(rawMessage(m, true)))
		if n.Field == query.FieldLarger {
			return raw > size
		}
		return raw < size
	default:
		return false
	}
}

// ageCutoff is now less an age such as 7d. A month or year back from a day
// the target month lacks is that month's last day, as in imapstore.
func ageCutoff(now time.Time, v string) time.Time {
	n, unit, _ := query.ParseAge(v) // the tree was validated
	switch unit {
	case 'w':
		return now.AddDate(0, 0, -7*n)
	case 'm':
		return addMonths(now, -n)
	case 'y':
		return addMonths(now, -12*n)
	default:
		return now.AddDate(0, 0, -n)
	}
}

// addMonths moves t by months, keeping its time of day, and stops at the
// last day of a target month shorter than t's day: 31 March less a month is
// 28 February, where time.AddDate gives 3 March.
func addMonths(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(months), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	if last := first.AddDate(0, 1, -1).Day(); d > last {
		d = last
	}
	return first.AddDate(0, 0, d-1)
}

// summaryLocked summarises m as read through r, its ref in the folder read.
func (s *Store) summaryLocked(m *Message, r ref.Ref, opts mailstore.SummaryOptions) mailstore.Summary {
	sum := mailstore.Summary{
		Ref: r, Date: dateOf(m), InternalDate: m.InternalDate,
		From: slices.Clone(m.From), To: slices.Clone(m.To), Subject: m.Subject,
		Flags: slices.Clone(m.Flags), Size: int64(len(rawMessage(m, true))), HasAttachment: len(m.Attachments) > 0,
		ThreadID: mailstore.ThreadKey(rootOf(m)),
	}
	if s.gmail {
		sum.ThreadID = strconv.FormatUint(m.GmailThreadID, 16)
		sum.Labels = slices.Clone(m.Labels)
	}
	if opts.Snippets {
		data := []byte(m.Body)
		if len(data) > mailstore.SampleBytes {
			data = data[:mailstore.SampleBytes]
		}
		sum.Sample = &mailstore.TextSample{MediaType: "text/plain", Charset: "utf-8", Encoding: "8bit", Data: data}
	}
	return sum
}

// dateOf is the Date header, or the internal date when there is none.
func dateOf(m *Message) time.Time {
	if m.Date.IsZero() {
		return m.InternalDate
	}
	return m.Date
}

func rootOf(m *Message) string {
	return mailstore.RootMessageID(m.References, m.InReplyTo, m.MessageID)
}

// trimID drops white space and angle brackets around a Message-ID.
func trimID(id string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(id), "<"), ">")
}

// encodedSize is the base64 size of data, which is what a server reports
// for an attachment part.
func encodedSize(data []byte) int64 {
	return int64(base64.StdEncoding.EncodedLen(len(data)))
}

func attachments(m *Message) []mailstore.Attachment {
	var out []mailstore.Attachment
	for _, a := range m.Attachments {
		out = append(out, mailstore.Attachment{Part: a.Part, Filename: a.Filename, ContentType: a.ContentType, Size: encodedSize(a.Data)})
	}
	return out
}

func cloneMessage(m Message) Message {
	m.views = nil // a copy is a message of its own
	m.From, m.To, m.Cc, m.Bcc = slices.Clone(m.From), slices.Clone(m.To), slices.Clone(m.Cc), slices.Clone(m.Bcc)
	m.Flags, m.Labels = slices.Clone(m.Flags), slices.Clone(m.Labels)
	m.InReplyTo, m.References = slices.Clone(m.InReplyTo), slices.Clone(m.References)
	m.Attachments = slices.Clone(m.Attachments)
	for i := range m.Attachments {
		m.Attachments[i].Data = slices.Clone(m.Attachments[i].Data)
	}
	return m
}

// rawMessage builds m as MIME. withAttachments adds the attachment parts,
// as the original message would have them. Without them it is what a
// store hands render.Render. An appended message is its raw bytes either way.
func rawMessage(m *Message, withAttachments bool) []byte {
	if m.raw != nil {
		return slices.Clone(m.raw)
	}
	var b bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	addrs := func(list []mailstore.Address) string {
		parts := make([]string, len(list))
		for i, a := range list {
			parts[i] = (&mail.Address{Name: a.Name, Address: a.Email}).String()
		}
		return strings.Join(parts, ", ")
	}
	ids := func(entries []string) string {
		list := mailstore.MessageIDs(entries)
		for i, id := range list {
			list[i] = "<" + id + ">"
		}
		return strings.Join(list, " ")
	}
	if len(m.From) > 0 {
		header("From", addrs(m.From))
	}
	if len(m.To) > 0 {
		header("To", addrs(m.To))
	}
	if len(m.Cc) > 0 {
		header("Cc", addrs(m.Cc))
	}
	if len(m.Bcc) > 0 {
		header("Bcc", addrs(m.Bcc))
	}
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	if !m.Date.IsZero() {
		header("Date", m.Date.Format(time.RFC1123Z))
	}
	if id := trimID(m.MessageID); id != "" {
		header("Message-ID", "<"+id+">")
	}
	if v := ids(m.InReplyTo); v != "" {
		header("In-Reply-To", v)
	}
	if v := ids(m.References); v != "" {
		header("References", v)
	}
	header("MIME-Version", "1.0")
	if !withAttachments || len(m.Attachments) == 0 {
		header("Content-Type", "text/plain; charset=utf-8")
		b.WriteString("\r\n")
		b.WriteString(m.Body)
		return b.Bytes()
	}
	const boundary = "fake-boundary"
	header("Content-Type", `multipart/mixed; boundary="`+boundary+`"`)
	b.WriteString("\r\n--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + m.Body + "\r\n")
	for _, a := range m.Attachments {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s\r\nContent-Disposition: attachment; filename=%q\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n",
			boundary, a.ContentType, a.Filename, base64.StdEncoding.EncodeToString(a.Data))
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes()
}

// mustHaveFolderLocked panics when folder was never added, so a typo in a test
// cannot silently inject nothing.
func (s *Store) mustHaveFolderLocked(folder string) {
	if _, ok := s.uidValidity[folder]; !ok {
		panic(fmt.Sprintf("fake: no folder %q; call AddFolder first", folder))
	}
}
