package cache

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
)

// Plan is a search compiled for the cache: the condition to run, and what
// the router needs to decide whether the cache can answer it completely.
type Plan struct {
	Where       Where
	NeedsBodies bool      // a text term is present
	Since       time.Time // the top-level AND lower date bound (newer_than or after); zero when none
	ServerOnly  bool      // the cache can never answer this query completely
	Reason      string    // with ServerOnly: text_outside_window or folder_not_synced
}

// errMalformedQuery is a tree neither query.Parse nor query.Validate lets
// through: an AND or OR without children, a NOT without exactly one, a nil
// child, or an unknown node kind.
var errMalformedQuery = errors.New("malformed query tree")

// planFolderNotSynced is the reason of a plan that searches a folder the
// cache never holds. It is an incomplete_reason value of the search API.
const planFolderNotSynced = "folder_not_synced"

// queryMinDate and queryMaxSize are the limits imapstore's compileStandard
// enforces off Google: IMAP keeps internal dates as Unix times, and RFC
// 3501 numbers are 32 bits. The cache refuses the same values with the same
// text there, so a search fails alike on either route.
var queryMinDate = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

const queryMaxSize = math.MaxUint32

// likeEscape follows every LIKE: likeEscaper marks %, _ and \ in the value
// with a backslash, so they match themselves.
const likeEscape = ` ESCAPE '\'`

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ftsMatch selects the messages whose FTS row matches the argument, an
// FTS5 query from ftsExpr.
const ftsMatch = "m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)"

// normColumns are the lower-cased, newline-joined columns the substring
// operators match.
var normColumns = map[query.Field]string{
	query.FieldFrom:     "m.from_norm",
	query.FieldTo:       "m.to_norm",
	query.FieldCc:       "m.cc_norm",
	query.FieldBcc:      "m.bcc_norm",
	query.FieldSubject:  "m.subject_norm",
	query.FieldFilename: "m.attachments_norm",
}

// Compile turns a query (in: and account: already split off, as the
// daemon's plan does) into a Plan. in is the in: value; gmail selects label
// semantics. now is the clock ages count back from, the one the server
// path's dialect uses, so both pick the same day.
func Compile(n *query.Node, in string, gmail bool, now time.Time) (Plan, error) {
	c := &compiler{gmail: gmail, now: now}
	var (
		conds     []string
		args      []any
		notSynced bool
	)
	if gmail {
		sql, a, synced := gmailIn(in)
		notSynced = !synced
		if sql != "" {
			conds, args = append(conds, sql), append(args, a...)
		}
	}
	var fts string
	if n != nil {
		sql, a, expr, err := c.root(n)
		if err != nil {
			return Plan{}, err
		}
		if sql != "" {
			conds, args = append(conds, sql), append(args, a...)
		}
		fts = expr
	}
	p := Plan{
		Where: Where{SQL: strings.Join(conds, " AND "), Args: args, FTS: fts},
		Since: lowerBound(n, now),
	}
	switch {
	case notSynced:
		p.ServerOnly, p.Reason = true, planFolderNotSynced
	case c.unsupported:
		p.ServerOnly = true
	}
	return p, nil
}

// gmailIn compiles the in: value of a Google account, where every message
// lives once in All Mail and in: names a label, as compileGmail sends it
// to X-GM-RAW. synced is false for trash, spam, junk and anywhere, whose
// folders lie outside All Mail: the cache holds none of their messages.
func gmailIn(in string) (sql string, args []any, synced bool) {
	switch strings.ToLower(in) {
	case "":
		return "", nil, true
	case "trash", "spam", "junk", "anywhere":
		return "", nil, false
	case "inbox":
		sql, args = likeElement("m.labels_norm", `\Inbox`)
	case "archive":
		sql, args = likeElement("m.labels_norm", `\Inbox`)
		sql = "NOT (" + sql + ")"
	case "sent":
		sql, args = likeElement("m.labels_norm", `\Sent`)
	case "drafts":
		sql, args = likeElement("m.labels_norm", `\Draft`)
	case "starred":
		sql, args = likeElement("m.flags_norm", `\Flagged`)
	case "important":
		sql, args = likeElement("m.labels_norm", `\Important`)
	default:
		sql, args = likeElement("m.labels_norm", in)
	}
	return sql, args, true
}

// compiler holds what compiling one tree needs and learns.
type compiler struct {
	gmail bool
	now   time.Time
	// unsupported is set when a term the cache cannot evaluate compiled
	// to 0.
	unsupported bool
}

// node compiles n to a condition over messages aliased m.
func (c *compiler) node(n *query.Node) (string, []any, error) {
	if n == nil {
		return "", nil, errMalformedQuery
	}
	if expr, ok := ftsExpr(n); ok {
		return ftsMatch, []any{expr}, nil
	}
	switch n.Op {
	case query.OpTerm:
		return c.term(n)
	case query.OpAnd, query.OpOr:
		if len(n.Children) == 0 {
			return "", nil, errMalformedQuery
		}
		sep := " AND "
		if n.Op == query.OpOr {
			sep = " OR "
		}
		return c.join(n.Children, sep)
	case query.OpNot:
		if len(n.Children) != 1 {
			return "", nil, errMalformedQuery
		}
		sql, args, err := c.node(n.Children[0])
		if err != nil {
			return "", nil, err
		}
		return "NOT (" + sql + ")", args, nil
	default:
		return "", nil, errMalformedQuery
	}
}

// join compiles nodes and joins them with sep, in parentheses.
func (c *compiler) join(nodes []*query.Node, sep string) (string, []any, error) {
	parts := make([]string, 0, len(nodes))
	var args []any
	for _, ch := range nodes {
		sql, a, err := c.node(ch)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, sql)
		args = append(args, a...)
	}
	return "(" + strings.Join(parts, sep) + ")", args, nil
}

// root compiles the whole tree. Text alone, or the text terms (and NOTs
// of text) directly under the top-level AND when at least one is positive,
// become the FTS5 query of Where.FTS, which SearchUIDs joins once. The
// rest becomes SQL, where any other text is an inline FTS subquery.
func (c *compiler) root(n *query.Node) (sql string, args []any, fts string, err error) {
	if expr, ok := ftsExpr(n); ok {
		return "", nil, expr, nil
	}
	if n.Op == query.OpAnd {
		var text, rest []*query.Node
		for _, ch := range n.Children {
			if ftsTextual(ch) {
				text = append(text, ch)
			} else {
				rest = append(rest, ch)
			}
		}
		if expr, ok := ftsExpr(&query.Node{Op: query.OpAnd, Children: text}); ok {
			sql, args, err = c.all(rest)
			return sql, args, expr, err
		}
	}
	sql, args, err = c.node(n)
	return sql, args, "", err
}

// all compiles nodes joined by AND. None is "", which matches everything.
func (c *compiler) all(nodes []*query.Node) (string, []any, error) {
	switch len(nodes) {
	case 0:
		return "", nil, nil
	case 1:
		return c.node(nodes[0])
	default:
		return c.join(nodes, " AND ")
	}
}

// ftsExpr returns n as an FTS5 query when n is made of text terms alone,
// joined by AND and OR, with NOT only beside a positive term under an AND,
// since FTS5's NOT is binary. Each term becomes one quoted FTS5 string, so
// a phrase matches its tokens in order and nothing in a value reads as
// FTS5 syntax. Compound results are parenthesized.
func ftsExpr(n *query.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	switch n.Op {
	case query.OpTerm:
		if n.Field != query.FieldText {
			return "", false
		}
		return ftsString(n.Value), true
	case query.OpOr:
		if len(n.Children) == 0 {
			return "", false
		}
		parts := make([]string, 0, len(n.Children))
		for _, ch := range n.Children {
			e, ok := ftsExpr(ch)
			if !ok {
				return "", false
			}
			parts = append(parts, e)
		}
		if len(parts) == 1 {
			return parts[0], true
		}
		return "(" + strings.Join(parts, " OR ") + ")", true
	case query.OpAnd:
		var pos, neg []string
		for _, ch := range n.Children {
			if e, ok := ftsExpr(ch); ok {
				pos = append(pos, e)
				continue
			}
			if e, ok := ftsNegated(ch); ok {
				neg = append(neg, e)
				continue
			}
			return "", false
		}
		if len(pos) == 0 {
			return "", false
		}
		expr := pos[0]
		if len(pos) > 1 {
			expr = "(" + strings.Join(pos, " AND ") + ")"
		}
		for _, e := range neg {
			expr = "(" + expr + " NOT " + e + ")"
		}
		return expr, true
	default:
		return "", false
	}
}

// ftsNegated returns the FTS5 query of n's child when n is a NOT of text
// alone.
func ftsNegated(n *query.Node) (string, bool) {
	if n == nil || n.Op != query.OpNot || len(n.Children) != 1 {
		return "", false
	}
	return ftsExpr(n.Children[0])
}

// ftsTextual reports whether n can join the top-level FTS5 query: text
// alone, or a NOT of text alone.
func ftsTextual(n *query.Node) bool {
	if _, ok := ftsExpr(n); ok {
		return true
	}
	_, ok := ftsNegated(n)
	return ok
}

// ftsString quotes v as one FTS5 string, doubling any quote inside it.
func ftsString(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}

// term compiles one operator term.
func (c *compiler) term(n *query.Node) (string, []any, error) {
	switch n.Field {
	case query.FieldFrom, query.FieldTo, query.FieldCc, query.FieldBcc, query.FieldSubject, query.FieldFilename:
		sql, args := likeContains(normColumns[n.Field], n.Value)
		return sql, args, nil
	case query.FieldIs:
		return c.is(n)
	case query.FieldHas:
		if strings.EqualFold(n.Value, "attachment") {
			return "m.has_attachment = 1", nil, nil
		}
		return c.cannot()
	case query.FieldLabel:
		return c.label(n)
	case query.FieldNewerThan, query.FieldOlderThan, query.FieldAfter, query.FieldBefore:
		return c.date(n)
	case query.FieldLarger, query.FieldSmaller:
		return c.size(n)
	case query.FieldIn, query.FieldAccount:
		return "", nil, fmt.Errorf("%s: chooses where to search and must be split off before Compile", n.Field)
	default:
		return c.cannot()
	}
}

// cannot is a term the cache cannot evaluate: it matches nothing, so a
// forced cache search still runs, and the plan becomes server-only.
func (c *compiler) cannot() (string, []any, error) {
	c.unsupported = true
	return "0", nil, nil
}

// is compiles is:. Starred is IMAP's \Flagged on every account, Gmail's
// star included; important is Gmail's \Important label.
func (c *compiler) is(n *query.Node) (string, []any, error) {
	switch strings.ToLower(n.Value) {
	case "unread":
		sql, args := likeElement("m.flags_norm", `\Seen`)
		return "NOT (" + sql + ")", args, nil
	case "read":
		sql, args := likeElement("m.flags_norm", `\Seen`)
		return sql, args, nil
	case "starred":
		sql, args := likeElement("m.flags_norm", `\Flagged`)
		return sql, args, nil
	case "important":
		if c.gmail {
			sql, args := likeElement("m.labels_norm", `\Important`)
			return sql, args, nil
		}
	}
	return c.cannot()
}

// label compiles label:. On Google it is a label; elsewhere an IMAP
// keyword, and a system flag such as \Seen is not a label.
func (c *compiler) label(n *query.Node) (string, []any, error) {
	switch {
	case c.gmail:
		sql, args := likeElement("m.labels_norm", n.Value)
		return sql, args, nil
	case strings.HasPrefix(n.Value, `\`):
		return c.cannot()
	default:
		sql, args := likeElement("m.flags_norm", n.Value)
		return sql, args, nil
	}
}

// likeContains matches value as a case-insensitive substring of column.
func likeContains(column, value string) (string, []any) {
	return column + " LIKE ?" + likeEscape, []any{"%" + likeEscaper.Replace(strings.ToLower(value)) + "%"}
}

// likeElement matches value, case-insensitively, as one whole element of a
// newline-joined column, which has a newline before and after each element.
func likeElement(column, value string) (string, []any) {
	return column + " LIKE ?" + likeEscape, []any{"%\n" + likeEscaper.Replace(strings.ToLower(value)) + "\n%"}
}

// date compiles a date or age term. newer_than and after are IMAP SINCE,
// on or after the day; older_than and before are BEFORE, strictly before
// it. internal_date holds Unix seconds.
func (c *compiler) date(n *query.Node) (string, []any, error) {
	d, err := c.day(n)
	if err != nil {
		return "", nil, err
	}
	if n.Field == query.FieldNewerThan || n.Field == query.FieldAfter {
		return "m.internal_date >= ?", []any{d.Unix()}, nil
	}
	return "m.internal_date < ?", []any{d.Unix()}, nil
}

// day is the day a date or age term compares with, as compileStandard
// computes it, refused off Google when it is before queryMinDate.
func (c *compiler) day(n *query.Node) (time.Time, error) {
	var d time.Time
	switch n.Field {
	case query.FieldNewerThan, query.FieldOlderThan:
		if _, _, err := query.ParseAge(n.Value); err != nil {
			return time.Time{}, badQueryValue(n, err)
		}
		d = queryAgeStart(c.now, n.Value)
	default:
		var err error
		if d, err = query.ParseDate(n.Value); err != nil {
			return time.Time{}, badQueryValue(n, err)
		}
	}
	if !c.gmail && d.Before(queryMinDate) {
		return time.Time{}, fmt.Errorf("%w: %s:%s reaches back before %s, the earliest date IMAP SEARCH compares here; use a later date or a shorter age",
			mailstore.ErrUnsupportedQuery, n.Field, n.Value, queryMinDate.Format(time.DateOnly))
	}
	return d, nil
}

// size compiles larger: and smaller:, both strict as IMAP's LARGER and
// SMALLER. larger:0 is every message, as IMAP reads it (go-imap sends 0 as
// no bound), and smaller:0 none.
func (c *compiler) size(n *query.Node) (string, []any, error) {
	size, err := query.ParseSize(n.Value)
	if err != nil {
		return "", nil, badQueryValue(n, err)
	}
	if !c.gmail && size > queryMaxSize {
		return "", nil, fmt.Errorf("%w: %s:%s is %d bytes, and IMAP SEARCH sizes go up to %d bytes",
			mailstore.ErrUnsupportedQuery, n.Field, n.Value, size, int64(queryMaxSize))
	}
	switch {
	case n.Field == query.FieldLarger && size == 0:
		return "1", nil, nil
	case n.Field == query.FieldLarger:
		return "m.size > ?", []any{size}, nil
	case size == 0:
		return "0", nil, nil
	default:
		return "m.size < ?", []any{size}, nil
	}
}

// badQueryValue wraps a parse error for a term in ErrUnsupportedQuery, as
// imapstore's badValue does. Validated trees never get here.
func badQueryValue(n *query.Node, err error) error {
	return fmt.Errorf("%w: %s: %w", mailstore.ErrUnsupportedQuery, n.Field, err)
}

// lowerBound is the latest lower date bound among the direct terms of n's
// top-level AND, or of n itself when it is a term: newer_than or after.
// Zero when there is none.
func lowerBound(n *query.Node, now time.Time) time.Time {
	if n == nil {
		return time.Time{}
	}
	if n.Op != query.OpAnd {
		since, _ := termBound(n, now)
		return since
	}
	var (
		since time.Time
		found bool
	)
	for _, ch := range n.Children {
		if b, ok := termBound(ch, now); ok && (!found || b.After(since)) {
			since, found = b, true
		}
	}
	return since
}

// termBound is the day n bounds the search from below, when n is a
// newer_than or after term with a valid value.
func termBound(n *query.Node, now time.Time) (time.Time, bool) {
	if n == nil || n.Op != query.OpTerm {
		return time.Time{}, false
	}
	switch n.Field {
	case query.FieldNewerThan:
		if _, _, err := query.ParseAge(n.Value); err != nil {
			return time.Time{}, false
		}
		return queryAgeStart(now, n.Value), true
	case query.FieldAfter:
		d, err := query.ParseDate(n.Value)
		return d, err == nil
	default:
		return time.Time{}, false
	}
}

// queryAgeStart is imapstore's ageStart: the day an age such as 7d reaches
// back to. IMAP compares dates without times, so newer_than:1d means since
// the start of yesterday. The day is taken in now's own zone and returned
// at midnight UTC. A month or year back from a day the target month lacks
// is that month's last day: newer_than:1m on 31 March is since 28
// February. The longest age the parser allows, 9999y, still fits a
// time.Time.
func queryAgeStart(now time.Time, v string) time.Time {
	n, unit, _ := query.ParseAge(v) // callers parsed v first
	y, m, d := now.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	switch unit {
	case 'w':
		return day.AddDate(0, 0, -7*n)
	case 'm':
		return queryAddMonths(day, -n)
	case 'y':
		return queryAddMonths(day, -12*n)
	default:
		return day.AddDate(0, 0, -n)
	}
}

// queryAddMonths is imapstore's addMonths: it moves t by months, keeping
// its time of day, and where time.AddDate would spill past the end of a
// shorter target month, as 31 March less a month gives 3 March, it stops at
// that month's last day.
func queryAddMonths(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(months), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	if last := first.AddDate(0, 1, -1).Day(); d > last {
		d = last
	}
	return first.AddDate(0, 0, d-1)
}
