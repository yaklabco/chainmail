package fakegmail

import (
	"bufio"
	"bytes"
	"fmt"
	"reflect"
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

var (
	_ imapserver.Session            = (*session)(nil)
	_ imapserver.SessionNamespace   = (*session)(nil)
	_ imapserver.SessionMove        = (*session)(nil)
	_ imapserver.SessionAppendLimit = (*session)(nil)
)

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

// List treats every folder as subscribed, as Gmail does, so the SUBSCRIBED,
// REMOTE and RECURSIVEMATCH selection options select everything.
func (sess *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	if len(patterns) == 0 { // imapserver drops the empty pattern of LIST "" ""
		return w.WriteList(&imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Delim: '/'})
	}
	if options.ReturnStatus != nil {
		if err := checkStatusOptions(options.ReturnStatus); err != nil {
			return err
		}
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	match := func(name string) bool {
		return slices.ContainsFunc(patterns, func(p string) bool { return imapserver.MatchList(name, '/', ref, p) })
	}
	subscribed := options.SelectSubscribed || options.ReturnSubscribed
	write := func(data *imap.ListData) error {
		if subscribed {
			data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
		}
		return w.WriteList(data)
	}
	if match("[Gmail]") && !options.SelectSpecialUse {
		if err := write(&imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect, imap.MailboxAttrHasChildren}, Delim: '/', Mailbox: "[Gmail]"}); err != nil {
			return err
		}
	}
	for _, f := range s.folders {
		if !match(f.name) || (options.SelectSpecialUse && len(f.attrs) == 0) {
			continue
		}
		data := &imap.ListData{Attrs: append([]imap.MailboxAttr{imap.MailboxAttrHasNoChildren}, f.attrs...), Delim: '/', Mailbox: f.name}
		if options.ReturnStatus != nil {
			data.Status = statusLocked(f, options.ReturnStatus)
		}
		if err := write(data); err != nil {
			return err
		}
	}
	return nil
}

func (sess *session) Status(name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	if err := checkStatusOptions(options); err != nil {
		return nil, err
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.folderLocked(name)
	if f == nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "Unknown Mailbox: " + name}
	}
	return statusLocked(f, options), nil
}

// checkStatusOptions refuses the STATUS items of extensions the fake does not
// advertise. imapserver would otherwise dereference a nil answer and drop the
// connection.
func checkStatusOptions(options *imap.StatusOptions) error {
	if options.NumDeleted || options.Size || options.DeletedStorage || options.HighestModSeq {
		return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "fakegmail: unsupported STATUS item"}
	}
	return nil
}

func statusLocked(f *folder, options *imap.StatusOptions) *imap.StatusData {
	data := &imap.StatusData{Mailbox: f.name}
	if options.NumMessages {
		n := uint32(len(f.order))
		data.NumMessages = &n
	}
	if options.NumRecent {
		var n uint32 // the fake has no new arrivals
		data.NumRecent = &n
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
	if options.AppendLimit {
		n := uint32(appendLimit)
		data.AppendLimit = &n
	}
	return data
}

// Namespace answers as Gmail does: one personal namespace with no prefix.
func (sess *session) Namespace() (*imap.NamespaceData, error) {
	return &imap.NamespaceData{Personal: []imap.NamespaceDescriptor{{Prefix: "", Delim: '/'}}}, nil
}

// AppendLimit is Gmail's APPENDLIMIT. The fake is read-only all the same.
func (sess *session) AppendLimit() uint32 { return appendLimit }

func (sess *session) Move(*imapserver.MoveWriter, imap.NumSet, string) error { return errReadOnly }

func (sess *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	if kind != imapserver.NumKindUID {
		return nil, &imap.Error{Type: imap.StatusResponseTypeBad, Text: "fakegmail answers UID SEARCH only"}
	}
	if options != nil && options.ReturnSave {
		return nil, errSearchRes
	}
	if name, ok := unsupportedCriterion(criteria, true); ok {
		return nil, &imap.Error{Type: imap.StatusResponseTypeBad, Text: "fakegmail: unsupported criterion " + name}
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := sess.selected
	criteria, err := staticCriteria(f, criteria)
	if err != nil {
		return nil, err
	}
	var trees []*query.Node
	for _, raw := range criteria.GmailRaw {
		s.rawSearches = append(s.rawSearches, RawSearch{Folder: f.name, Raw: raw})
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
	data := &imap.SearchData{}
	for _, m := range f.order {
		if matchCriteria(f, m, criteria) && slices.IndexFunc(trees, func(n *query.Node) bool { return !matchRaw(n, m) }) < 0 {
			uid := f.uids[m]
			all.AddNum(uid)
			data.Count++
			if data.Min == 0 {
				data.Min = uint32(uid)
			}
			data.Max = uint32(uid) // f.order is in UID order
		}
	}
	data.All = all
	return data, nil
}

var errSearchRes = &imap.Error{Type: imap.StatusResponseTypeBad, Text: "fakegmail: SEARCHRES is not advertised"}

// supportedCriteria zeroes every criterion the fake implements. Whatever is
// left is one it would otherwise ignore, which would match everything.
func supportedCriteria(c imap.SearchCriteria, top bool) imap.SearchCriteria {
	c.UID, c.Header, c.Text, c.Flag, c.NotFlag, c.Not, c.Or = nil, nil, nil, nil, nil, nil, nil
	c.GmailMsgID, c.GmailThreadID = nil, nil
	if top {
		c.GmailRaw = nil // X-GM-RAW is evaluated only at the top level
	}
	return c
}

// unsupportedCriterion names the first criterion in c, or nested in its Not
// and Or, that the fake does not implement. It inspects every field, so a
// criterion added to go-imap later is refused until the fake learns it.
func unsupportedCriterion(c *imap.SearchCriteria, top bool) (string, bool) {
	rest := reflect.ValueOf(supportedCriteria(*c, top))
	for i := range rest.NumField() {
		if !rest.Field(i).IsZero() {
			return rest.Type().Field(i).Name, true
		}
	}
	for i := range c.Not {
		if name, ok := unsupportedCriterion(&c.Not[i], false); ok {
			return name, true
		}
	}
	for i := range c.Or {
		for j := range c.Or[i] {
			if name, ok := unsupportedCriterion(&c.Or[i][j], false); ok {
				return name, true
			}
		}
	}
	return "", false
}

// staticCriteria returns a copy of c with every * in its UID sets resolved
// against f, as a server must before matching.
func staticCriteria(f *folder, c *imap.SearchCriteria) (*imap.SearchCriteria, error) {
	out := *c
	out.UID = make([]imap.UIDSet, len(c.UID))
	for i, set := range c.UID {
		static, err := staticUIDSet(f, set)
		if err != nil {
			return nil, err
		}
		out.UID[i] = static
	}
	out.Not = make([]imap.SearchCriteria, len(c.Not))
	for i := range c.Not {
		n, err := staticCriteria(f, &c.Not[i])
		if err != nil {
			return nil, err
		}
		out.Not[i] = *n
	}
	out.Or = make([][2]imap.SearchCriteria, len(c.Or))
	for i := range c.Or {
		for j := range c.Or[i] {
			n, err := staticCriteria(f, &c.Or[i][j])
			if err != nil {
				return nil, err
			}
			out.Or[i][j] = *n
		}
	}
	return &out, nil
}

// staticUIDSet returns a copy of set with * resolved to f's highest UID, as
// imapmemserver does. Without it, Contains reads n:* as "n and above" and a
// bare * as nothing.
func staticUIDSet(f *folder, set imap.UIDSet) (imap.UIDSet, error) {
	if imap.IsSearchRes(set) {
		return nil, errSearchRes
	}
	out := slices.Clone(set)
	for i := range out {
		staticRange(&out[i].Start, &out[i].Stop, f.nextUID-1)
	}
	return out, nil
}

// staticSeqSet is staticUIDSet for sequence numbers.
func staticSeqSet(f *folder, set imap.SeqSet) (imap.SeqSet, error) {
	if imap.IsSearchRes(set) {
		return nil, errSearchRes
	}
	out := slices.Clone(set)
	for i := range out {
		staticRange(&out[i].Start, &out[i].Stop, uint32(len(f.order)))
	}
	return out, nil
}

// staticRange replaces * (0) with highest and orders a range that had one:
// RFC 3501 reads 5:* as 3:5 in a mailbox whose highest number is 3.
func staticRange[T ~uint32](start, stop *T, highest T) {
	dynamic := *start == 0 || *stop == 0
	if *start == 0 {
		*start = highest
	}
	if *stop == 0 {
		*stop = highest
	}
	if dynamic && *start > *stop {
		*start, *stop = *stop, *start
	}
}

func (sess *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	if name, ok := unsupportedFetchItem(options, sess.s.opts); ok {
		return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "fakegmail: unsupported FETCH item " + name}
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := sess.selected
	idx, err := messagesIn(f, numSet)
	if err != nil {
		return err
	}
	for _, i := range idx {
		m := f.order[i]
		if err := writeMessage(w.CreateMessage(uint32(i+1)), m, f.uids[m], options); err != nil {
			return err
		}
	}
	return nil
}

// unsupportedFetchItem names the first FETCH item in options that the fake
// does not answer: BINARY and CONDSTORE items, which Gmail does not
// advertise, and PREVIEW unless Options.Preview is set. Like
// unsupportedCriterion it inspects every field, so a new item is refused
// until the fake learns it.
func unsupportedFetchItem(options *imap.FetchOptions, opts Options) (string, bool) {
	rest := *options
	rest.BodyStructure, rest.BodySection = nil, nil
	rest.Envelope, rest.Flags, rest.InternalDate, rest.RFC822Size, rest.UID = false, false, false, false, false
	rest.GmailMsgID, rest.GmailThreadID, rest.GmailLabels = false, false, false
	if opts.Preview {
		rest.Preview = false
	}
	v := reflect.ValueOf(rest)
	for i := range v.NumField() {
		if !v.Field(i).IsZero() {
			return v.Type().Field(i).Name, true
		}
	}
	return "", false
}

// messagesIn returns the positions in f.order of the messages numSet names.
func messagesIn(f *folder, numSet imap.NumSet) ([]int, error) {
	var contains func(seq uint32, uid imap.UID) bool
	switch set := numSet.(type) {
	case imap.UIDSet:
		static, err := staticUIDSet(f, set)
		if err != nil {
			return nil, err
		}
		contains = func(_ uint32, uid imap.UID) bool { return static.Contains(uid) }
	case imap.SeqSet:
		static, err := staticSeqSet(f, set)
		if err != nil {
			return nil, err
		}
		contains = func(seq uint32, _ imap.UID) bool { return static.Contains(seq) }
	default:
		return nil, &imap.Error{Type: imap.StatusResponseTypeBad, Text: fmt.Sprintf("fakegmail: unexpected message set %T", numSet)}
	}
	var idx []int
	for i, m := range f.order {
		if contains(uint32(i+1), f.uids[m]) {
			idx = append(idx, i)
		}
	}
	return idx, nil
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
