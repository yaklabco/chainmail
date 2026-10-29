package imapstore

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
)

// compileGmail turns a query tree, with in: and account: split off, into an
// X-GM-RAW string. in:trash, in:spam and in:anywhere chose the source
// folders instead, so they are left out. in:archive becomes -in:inbox, All
// Mail without \Inbox. Dates become Gmail's YYYY/MM/DD, ages use the d, m
// and y units Gmail documents, and sizes go as plain bytes. Text Gmail would
// read as an operator is quoted, and a double negation is dropped.
func compileGmail(n *query.Node, in string) string {
	var parts []string
	switch strings.ToLower(in) {
	case "", "trash", "spam", "junk", "anywhere":
	case "archive":
		parts = append(parts, "-in:inbox")
	default:
		parts = append(parts, string(query.FieldIn)+":"+gmailQuote(in, false))
	}
	if n != nil {
		var b strings.Builder
		writeGmail(&b, n, false)
		s := b.String()
		if len(parts) > 0 && dropDoubleNot(n).Op == query.OpOr {
			s = "(" + s + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// writeGmail prints n in Gmail search syntax. It follows query.Node.String,
// where OR binds tighter than the implicit AND, so inOr says n is a direct
// child of an OR and an AND needs parentheses. It reads n and never changes it.
func writeGmail(b *strings.Builder, n *query.Node, inOr bool) {
	n = dropDoubleNot(n)
	switch n.Op {
	case query.OpTerm:
		if n.Field == query.FieldText {
			b.WriteString(gmailQuote(n.Value, true))
			return
		}
		b.WriteString(string(n.Field))
		b.WriteByte(':')
		b.WriteString(gmailQuote(gmailValue(n), false))
	case query.OpNot:
		// dropDoubleNot leaves no NOT directly under a NOT.
		b.WriteByte('-')
		child := n.Children[0]
		if child.Op == query.OpTerm {
			writeGmail(b, child, false)
			return
		}
		b.WriteByte('(')
		writeGmail(b, child, false)
		b.WriteByte(')')
	case query.OpAnd:
		if inOr {
			b.WriteByte('(')
		}
		for i, c := range n.Children {
			if i > 0 {
				b.WriteByte(' ')
			}
			writeGmail(b, c, false)
		}
		if inOr {
			b.WriteByte(')')
		}
	case query.OpOr:
		for i, c := range n.Children {
			if i > 0 {
				b.WriteString(" OR ")
			}
			writeGmail(b, c, true)
		}
	}
}

// dropDoubleNot strips pairs of NOTs from the top of n: --a means a. Gmail
// documents no meaning for a doubled minus.
func dropDoubleNot(n *query.Node) *query.Node {
	for n.Op == query.OpNot && n.Children[0].Op == query.OpNot {
		n = n.Children[0].Children[0]
	}
	return n
}

// gmailValue is a term's value in the form Gmail documents. The tree was
// validated, so a value that does not parse is left as it is.
func gmailValue(n *query.Node) string {
	switch n.Field {
	case query.FieldAfter, query.FieldBefore:
		return strings.ReplaceAll(n.Value, "-", "/")
	case query.FieldNewerThan, query.FieldOlderThan:
		// Gmail documents d, m and y, lowercase, so weeks become days.
		num, unit, err := query.ParseAge(n.Value)
		if err != nil {
			return n.Value
		}
		if unit == 'w' {
			num, unit = 7*num, 'd'
		}
		return strconv.Itoa(num) + string(unit)
	case query.FieldLarger, query.FieldSmaller:
		// K and M are KiB and MiB here. Gmail does not say which it
		// means, so the size goes as bytes.
		size, err := query.ParseSize(n.Value)
		if err != nil {
			return n.Value
		}
		return strconv.FormatInt(size, 10)
	default:
		return n.Value
	}
}

// gmailOperatorWords are bare words Gmail reads as operators.
var gmailOperatorWords = []string{"AND", "AROUND"}

// gmailQuote returns v as it must appear in X-GM-RAW. It quotes what
// query.Node.String quotes, and also braces, which Gmail reads as an OR
// group, and bare AND and AROUND in any case. text says v is a bare word.
func gmailQuote(v string, text bool) string {
	if !gmailNeedsQuotes(v, text) {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func gmailNeedsQuotes(v string, text bool) bool {
	if v == "" {
		return true
	}
	if text && (v == "OR" || strings.HasPrefix(v, "-") || strings.ContainsRune(v, ':') ||
		slices.ContainsFunc(gmailOperatorWords, func(w string) bool { return strings.EqualFold(v, w) })) {
		return true
	}
	return strings.ContainsFunc(v, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(`()"{}`, r)
	})
}

// gmailCriteria wraps an X-GM-RAW string. An empty one matches everything.
func gmailCriteria(raw string) *imap.SearchCriteria {
	if raw == "" {
		return &imap.SearchCriteria{}
	}
	return &imap.SearchCriteria{GmailRaw: []string{raw}}
}

var headerKeys = map[query.Field]string{
	query.FieldFrom: "From", query.FieldTo: "To", query.FieldCc: "Cc", query.FieldBcc: "Bcc", query.FieldSubject: "Subject",
}

// minDate is the earliest SINCE or BEFORE date compileStandard sends. IMAP
// servers keep internal dates as Unix times, an age as long as 9999y would
// reach a negative year, and go-imap reads 0001-01-01 as no bound at all, so
// earlier dates are refused rather than sent with a changed meaning.
var minDate = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// maxSize is the largest LARGER or SMALLER value: RFC 3501 numbers are 32 bits.
const maxSize = math.MaxUint32

// matchNothing is NOT ALL.
func matchNothing() *imap.SearchCriteria {
	return &imap.SearchCriteria{Not: []imap.SearchCriteria{{}}}
}

// compileStandard turns a query tree, with in: and account: split off, into
// IMAP SEARCH criteria. Operators IMAP SEARCH cannot express are refused.
func compileStandard(n *query.Node, now time.Time) (*imap.SearchCriteria, error) {
	if n == nil {
		return &imap.SearchCriteria{}, nil
	}
	switch n.Op {
	case query.OpAnd:
		out := &imap.SearchCriteria{}
		for _, c := range n.Children {
			cc, err := compileStandard(c, now)
			if err != nil {
				return nil, err
			}
			intersect(out, cc)
		}
		return out, nil
	case query.OpOr:
		var acc *imap.SearchCriteria
		for _, c := range n.Children {
			cc, err := compileStandard(c, now)
			if err != nil {
				return nil, err
			}
			if acc == nil {
				acc = cc
				continue
			}
			acc = &imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{*acc, *cc}}}
		}
		return acc, nil
	case query.OpNot:
		cc, err := compileStandard(n.Children[0], now)
		if err != nil {
			return nil, err
		}
		return &imap.SearchCriteria{Not: []imap.SearchCriteria{*cc}}, nil
	default:
		return standardTerm(n, now)
	}
}

func standardTerm(n *query.Node, now time.Time) (*imap.SearchCriteria, error) {
	switch n.Field {
	case query.FieldText:
		return &imap.SearchCriteria{Text: []string{n.Value}}, nil
	case query.FieldFrom, query.FieldTo, query.FieldCc, query.FieldBcc, query.FieldSubject:
		return &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: headerKeys[n.Field], Value: n.Value}}}, nil
	case query.FieldIs:
		switch strings.ToLower(n.Value) {
		case "unread":
			return &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil
		case "read":
			return &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}, nil
		case "starred":
			return &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}}, nil
		default:
			return nil, fmt.Errorf("%w: is:%s is not valid; use is:unread, is:read or is:starred", mailstore.ErrUnsupportedQuery, n.Value)
		}
	case query.FieldNewerThan, query.FieldOlderThan:
		if _, _, err := query.ParseAge(n.Value); err != nil {
			return nil, badValue(n, err)
		}
		d := ageStart(now, n.Value)
		if err := checkDate(n, d); err != nil {
			return nil, err
		}
		if n.Field == query.FieldNewerThan {
			return &imap.SearchCriteria{Since: d}, nil
		}
		return &imap.SearchCriteria{Before: d}, nil
	case query.FieldAfter, query.FieldBefore:
		d, err := query.ParseDate(n.Value)
		if err != nil {
			return nil, badValue(n, err)
		}
		if err := checkDate(n, d); err != nil {
			return nil, err
		}
		if n.Field == query.FieldAfter {
			return &imap.SearchCriteria{Since: d}, nil
		}
		return &imap.SearchCriteria{Before: d}, nil
	case query.FieldLarger, query.FieldSmaller:
		size, err := query.ParseSize(n.Value)
		if err != nil {
			return nil, badValue(n, err)
		}
		if size > maxSize {
			return nil, fmt.Errorf("%w: %s:%s is %d bytes, and IMAP SEARCH sizes go up to %d bytes",
				mailstore.ErrUnsupportedQuery, n.Field, n.Value, size, int64(maxSize))
		}
		if n.Field == query.FieldLarger {
			// larger:0 drops out, as go-imap reads 0 as no bound. Every
			// message is larger than 0 bytes.
			return &imap.SearchCriteria{Larger: size}, nil
		}
		if size == 0 {
			return matchNothing(), nil // no message is smaller than 0 bytes
		}
		return &imap.SearchCriteria{Smaller: size}, nil
	case query.FieldHas, query.FieldFilename:
		return nil, fmt.Errorf("%w: %s: needs the local index on this account, which arrives in a later release; it works on Gmail accounts now",
			mailstore.ErrUnsupportedQuery, n.Field)
	case query.FieldLabel:
		return nil, fmt.Errorf("%w: label: works on Gmail accounts only; use in:<folder> here", mailstore.ErrUnsupportedQuery)
	default:
		return nil, fmt.Errorf("%w: %s: cannot be sent to the server", mailstore.ErrUnsupportedQuery, n.Field)
	}
}

// badValue wraps a parse error for a term in ErrUnsupportedQuery. Validated
// trees never get here, but a bad value is the caller's, not an internal error.
func badValue(n *query.Node, err error) error {
	return fmt.Errorf("%w: %s: %w", mailstore.ErrUnsupportedQuery, n.Field, err)
}

// checkDate refuses a SINCE or BEFORE date earlier than minDate.
func checkDate(n *query.Node, d time.Time) error {
	if !d.Before(minDate) {
		return nil
	}
	return fmt.Errorf("%w: %s:%s reaches back before %s, the earliest date IMAP SEARCH compares here; use a later date or a shorter age",
		mailstore.ErrUnsupportedQuery, n.Field, n.Value, minDate.Format(time.DateOnly))
}

// ageStart is the day an age such as 7d reaches back to. IMAP compares dates
// without times, so newer_than:1d means since the start of yesterday. A month
// or year back from a day the target month lacks is that month's last day:
// newer_than:1m on 31 March is since 28 February. The longest age the parser
// allows, 9999y, still fits a time.Time.
func ageStart(now time.Time, v string) time.Time {
	n, unit, _ := query.ParseAge(v) // the tree was validated
	y, m, d := now.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	switch unit {
	case 'w':
		return day.AddDate(0, 0, -7*n)
	case 'm':
		return addMonths(day, -n)
	case 'y':
		return addMonths(day, -12*n)
	default:
		return day.AddDate(0, 0, -n)
	}
}

// addMonths moves t by months, keeping its time of day. Where time.AddDate
// would spill past the end of a shorter target month, as 31 March less a
// month gives 3 March, addMonths stops at that month's last day.
func addMonths(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(months), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	if last := first.AddDate(0, 1, -1).Day(); d > last {
		d = last
	}
	return first.AddDate(0, 0, d-1)
}

// intersect adds src to dst as AND. imap.SearchCriteria.And clears dst's
// Smaller when src has none, so the size and date bounds are combined here.
func intersect(dst, src *imap.SearchCriteria) {
	dst.SeqNum = append(dst.SeqNum, src.SeqNum...)
	dst.UID = append(dst.UID, src.UID...)
	dst.Since = later(dst.Since, src.Since)
	dst.Before = earlier(dst.Before, src.Before)
	dst.SentSince = later(dst.SentSince, src.SentSince)
	dst.SentBefore = earlier(dst.SentBefore, src.SentBefore)
	dst.Header = append(dst.Header, src.Header...)
	dst.Body = append(dst.Body, src.Body...)
	dst.Text = append(dst.Text, src.Text...)
	dst.Flag = append(dst.Flag, src.Flag...)
	dst.NotFlag = append(dst.NotFlag, src.NotFlag...)
	if src.Larger > dst.Larger {
		dst.Larger = src.Larger
	}
	if src.Smaller > 0 && (dst.Smaller == 0 || src.Smaller < dst.Smaller) {
		dst.Smaller = src.Smaller
	}
	switch {
	case src.ModSeq == nil:
	case dst.ModSeq == nil:
		dst.ModSeq = src.ModSeq
	default:
		// Only one MODSEQ key fits, so the second goes in as NOT NOT.
		dst.Not = append(dst.Not, imap.SearchCriteria{Not: []imap.SearchCriteria{{ModSeq: src.ModSeq}}})
	}
	dst.Not = append(dst.Not, src.Not...)
	dst.Or = append(dst.Or, src.Or...)
	dst.GmailRaw = append(dst.GmailRaw, src.GmailRaw...)
	dst.GmailMsgID = append(dst.GmailMsgID, src.GmailMsgID...)
	dst.GmailThreadID = append(dst.GmailThreadID, src.GmailThreadID...)
}

// later is the later of two lower date bounds, where zero is none.
func later(a, b time.Time) time.Time {
	if a.IsZero() || b.After(a) {
		return b
	}
	return a
}

// earlier is the earlier of two upper date bounds, where zero is none.
func earlier(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

var attrRoles = map[imap.MailboxAttr]mailstore.Role{
	imap.MailboxAttrSent: mailstore.RoleSent, imap.MailboxAttrDrafts: mailstore.RoleDrafts,
	imap.MailboxAttrArchive: mailstore.RoleArchive, imap.MailboxAttrTrash: mailstore.RoleTrash,
	imap.MailboxAttrJunk: mailstore.RoleSpam, imap.MailboxAttrAll: mailstore.RoleAll,
}

// nameRoles are the usual names of special folders on servers without SPECIAL-USE.
var nameRoles = map[string]mailstore.Role{
	"sent": mailstore.RoleSent, "sent items": mailstore.RoleSent, "sent messages": mailstore.RoleSent, "sent mail": mailstore.RoleSent,
	"drafts": mailstore.RoleDrafts, "draft": mailstore.RoleDrafts,
	"archive": mailstore.RoleArchive, "archives": mailstore.RoleArchive,
	"trash": mailstore.RoleTrash, "deleted items": mailstore.RoleTrash, "deleted messages": mailstore.RoleTrash, "bin": mailstore.RoleTrash,
	"junk": mailstore.RoleSpam, "spam": mailstore.RoleSpam, "junk e-mail": mailstore.RoleSpam, "junk email": mailstore.RoleSpam, "bulk mail": mailstore.RoleSpam,
}

// assignRoles gives each listed folder a role. INBOX is the inbox by name.
// Special-use attributes come first. A role no folder has by attribute goes
// to the first selectable folder whose last path segment has a usual name
// for it.
func assignRoles(list []*imap.ListData) []mailstore.Role {
	out := make([]mailstore.Role, len(list))
	claimed := map[mailstore.Role]bool{}
	for i, l := range list {
		out[i] = mailstore.RoleNone
		if strings.EqualFold(l.Mailbox, "INBOX") {
			out[i] = mailstore.RoleInbox
			continue
		}
		for _, a := range l.Attrs {
			if r, ok := attrRoles[a]; ok {
				out[i] = r
				claimed[r] = true
				break
			}
		}
	}
	for i, l := range list {
		if out[i] != mailstore.RoleNone || slices.ContainsFunc(l.Attrs, unselectable) {
			continue
		}
		leaf := l.Mailbox
		if l.Delim != 0 {
			if j := strings.LastIndex(leaf, string(l.Delim)); j >= 0 {
				leaf = leaf[j+len(string(l.Delim)):]
			}
		}
		if r, ok := nameRoles[strings.ToLower(leaf)]; ok && !claimed[r] {
			out[i] = r
			claimed[r] = true
		}
	}
	return out
}

func unselectable(a imap.MailboxAttr) bool {
	return a == imap.MailboxAttrNoSelect || a == imap.MailboxAttrNonExistent
}

// byRole returns the first selectable folder with role, or "".
func byRole(folders []mailstore.Folder, role mailstore.Role) string {
	for _, f := range folders {
		if f.Selectable && f.Role == role {
			return f.Name
		}
	}
	return ""
}

// gmailSources picks the folders a Gmail search runs in. Trash and Spam are
// not part of All Mail, so in:trash, in:spam and in:anywhere name them.
func gmailSources(folders []mailstore.Folder, in string) ([]string, error) {
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
		if name := byRole(folders, r); name != "" {
			out = append(out, name)
		}
	}
	if slices.Contains(want, mailstore.RoleAll) && byRole(folders, mailstore.RoleAll) == "" {
		return nil, fmt.Errorf("%w: no All Mail folder; in Gmail settings, show All Mail in IMAP", mailstore.ErrNotFound)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no folder for in:%s", mailstore.ErrNotFound, in)
	}
	return out, nil
}

// standardSources picks the folders a search runs in on a non-Gmail account:
// every selectable folder but Trash, Junk and \All, or what in: names.
// in:anywhere adds Trash and Junk. A \All folder, such as a Dovecot virtual
// one, holds a copy of every message in the others, so it is searched only
// when in: names it, or each match would be found twice.
func standardSources(folders []mailstore.Folder, in string) ([]string, error) {
	lower := strings.ToLower(in)
	var out []string
	switch lower {
	case "", "anywhere":
		for _, f := range folders {
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
		for _, f := range folders {
			if f.Selectable && f.Role == role {
				out = append(out, f.Name)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%w: no folder has the %s role; run: chainmail folders", mailstore.ErrNotFound, role)
		}
		return out, nil
	default:
		for _, f := range folders {
			if f.Selectable && f.Name == in {
				return []string{f.Name}, nil
			}
		}
		return nil, fmt.Errorf("%w: no folder named %q; run: chainmail folders", mailstore.ErrNotFound, in)
	}
}
