package cache

import (
	"errors"
	"fmt"
	"strings"
	"time"

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

// likeEscape follows every LIKE: likeEscaper marks %, _ and \ in the value
// with a backslash, so they match themselves.
const likeEscape = ` ESCAPE '\'`

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

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
	if n != nil {
		sql, a, err := c.node(n)
		if err != nil {
			return Plan{}, err
		}
		conds, args = append(conds, sql), append(args, a...)
	}
	p := Plan{Where: Where{SQL: strings.Join(conds, " AND "), Args: args}}
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
