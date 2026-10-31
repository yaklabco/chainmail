package fakegmail

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
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

// errExamined answers a write in a folder opened with EXAMINE.
var errExamined = &imap.Error{Type: imap.StatusResponseTypeNo, Text: "fakegmail: the folder is open read-only"}

type session struct {
	s        *Server
	selected *folder
	readOnly bool
	// view is the selected folder as this session's client knows it, in
	// sequence-number order. Entries whose message has left the folder stay
	// until an EXPUNGE is sent for them.
	view []viewEntry
	// known counts the entries the client has heard of through EXISTS.
	known int
	// lastUID is the highest UID in view, so new arrivals are those above it.
	lastUID imap.UID
	// quiet marks a command ending that leaves new arrivals unlearned, under
	// Options.LateArrivals: see endQuiet.
	quiet bool
	// due, under Options.LateExists, is the highest UID of the arrivals a
	// command ending has made due, for the end of the next command to tell
	// of, or 0.
	due imap.UID
}

type viewEntry struct {
	m   *Message
	uid imap.UID
}

var (
	_ imapserver.Session            = (*session)(nil)
	_ imapserver.SessionNamespace   = (*session)(nil)
	_ imapserver.SessionMove        = (*session)(nil)
	_ imapserver.SessionAppendLimit = (*session)(nil)
	_ imapserver.SessionGmail       = (*session)(nil)
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

// permanentFlags is what Gmail answers to SELECT.
var permanentFlags = []imap.Flag{
	imap.FlagAnswered, imap.FlagFlagged, imap.FlagDraft, imap.FlagDeleted, imap.FlagSeen, "$NotPhishing", "$Phishing", imap.FlagWildcard,
}

func (sess *session) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	command := "SELECT"
	if options != nil && options.ReadOnly {
		command = "EXAMINE"
	}
	if err := s.faultLocked(command); err != nil {
		// A failed SELECT or EXAMINE leaves no folder selected.
		sess.selected, sess.view = nil, nil
		return nil, err
	}
	f := s.folderLocked(name)
	if f == nil {
		return nil, noSuchFolder(name)
	}
	sess.readOnly = options != nil && options.ReadOnly
	var perm []imap.Flag
	if !sess.readOnly {
		s.writableSelects++
		perm = permanentFlags
	}
	sess.selected = f
	sess.view, sess.lastUID = nil, 0
	// Under Options.LateExists the Poll that ends the SELECT makes nothing due.
	sess.quiet, sess.due = s.opts.LateExists, 0
	sess.addNewLocked()
	sess.known = len(sess.view)
	return &imap.SelectData{
		Flags:          []imap.Flag{imap.FlagSeen, imap.FlagFlagged, imap.FlagAnswered, imap.FlagDraft, imap.FlagDeleted},
		PermanentFlags: perm,
		NumMessages:    uint32(len(f.order)), UIDNext: f.nextUID, UIDValidity: f.uidValidity,
	}, nil
}

func noSuchFolder(name string) error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "Unknown Mailbox: " + name}
}

// addNewLocked appends the selected folder's new arrivals to the view. It
// runs before every command, as Gmail and Dovecot sync before answering, so a
// UID command can name a message another session just added. The client hears
// of them through EXISTS when the command ends.
func (sess *session) addNewLocked() { sess.addUpToLocked(math.MaxUint32) }

// addUpToLocked is addNewLocked for the arrivals with UIDs up to limit.
func (sess *session) addUpToLocked(limit imap.UID) {
	f := sess.selected
	for _, m := range f.order {
		if uid := f.uids[m]; uid > sess.lastUID && uid <= limit {
			sess.view = append(sess.view, viewEntry{m: m, uid: uid})
			sess.lastUID = uid
		}
	}
}

// newestLocked returns the highest UID in the selected folder, or 0.
func (sess *session) newestLocked() imap.UID {
	f := sess.selected
	var newest imap.UID
	for _, m := range f.order {
		newest = max(newest, f.uids[m])
	}
	return newest
}

// syncLocked runs addNewLocked before a command, unless Options.LateArrivals
// or Options.LateExists leaves new arrivals for the session to learn of
// later.
func (sess *session) syncLocked() {
	if !sess.s.opts.late() {
		sess.addNewLocked()
	}
}

// endQuiet, deferred by FETCH, STORE and SEARCH, keeps a session under
// Options.LateArrivals or Options.LateExists from learning of new arrivals
// in the Poll that ends the command, when the command succeeded and so Poll
// runs, unless they are due.
func (sess *session) endQuiet(err *error) {
	sess.quiet = *err == nil && sess.s.opts.late()
}

// present reports whether the entry's message is still in the folder under
// the same UID.
func (sess *session) present(e viewEntry) bool {
	uid, ok := sess.selected.uids[e.m]
	return ok && uid == e.uid
}

// expungeGoneLocked drops view entries whose message has left the folder,
// writing an EXPUNGE for each the client knows of, highest first so the
// sequence numbers stay valid.
func (sess *session) expungeGoneLocked(write func(seqNum uint32) error) error {
	for i := len(sess.view) - 1; i >= 0; i-- {
		if sess.present(sess.view[i]) {
			continue
		}
		if i < sess.known {
			if err := write(uint32(i + 1)); err != nil {
				return err
			}
			sess.known--
		}
		sess.view = slices.Delete(sess.view, i, i+1)
	}
	return nil
}

// writable checks the session may change the selected folder.
func (sess *session) writable() error {
	if sess.readOnly {
		return errExamined
	}
	return nil
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
		return nil, noSuchFolder(name)
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

// AppendLimit is Gmail's APPENDLIMIT.
func (sess *session) AppendLimit() uint32 { return appendLimit }

func (sess *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (_ *imap.SearchData, err error) {
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
	sess.syncLocked()
	defer sess.endQuiet(&err)
	criteria, err = staticCriteria(f, criteria)
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
		if s.opts.late() && f.uids[m] > sess.lastUID {
			break // an arrival the session has not learned of, and the rest are later
		}
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

// staticUIDSet returns a copy of set with * resolved to f.highestUID, as
// imapmemserver does. Without it, Contains reads n:* as "n and above" and a
// bare * as nothing.
func staticUIDSet(f *folder, set imap.UIDSet) (imap.UIDSet, error) {
	if imap.IsSearchRes(set) {
		return nil, errSearchRes
	}
	out := slices.Clone(set)
	for i := range out {
		staticRange(&out[i].Start, &out[i].Stop, f.highestUID())
	}
	return out, nil
}

// staticSeqSet is staticUIDSet for sequence numbers.
func staticSeqSet(highest uint32, set imap.SeqSet) (imap.SeqSet, error) {
	if imap.IsSearchRes(set) {
		return nil, errSearchRes
	}
	out := slices.Clone(set)
	for i := range out {
		staticRange(&out[i].Start, &out[i].Stop, highest)
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

func (sess *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) (err error) {
	if name, ok := unsupportedFetchItem(options, sess.s.opts); ok {
		return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "fakegmail: unsupported FETCH item " + name}
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.faultLocked("FETCH"); err != nil {
		return err
	}
	f := sess.selected
	sess.syncLocked()
	defer sess.endQuiet(&err)
	idx, err := sess.messagesIn(numSet)
	if err != nil {
		return err
	}
	for _, i := range idx {
		e := sess.view[i]
		if err := writeMessage(w.CreateMessage(uint32(i+1)), f, e.m, e.uid, options); err != nil {
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

// messagesIn returns the positions in the view of the messages numSet names
// that are still in the folder. Sequence numbers count every entry the
// client knows of, as IMAP requires until an EXPUNGE is sent, and reach only
// as far as the last EXISTS: a sequence set never names an arrival the client
// has not been told of.
func (sess *session) messagesIn(numSet imap.NumSet) ([]int, error) {
	f := sess.selected
	var contains func(seq uint32, uid imap.UID) bool
	switch set := numSet.(type) {
	case imap.UIDSet:
		static, err := staticUIDSet(f, set)
		if err != nil {
			return nil, err
		}
		contains = func(_ uint32, uid imap.UID) bool { return static.Contains(uid) }
	case imap.SeqSet:
		known := uint32(sess.known)
		static, err := staticSeqSet(known, set)
		if err != nil {
			return nil, err
		}
		contains = func(seq uint32, _ imap.UID) bool { return seq <= known && static.Contains(seq) }
	default:
		return nil, &imap.Error{Type: imap.StatusResponseTypeBad, Text: fmt.Sprintf("fakegmail: unexpected message set %T", numSet)}
	}
	var idx []int
	for i, e := range sess.view {
		if sess.present(e) && contains(uint32(i+1), e.uid) {
			idx = append(idx, i)
		}
	}
	return idx, nil
}

func writeMessage(rw *imapserver.FetchResponseWriter, f *folder, m *Message, uid imap.UID, options *imap.FetchOptions) error {
	raw := []byte(m.Raw)
	rw.WriteUID(uid)
	if options.Flags {
		rw.WriteFlags(f.flags(m))
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
		if !slices.Contains(f.flags(m), fl) {
			return false
		}
	}
	for _, fl := range c.NotFlag {
		if slices.Contains(f.flags(m), fl) {
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
		switch {
		case strings.EqualFold(v, "inbox"):
			return hasLabel(m, `\Inbox`)
		case strings.EqualFold(v, "sent"):
			return hasLabel(m, `\Sent`)
		case strings.EqualFold(v, "drafts"):
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
	sess.selected, sess.readOnly, sess.view, sess.known, sess.lastUID = nil, false, nil, 0, 0
	return nil
}

// Poll tells the client what changed in the selected folder since its last
// command: EXPUNGE for messages that left, when allowed, and EXISTS for new
// arrivals. Under Options.LateExists a command other than FETCH, STORE and
// SEARCH only makes new arrivals due, and the next command tells of them.
func (sess *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.selected == nil {
		return nil
	}
	if sess.due > 0 {
		sess.addUpToLocked(sess.due)
		sess.due = 0
	}
	switch {
	case sess.quiet:
	case s.opts.LateExists:
		if newest := sess.newestLocked(); newest > sess.lastUID {
			sess.due = newest
		}
	default:
		sess.addNewLocked()
	}
	sess.quiet = false
	if allowExpunge {
		if err := sess.expungeGoneLocked(w.WriteExpunge); err != nil {
			return err
		}
	}
	if len(sess.view) == sess.known {
		return nil
	}
	sess.known = len(sess.view)
	return w.WriteNumMessages(uint32(sess.known))
}

func (sess *session) Idle(_ *imapserver.UpdateWriter, stop <-chan struct{}) error {
	<-stop
	return nil
}

// Subscribe and Unsubscribe succeed and change nothing: Gmail treats every
// folder as subscribed.
func (sess *session) Subscribe(string) error   { return nil }
func (sess *session) Unsubscribe(string) error { return nil }

// checkUserLabel refuses names that are not user labels: INBOX, the [Gmail]
// folders and system labels.
func checkUserLabel(name string) error {
	const prefix = "[Gmail]/"
	_, under := cutFoldPrefix(name, prefix)
	gmail := strings.EqualFold(name, "[Gmail]") || under
	if name == "" || gmail || strings.EqualFold(name, "INBOX") || strings.HasPrefix(name, `\`) {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "fakegmail: " + name + " is not a user label"}
	}
	return nil
}

func folderExists(name string) error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlreadyExists, Text: "Duplicate folder name " + name}
}

func tryCreate(name string) error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "Unknown Mailbox: " + name}
}

// Create makes a user label. Gmail matches label names without regard to
// case, so Receipts and receipts are the same label.
func (sess *session) Create(name string, _ *imap.CreateOptions) error {
	name = strings.TrimSuffix(name, "/")
	if err := checkUserLabel(name); err != nil {
		return err
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.folderFoldLocked(name) != nil {
		return folderExists(name)
	}
	s.addUserFolderLocked(name)
	return nil
}

// beneath reports whether name is below parent in the label hierarchy,
// ignoring case as Gmail does.
func beneath(name, parent string) bool {
	_, ok := cutFoldPrefix(name, parent+"/")
	return ok
}

// Rename renames a user label and the labels under it, on every message that
// carries them. UIDs and UIDVALIDITY stay. A label cannot move beneath itself.
func (sess *session) Rename(name, newName string, _ *imap.RenameOptions) error {
	if err := checkUserLabel(name); err != nil {
		return err
	}
	if err := checkUserLabel(newName); err != nil {
		return err
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.folderFoldLocked(name)
	if f == nil {
		return noSuchFolder(name)
	}
	if beneath(newName, f.name) {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "fakegmail: cannot move " + f.name + " beneath itself"}
	}
	renames := map[*folder]string{}
	byLabel := map[string]string{} // old label, folded, to new label
	for _, g := range s.folders {
		switch {
		case g.kind != kindLabel || strings.HasPrefix(g.label, `\`):
			continue
		case g == f:
			renames[g] = newName
		case beneath(g.name, f.name):
			rest, _ := cutFoldPrefix(g.name, f.name)
			renames[g] = newName + rest
		default:
			continue
		}
		byLabel[foldKey(g.label)] = renames[g]
	}
	for _, to := range renames {
		if other := s.folderFoldLocked(to); other != nil && renames[other] == "" {
			return folderExists(to)
		}
	}
	// Each label is looked up once against the old names, so no label is
	// renamed twice whatever order the folders come in. A message that carried
	// two spellings of one label, such as σ and ς, keeps one.
	for _, m := range s.msgs {
		seen := make(map[string]bool, len(m.Labels))
		labels := m.Labels[:0]
		for _, l := range m.Labels {
			if to, ok := byLabel[foldKey(l)]; ok {
				l = to
			}
			if k := foldKey(l); !seen[k] {
				seen[k] = true
				labels = append(labels, l)
			}
		}
		m.Labels = labels
	}
	for g, to := range renames {
		g.name, g.label = to, to
	}
	return nil
}

// Delete removes a user label from every message and drops its folder. The
// messages stay in All Mail.
func (sess *session) Delete(name string) error {
	if err := checkUserLabel(name); err != nil {
		return err
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.folderFoldLocked(name)
	if f == nil {
		return noSuchFolder(name)
	}
	for _, m := range s.msgs {
		removeLabel(m, f.label)
	}
	s.folders = slices.DeleteFunc(s.folders, func(g *folder) bool { return g == f })
	return nil
}

// Append adds a message to a folder as Gmail does: to a label folder with
// that label, so APPEND to Sent Mail or Drafts gives \Sent or \Draft. Once
// it has stored the message it runs the AfterAppend hook.
func (sess *session) Append(name string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if options == nil {
		options = &imap.AppendOptions{}
	}
	data, hook, err := sess.appendMessage(name, raw, options)
	if err == nil && hook != nil {
		hook()
	}
	return data, err
}

// appendMessage stores an APPEND's message and returns the AfterAppend hook.
func (sess *session) appendMessage(name string, raw []byte, options *imap.AppendOptions) (*imap.AppendData, func(), error) {
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.folderLocked(name)
	if f == nil {
		return nil, nil, tryCreate(name)
	}
	date := options.Time
	if date.IsZero() {
		date = time.Now()
	}
	m := s.newMessageLocked(string(raw), date, nil)
	fileLocked(m, f)
	uid := f.ensure(m)
	storeFlags(f, m, imap.StoreFlagsSet, options.Flags)
	s.refreshLocked()
	s.appends++
	return &imap.AppendData{UID: uid, UIDValidity: f.uidValidity}, s.afterAppend, nil
}

// Store changes flags. A set that names no current message answers OK with
// no FETCH, as Gmail does for a stale UID.
func (sess *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, _ *imap.StoreOptions) (err error) {
	if err := sess.writable(); err != nil {
		return err
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := sess.selected
	sess.syncLocked()
	defer sess.endQuiet(&err)
	idx, err := sess.messagesIn(numSet)
	if err != nil {
		return err
	}
	for _, i := range idx {
		storeFlags(f, sess.view[i].m, flags.Op, flags.Flags)
	}
	if flags.Silent {
		return nil
	}
	for _, i := range idx {
		e := sess.view[i]
		rw := w.CreateMessage(uint32(i + 1))
		rw.WriteUID(e.uid)
		rw.WriteFlags(f.flags(e.m))
		if err := rw.Close(); err != nil {
			return err
		}
	}
	return nil
}

// storeFlags applies a flag change to m in f. \Deleted goes to the folder,
// the rest to the message, and \Starred follows \Flagged.
func storeFlags(f *folder, m *Message, op imap.StoreFlagsOp, flags []imap.Flag) {
	deleted := slices.Contains(flags, imap.FlagDeleted)
	var others []imap.Flag
	for _, fl := range flags {
		if fl != imap.FlagDeleted && !slices.Contains(others, fl) {
			others = append(others, fl)
		}
	}
	switch op {
	case imap.StoreFlagsSet:
		m.Flags = others
		if deleted {
			f.deleted[m] = true
		} else {
			delete(f.deleted, m)
		}
	case imap.StoreFlagsAdd:
		for _, fl := range others {
			if !slices.Contains(m.Flags, fl) {
				m.Flags = append(m.Flags, fl)
			}
		}
		if deleted {
			f.deleted[m] = true
		}
	case imap.StoreFlagsDel:
		m.Flags = slices.DeleteFunc(m.Flags, func(fl imap.Flag) bool { return slices.Contains(others, fl) })
		if deleted {
			delete(f.deleted, m)
		}
	}
	starFromFlags(m)
}

// uidPair is one message's UID in the source and destination of a COPY or MOVE.
type uidPair struct{ src, dest imap.UID }

// copyData builds COPYUID for pairs in order. It does not sort: COPYUID
// matches source and destination UIDs by position.
func copyData(dest *folder, pairs []uidPair) *imap.CopyData {
	if len(pairs) == 0 {
		return nil
	}
	data := &imap.CopyData{UIDValidity: dest.uidValidity}
	for _, p := range pairs {
		data.SourceUIDs = appendUID(data.SourceUIDs, p.src)
		data.DestUIDs = appendUID(data.DestUIDs, p.dest)
	}
	return data
}

// appendUID adds uid at the end of set, growing the last range when uid
// follows it.
func appendUID(set imap.UIDSet, uid imap.UID) imap.UIDSet {
	if n := len(set); n > 0 && set[n-1].Stop+1 == uid {
		set[n-1].Stop = uid
		return set
	}
	return append(set, imap.UIDRange{Start: uid, Stop: uid})
}

// Copy files messages in dest as Gmail does: into a label folder it adds the
// label, which is how Gmail copies. A set that names no current message
// answers OK with no COPYUID.
func (sess *session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.syncLocked()
	d := s.folderLocked(dest)
	if d == nil {
		return nil, tryCreate(dest)
	}
	idx, err := sess.messagesIn(numSet)
	if err != nil {
		return nil, err
	}
	var pairs []uidPair
	for _, i := range idx {
		e := sess.view[i]
		fileLocked(e.m, d)
		pairs = append(pairs, uidPair{e.uid, d.ensure(e.m)})
	}
	s.refreshLocked()
	return copyData(d, pairs), nil
}

// Move moves messages as Gmail does (see moveLocked), then reports COPYUID
// and an EXPUNGE for each message that left the selected folder.
func (sess *session) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	if err := sess.writable(); err != nil {
		return err
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.syncLocked()
	d := s.folderLocked(dest)
	if d == nil {
		return tryCreate(dest)
	}
	idx, err := sess.messagesIn(numSet)
	if err != nil {
		return err
	}
	var pairs []uidPair
	for _, i := range idx {
		e := sess.view[i]
		moveLocked(e.m, sess.selected, d)
		pairs = append(pairs, uidPair{e.uid, d.ensure(e.m)})
	}
	s.refreshLocked()
	if data := copyData(d, pairs); data != nil {
		if err := w.WriteCopyData(data); err != nil {
			return err
		}
	}
	return sess.expungeGoneLocked(w.WriteExpunge)
}

// Expunge expunges the messages marked \Deleted in the selected folder, only
// those in uids for UID EXPUNGE, as expungeLocked describes.
func (sess *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	if err := sess.writable(); err != nil {
		return err
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	f := sess.selected
	sess.syncLocked()
	var in imap.UIDSet
	if uids != nil {
		static, err := staticUIDSet(f, *uids)
		if err != nil {
			return err
		}
		in = static
	}
	for _, e := range sess.view {
		if sess.present(e) && f.deleted[e.m] && (uids == nil || in.Contains(e.uid)) {
			expungeLocked(e.m, f)
		}
	}
	s.refreshLocked()
	return sess.expungeGoneLocked(w.WriteExpunge)
}

// labelUpdate is one message's labels after a label STORE, for its FETCH
// response.
type labelUpdate struct {
	seq    uint32
	uid    imap.UID
	labels []string
}

// systemLabels are Gmail's system labels in the case Gmail writes them.
var systemLabels = []string{`\Inbox`, `\Important`, `\Starred`, `\Sent`, `\Draft`}

// StoreGmailLabels answers STORE X-GM-LABELS (imapserver.SessionGmail). It
// writes each message's UID and labels unless the STORE is SILENT.
func (sess *session) StoreGmailLabels(w *imapserver.FetchWriter, numSet imap.NumSet, store *imap.StoreGmailLabels) error {
	updates, err := sess.storeLabels(numSet, store.Op, store.Labels)
	if err != nil || store.Silent {
		return err
	}
	for _, u := range updates {
		rw := w.CreateMessage(u.seq)
		rw.WriteUID(u.uid)
		rw.WriteGmailLabels(u.labels)
		if err := rw.Close(); err != nil {
			return err
		}
	}
	return nil
}

// storeLabels runs STORE [+|-]X-GM-LABELS on the selected folder and returns
// the FETCH answers. op Set replaces the labels. System labels are matched
// without regard to case, a user label the server lacks is created, as Gmail
// does, and the folder names INBOX, Sent Mail and Drafts stand for their
// labels. \Flagged follows \Starred. A set that names no current message
// answers OK with no FETCH.
func (sess *session) storeLabels(numSet imap.NumSet, op imap.StoreFlagsOp, labels []string) (_ []labelUpdate, err error) {
	if err := sess.writable(); err != nil {
		return nil, err
	}
	s := sess.s
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.syncLocked()
	defer sess.endQuiet(&err)
	idx, err := sess.messagesIn(numSet)
	if err != nil || len(idx) == 0 {
		return nil, err
	}
	names := make([]string, 0, len(labels))
	var create []string
	for _, l := range labels {
		name, isNew, err := s.labelNameLocked(l)
		if err != nil {
			return nil, err
		}
		if isNew && op != imap.StoreFlagsDel && !slices.ContainsFunc(create, func(c string) bool { return strings.EqualFold(c, name) }) {
			create = append(create, name)
		}
		names = append(names, name)
	}
	for _, name := range create {
		s.addUserFolderLocked(name)
	}
	for _, i := range idx {
		m := sess.view[i].m
		if op == imap.StoreFlagsSet {
			m.Labels = nil
		}
		for _, name := range names {
			if op == imap.StoreFlagsDel {
				removeLabel(m, name)
			} else {
				addLabel(m, name)
			}
		}
		starFromLabels(m)
	}
	s.refreshLocked()
	updates := make([]labelUpdate, 0, len(idx))
	for _, i := range idx {
		e := sess.view[i]
		updates = append(updates, labelUpdate{seq: uint32(i + 1), uid: e.uid, labels: slices.Clone(e.m.Labels)})
	}
	return updates, nil
}

// labelNameLocked resolves a label as Gmail does, saying whether it names no
// folder yet. Trash, Spam and All Mail are not labels.
func (s *Server) labelNameLocked(l string) (name string, isNew bool, err error) {
	if strings.HasPrefix(l, `\`) {
		if i := slices.IndexFunc(systemLabels, func(sys string) bool { return strings.EqualFold(sys, l) }); i >= 0 {
			return systemLabels[i], false, nil
		}
		return "", false, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "fakegmail: unknown system label " + l}
	}
	if f := s.folderFoldLocked(l); f != nil {
		if f.kind != kindLabel {
			return "", false, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "fakegmail: " + f.name + " is not a label"}
		}
		return f.label, false, nil
	}
	if err := checkUserLabel(l); err != nil {
		return "", false, err
	}
	return l, true, nil
}
