package fakegmail

import (
	"bufio"
	"bytes"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"

	"github.com/jamesainslie/chainmail/internal/query"
)

var errReadOnly = &imap.Error{Type: imap.StatusResponseTypeNo, Text: "fakegmail is read-only"}

type session struct {
	s        *Server
	selected *folder
}

var _ imapserver.Session = (*session)(nil)

func (sess *session) Close() error { return nil }

func (sess *session) Login(username, password string) error {
	if username != sess.s.username || password != sess.s.password {
		return imapserver.ErrAuthFailed
	}
	sess.s.mu.Lock()
	sess.s.logins++
	sess.s.mu.Unlock()
	return nil
}

func (sess *session) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.folderLocked(name)
	if f == nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "Unknown Mailbox: " + name}
	}
	if options == nil || !options.ReadOnly {
		s.writableSelects++
	}
	sess.selected = f
	return &imap.SelectData{
		Flags:       []imap.Flag{imap.FlagSeen, imap.FlagFlagged, imap.FlagAnswered, imap.FlagDraft, imap.FlagDeleted},
		NumMessages: uint32(len(f.order)), UIDNext: f.nextUID, UIDValidity: f.uidValidity,
	}, nil
}

func (sess *session) List(w *imapserver.ListWriter, _ string, patterns []string, _ *imap.ListOptions) error {
	if slices.Contains(patterns, "") {
		return w.WriteList(&imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Delim: '/'})
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := w.WriteList(&imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect, imap.MailboxAttrHasChildren}, Delim: '/', Mailbox: "[Gmail]"}); err != nil {
		return err
	}
	for _, f := range s.folders {
		if err := w.WriteList(&imap.ListData{Attrs: append([]imap.MailboxAttr{imap.MailboxAttrHasNoChildren}, f.attrs...), Delim: '/', Mailbox: f.name}); err != nil {
			return err
		}
	}
	return nil
}

func (sess *session) Status(name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.folderLocked(name)
	if f == nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "Unknown Mailbox: " + name}
	}
	data := &imap.StatusData{Mailbox: f.name}
	if options.NumMessages {
		n := uint32(len(f.order))
		data.NumMessages = &n
	}
	if options.NumUnseen {
		var n uint32
		for _, m := range f.order {
			if !slices.Contains(m.Flags, imap.FlagSeen) {
				n++
			}
		}
		data.NumUnseen = &n
	}
	if options.UIDNext {
		data.UIDNext = f.nextUID
	}
	if options.UIDValidity {
		data.UIDValidity = f.uidValidity
	}
	return data, nil
}

func (sess *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	if kind != imapserver.NumKindUID {
		return nil, &imap.Error{Type: imap.StatusResponseTypeBad, Text: "fakegmail answers UID SEARCH only"}
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := sess.selected
	var trees []*query.Node
	for _, raw := range criteria.GmailRaw {
		s.lastRaw = append(s.lastRaw, raw)
		n, err := query.Parse(gmailDateRE.ReplaceAllString(raw, "$1:$2-$3-$4"))
		if err != nil {
			return nil, &imap.Error{Type: imap.StatusResponseTypeBad, Text: "X-GM-RAW: " + err.Error()}
		}
		if n == nil {
			continue // an empty query adds no condition
		}
		trees = append(trees, n)
	}
	var all imap.UIDSet
	var count uint32
	for _, m := range f.order {
		if matchCriteria(f, m, criteria) && slices.IndexFunc(trees, func(n *query.Node) bool { return !matchRaw(n, m) }) < 0 {
			all.AddNum(f.uids[m])
			count++
		}
	}
	data := &imap.SearchData{All: all}
	if options != nil && options.ReturnCount {
		data.Count = count
	}
	return data, nil
}

func (sess *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := sess.selected
	for i, m := range f.order {
		seq, uid := uint32(i+1), f.uids[m]
		switch set := numSet.(type) {
		case imap.UIDSet:
			if !set.Contains(uid) {
				continue
			}
		case imap.SeqSet:
			if !set.Contains(seq) {
				continue
			}
		}
		if err := writeMessage(w.CreateMessage(seq), m, uid, options); err != nil {
			return err
		}
	}
	return nil
}

func writeMessage(rw *imapserver.FetchResponseWriter, m *Message, uid imap.UID, options *imap.FetchOptions) error {
	raw := []byte(m.Raw)
	rw.WriteUID(uid)
	if options.Flags {
		rw.WriteFlags(m.Flags)
	}
	if options.InternalDate {
		rw.WriteInternalDate(m.Date)
	}
	if options.RFC822Size {
		rw.WriteRFC822Size(int64(len(raw)))
	}
	if options.Envelope {
		if h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw))); err == nil {
			rw.WriteEnvelope(imapserver.ExtractEnvelope(h))
		}
	}
	if options.BodyStructure != nil {
		rw.WriteBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(raw)))
	}
	if options.GmailMsgID {
		rw.WriteGmailMsgID(m.msgID)
	}
	if options.GmailThreadID {
		rw.WriteGmailThreadID(m.threadID)
	}
	if options.GmailLabels {
		rw.WriteGmailLabels(m.Labels)
	}
	if options.Preview {
		rw.WritePreview(m.Preview)
	}
	for _, bs := range options.BodySection {
		buf := imapserver.ExtractBodySection(bytes.NewReader(raw), bs)
		wc := rw.WriteBodySection(bs, int64(len(buf)))
		_, werr := wc.Write(buf)
		if cerr := wc.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
		if !bs.Peek && !slices.Contains(m.Flags, imap.FlagSeen) {
			m.Flags = append(m.Flags, imap.FlagSeen)
		}
	}
	return rw.Close()
}

// gmailDateRE finds Gmail's YYYY/MM/DD dates, which the query parser reads as YYYY-MM-DD.
var gmailDateRE = regexp.MustCompile(`\b(after|before):([0-9]{4})/([0-9]{2})/([0-9]{2})\b`)

func header(m *Message, key string) string {
	h, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(m.Raw)))
	if err != nil {
		return ""
	}
	return strings.Join(h.Values(key), ", ")
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func matchCriteria(f *folder, m *Message, c *imap.SearchCriteria) bool {
	for _, set := range c.UID {
		if !set.Contains(f.uids[m]) {
			return false
		}
	}
	for _, id := range c.GmailMsgID {
		if m.msgID != id {
			return false
		}
	}
	for _, id := range c.GmailThreadID {
		if m.threadID != id {
			return false
		}
	}
	for _, h := range c.Header {
		if !containsFold(header(m, h.Key), h.Value) {
			return false
		}
	}
	for _, t := range c.Text {
		if !containsFold(m.Raw, t) {
			return false
		}
	}
	for _, fl := range c.Flag {
		if !slices.Contains(m.Flags, fl) {
			return false
		}
	}
	for _, fl := range c.NotFlag {
		if slices.Contains(m.Flags, fl) {
			return false
		}
	}
	for i := range c.Not {
		if matchCriteria(f, m, &c.Not[i]) {
			return false
		}
	}
	for i := range c.Or {
		if !matchCriteria(f, m, &c.Or[i][0]) && !matchCriteria(f, m, &c.Or[i][1]) {
			return false
		}
	}
	return true
}

// matchRaw evaluates an X-GM-RAW tree with simple substring rules.
func matchRaw(n *query.Node, m *Message) bool {
	switch n.Op {
	case query.OpAnd:
		for _, c := range n.Children {
			if !matchRaw(c, m) {
				return false
			}
		}
		return true
	case query.OpOr:
		for _, c := range n.Children {
			if matchRaw(c, m) {
				return true
			}
		}
		return false
	case query.OpNot:
		return !matchRaw(n.Children[0], m)
	}
	v := n.Value
	switch n.Field {
	case query.FieldText:
		return containsFold(m.Raw, v)
	case query.FieldFrom, query.FieldTo, query.FieldCc, query.FieldBcc, query.FieldSubject:
		return containsFold(header(m, string(n.Field)), v)
	case query.FieldLabel:
		return hasLabel(m, v)
	case query.FieldIn:
		switch strings.ToLower(v) {
		case "inbox":
			return hasLabel(m, `\Inbox`)
		case "sent":
			return hasLabel(m, `\Sent`)
		case "drafts":
			return hasLabel(m, `\Draft`)
		default:
			return hasLabel(m, v)
		}
	case query.FieldHas:
		return containsFold(m.Raw, "content-disposition: attachment")
	case query.FieldFilename:
		return containsFold(m.Raw, "filename") && containsFold(m.Raw, v)
	case query.FieldIs:
		switch strings.ToLower(v) {
		case "unread":
			return !slices.Contains(m.Flags, imap.FlagSeen)
		case "read":
			return slices.Contains(m.Flags, imap.FlagSeen)
		default:
			return slices.Contains(m.Flags, imap.FlagFlagged)
		}
	case query.FieldAfter, query.FieldBefore:
		d, _ := query.ParseDate(v) // parsed already
		if n.Field == query.FieldAfter {
			return !m.Date.Before(d)
		}
		return m.Date.Before(d)
	case query.FieldNewerThan, query.FieldOlderThan:
		days, unit, _ := query.ParseAge(v) // parsed already
		mult := map[byte]int{'d': 1, 'w': 7, 'm': 30, 'y': 365}[unit]
		cut := time.Now().AddDate(0, 0, -days*mult)
		if n.Field == query.FieldNewerThan {
			return !m.Date.Before(cut)
		}
		return m.Date.Before(cut)
	case query.FieldLarger, query.FieldSmaller:
		size, _ := query.ParseSize(v) // parsed already
		if n.Field == query.FieldLarger {
			return int64(len(m.Raw)) > size
		}
		return int64(len(m.Raw)) < size
	default:
		return false
	}
}

func (sess *session) Unselect() error {
	sess.selected = nil
	return nil
}

func (sess *session) Poll(*imapserver.UpdateWriter, bool) error { return nil }

func (sess *session) Idle(_ *imapserver.UpdateWriter, stop <-chan struct{}) error {
	<-stop
	return nil
}

func (sess *session) Create(string, *imap.CreateOptions) error         { return errReadOnly }
func (sess *session) Delete(string) error                              { return errReadOnly }
func (sess *session) Rename(string, string, *imap.RenameOptions) error { return errReadOnly }
func (sess *session) Subscribe(string) error                           { return errReadOnly }
func (sess *session) Unsubscribe(string) error                         { return errReadOnly }
func (sess *session) Expunge(*imapserver.ExpungeWriter, *imap.UIDSet) error {
	return errReadOnly
}

func (sess *session) Append(string, imap.LiteralReader, *imap.AppendOptions) (*imap.AppendData, error) {
	return nil, errReadOnly
}

func (sess *session) Store(*imapserver.FetchWriter, imap.NumSet, *imap.StoreFlags, *imap.StoreOptions) error {
	return errReadOnly
}

func (sess *session) Copy(imap.NumSet, string) (*imap.CopyData, error) { return nil, errReadOnly }
