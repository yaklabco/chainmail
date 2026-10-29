// Package fake is an in-memory mailstore.Store for daemon component tests.
// Its query evaluation is simple substring matching: it exists to test the
// daemon's fan-out, merging, cursors and errors, not search semantics.
package fake

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/mail"
	"slices"
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

// Message is a fake message. Add assigns its UID.
type Message struct {
	Folder      string
	Date        time.Time
	From        []mailstore.Address
	To          []mailstore.Address
	Cc          []mailstore.Address
	Subject     string
	Body        string
	Flags       []string
	Labels      []string
	MessageID   string
	InReplyTo   []string
	References  []string
	Attachments []Attachment
	GmailMsgID  uint64

	uid uint32
}

// Store is an in-memory account.
type Store struct {
	mu          sync.Mutex
	account     string
	gmail       bool
	now         func() time.Time
	folders     []mailstore.Folder
	uidValidity map[string]uint32
	nextUID     map[string]uint32
	msgs        []*Message
	fail        map[string]error
	folderFail  map[string]error
	streamErr   error
	streamAfter int
	calls       map[string]int
	closed      bool
}

var _ mailstore.Store = (*Store)(nil)

// New returns an empty account. Gmail accounts get Gmail's folder names and
// accept has:, filename: and label:.
func New(account string, gmail bool) *Store {
	s := &Store{
		account: account, gmail: gmail, now: time.Now,
		uidValidity: map[string]uint32{}, nextUID: map[string]uint32{},
		fail: map[string]error{}, folderFail: map[string]error{}, calls: map[string]int{},
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

// AddFolder adds a selectable folder with UIDVALIDITY 1.
func (s *Store) AddFolder(name string, role mailstore.Role) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.folders = append(s.folders, mailstore.Folder{Name: name, Role: role, Selectable: true})
	s.uidValidity[name] = 1
	s.nextUID[name] = 1
}

// Add stores m and returns its ref. A Gmail message without a GmailMsgID gets one.
func (s *Store) Add(m Message) ref.Ref {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.uid = s.nextUID[m.Folder]
	s.nextUID[m.Folder]++
	if s.gmail && m.GmailMsgID == 0 {
		m.GmailMsgID = uint64(1000 + len(s.msgs))
	}
	if !s.gmail {
		m.GmailMsgID = 0
	}
	s.msgs = append(s.msgs, &m)
	return s.refLocked(&m)
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
	s.uidValidity[folder] = v
}

// Fail makes method (such as "Search" or "FetchMessages") return err. A nil err clears it.
func (s *Store) Fail(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[method] = err
}

// FailFolder makes Search fail in one folder.
func (s *Store) FailFolder(folder string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
			if m.Folder != out[i].Name {
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
	names, err := s.sourcesLocked(req.In)
	if err != nil {
		return nil, err
	}
	if req.Bounds != nil {
		names = slices.Sorted(maps.Keys(req.Bounds))
	}
	var out []mailstore.SourceResult
	for _, name := range names {
		res := mailstore.SourceResult{Source: mailstore.Source{Folder: name, UIDValidity: s.uidValidity[name]}}
		bound, continuing := req.Bounds[name]
		switch {
		case s.folderFail[name] != nil:
			res.Err = s.folderFail[name]
		case continuing && bound.UIDValidity != res.Source.UIDValidity:
			res.Err = fmt.Errorf("%w: %s changed since the first page", mailstore.ErrStaleRef, name)
		default:
			for i := len(s.msgs) - 1; i >= 0; i-- {
				m := s.msgs[i]
				if m.Folder != name || (continuing && bound.Below > 0 && m.uid >= bound.Below) || !s.match(req.Query, m) {
					continue
				}
				res.UIDs = append(res.UIDs, m.uid)
			}
			slices.SortFunc(res.UIDs, func(a, b uint32) int { return int(b) - int(a) })
			res.Count = uint32(len(res.UIDs))
			if req.CountOnly {
				res.UIDs = nil
			}
		}
		out = append(out, res)
	}
	return out, nil
}

// FetchSummaries implements mailstore.Store.
func (s *Store) FetchSummaries(_ context.Context, src mailstore.Source, uids []uint32, opts mailstore.SummaryOptions) ([]mailstore.Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin("FetchSummaries"); err != nil {
		return nil, err
	}
	if s.uidValidity[src.Folder] != src.UIDValidity {
		return nil, fmt.Errorf("%w: %s changed", mailstore.ErrStaleRef, src.Folder)
	}
	var out []mailstore.Summary
	for _, uid := range uids {
		if m := s.findLocked(src.Folder, uid); m != nil {
			out = append(out, s.summaryLocked(m, opts))
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
			Summary: s.summaryLocked(m, mailstore.SummaryOptions{}), Cc: m.Cc, MessageID: m.MessageID,
			InReplyTo: m.InReplyTo, References: m.References, Raw: rawMessage(m, false), Attachments: attachments(m),
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
		info = mailstore.PartInfo{Filename: a.Filename, ContentType: a.ContentType, Size: int64(len(a.Data))}
	}
	var rd io.Reader = bytes.NewReader(data)
	if s.streamErr != nil {
		rd = io.MultiReader(io.LimitReader(rd, int64(s.streamAfter)), errReader{s.streamErr})
	}
	return info, io.NopCloser(rd), nil
}

// Thread implements mailstore.Store.
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
	root := mailstore.RootMessageID(seed.References, seed.InReplyTo, seed.MessageID)
	out := []ref.Ref{s.refLocked(seed)}
	for _, m := range s.msgs {
		if m == seed || root == "" || s.roleLocked(m.Folder) == mailstore.RoleTrash || s.roleLocked(m.Folder) == mailstore.RoleSpam {
			continue
		}
		if mailstore.RootMessageID(m.References, m.InReplyTo, m.MessageID) == root {
			out = append(out, s.refLocked(m))
		}
	}
	return out, nil
}

// Close implements mailstore.Store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func (s *Store) refLocked(m *Message) ref.Ref {
	return ref.Ref{Account: s.account, Folder: m.Folder, UIDValidity: s.uidValidity[m.Folder], UID: m.uid, GmailMsgID: m.GmailMsgID}
}

func (s *Store) findLocked(folder string, uid uint32) *Message {
	for _, m := range s.msgs {
		if m.Folder == folder && m.uid == uid {
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
				if m.Folder == r.Folder && m.GmailMsgID == r.GmailMsgID {
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

func (s *Store) sourcesLocked(in string) ([]string, error) {
	var out []string
	lower := strings.ToLower(in)
	for _, f := range s.folders {
		switch {
		case lower == "" && s.gmail:
			if f.Role == mailstore.RoleAll {
				out = append(out, f.Name)
			}
		case lower == "":
			if f.Role != mailstore.RoleTrash && f.Role != mailstore.RoleSpam {
				out = append(out, f.Name)
			}
		case lower == "anywhere":
			if !s.gmail || f.Role == mailstore.RoleAll || f.Role == mailstore.RoleTrash || f.Role == mailstore.RoleSpam {
				out = append(out, f.Name)
			}
		case string(f.Role) == lower || (lower == "junk" && f.Role == mailstore.RoleSpam) || f.Name == in:
			out = append(out, f.Name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no folder for in:%s", mailstore.ErrNotFound, in)
	}
	return out, nil
}

func (s *Store) checkSupported(n *query.Node) error {
	if s.gmail {
		return nil
	}
	for _, t := range n.Terms() {
		switch t.Field {
		case query.FieldHas, query.FieldFilename, query.FieldLabel:
			return fmt.Errorf("%w: %s: works on Gmail accounts only until the local index arrives", mailstore.ErrUnsupportedQuery, t.Field)
		}
	}
	return nil
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
		return slices.ContainsFunc(m.Labels, func(l string) bool { return strings.EqualFold(l, n.Value) })
	case query.FieldAfter, query.FieldBefore:
		d, _ := query.ParseDate(n.Value) // the tree was validated
		if n.Field == query.FieldAfter {
			return !m.Date.Before(d)
		}
		return m.Date.Before(d)
	case query.FieldNewerThan, query.FieldOlderThan:
		cut := ageCutoff(s.now(), n.Value)
		if n.Field == query.FieldNewerThan {
			return !m.Date.Before(cut)
		}
		return m.Date.Before(cut)
	case query.FieldLarger, query.FieldSmaller:
		size, _ := query.ParseSize(n.Value)
		if n.Field == query.FieldLarger {
			return int64(len(m.Body)) > size
		}
		return int64(len(m.Body)) < size
	default:
		return false
	}
}

func ageCutoff(now time.Time, v string) time.Time {
	n, unit, _ := query.ParseAge(v) // the tree was validated
	switch unit {
	case 'w':
		return now.AddDate(0, 0, -7*n)
	case 'm':
		return now.AddDate(0, -n, 0)
	case 'y':
		return now.AddDate(-n, 0, 0)
	default:
		return now.AddDate(0, 0, -n)
	}
}

func (s *Store) summaryLocked(m *Message, opts mailstore.SummaryOptions) mailstore.Summary {
	sum := mailstore.Summary{
		Ref: s.refLocked(m), Date: m.Date, InternalDate: m.Date, From: m.From, To: m.To, Subject: m.Subject,
		Flags: slices.Clone(m.Flags), Size: int64(len(rawMessage(m, true))), HasAttachment: len(m.Attachments) > 0,
		ThreadID: mailstore.ThreadKey(mailstore.RootMessageID(m.References, m.InReplyTo, m.MessageID)),
	}
	if s.gmail {
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

func attachments(m *Message) []mailstore.Attachment {
	var out []mailstore.Attachment
	for _, a := range m.Attachments {
		out = append(out, mailstore.Attachment{Part: a.Part, Filename: a.Filename, ContentType: a.ContentType, Size: int64(len(a.Data))})
	}
	return out
}

// rawMessage builds m as MIME. withAttachments adds the attachment parts,
// as the original message would have them. Without them it is what a
// store hands render.Render.
func rawMessage(m *Message, withAttachments bool) []byte {
	var b bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	addrs := func(list []mailstore.Address) string {
		parts := make([]string, len(list))
		for i, a := range list {
			parts[i] = (&mail.Address{Name: a.Name, Address: a.Email}).String()
		}
		return strings.Join(parts, ", ")
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
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	if !m.Date.IsZero() {
		header("Date", m.Date.Format(time.RFC1123Z))
	}
	if m.MessageID != "" {
		header("Message-ID", "<"+m.MessageID+">")
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
