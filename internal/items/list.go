package items

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
)

// List limits.
const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// Refusals of lists. Their text never holds request values.
var (
	errListLimit    = &InvalidError{Msg: "limit must be 1 to 500", Hint: "pass 0 for the default of 100"}
	errPageToken    = &InvalidError{Msg: "the page token is malformed", Hint: "pass next_page_token from the previous page unchanged, with the same filters"}
	errInboxUnder   = &InvalidError{Msg: "inbox and under cannot be combined", Hint: "inbox tasks have no parent; drop one of the two"}
	errChangedSince = &InvalidError{Msg: "changed_since must be zero or more", Hint: "pass the revision an earlier list returned"}
)

// ListQuery selects items. Empty fields match everything.
type ListQuery struct {
	Type           Type
	State          State
	Area           Area // effective
	Under          string
	Inbox          bool
	Ongoing        *bool
	IncludeDeleted bool
	ChangedSince   *int64
	Limit          int // 0 -> 100; 1..500
	PageToken      string
}

// List returns one page of rows, the token of the next page ("" after the
// last), and the global revision the page was read at.
//
// Without ChangedSince, rows come in tree order: depth first, siblings in
// sibling order, deleted items and their subtrees left out unless
// IncludeDeleted. Under lists the items below that one, not including it.
// Filters drop rows but keep the order of the rest, and Depth counts from
// the top of the listed scope whether or not the rows above were dropped.
//
// With ChangedSince, rows are the items with a revision above it, ordered
// by revision then ID, deleted items always included; the other filters
// still apply. A page token there names the last row returned, so an item
// that changes between pages moves to the end instead of being skipped.
func (s *Store) List(ctx context.Context, q ListQuery) ([]Row, string, int64, error) {
	q, limit, tok, err := checkList(q)
	if err != nil {
		return nil, "", 0, err
	}
	var (
		rows     []Row
		next     string
		revision int64
	)
	err = s.read(ctx, func(tx *sql.Tx) error {
		f, err := loadForest(ctx, tx)
		if err != nil {
			return err
		}
		c, err := loadCounts(ctx, tx)
		if err != nil {
			return err
		}
		if revision, err = currentRevision(ctx, tx); err != nil {
			return err
		}
		var under *node
		base := 0
		if q.Under != "" {
			if under, err = f.find(q.Under, "item"); err != nil {
				return err
			}
			base = under.depth + 1
		}
		var matched []*node
		start := 0
		if q.ChangedSince == nil {
			tops := f.roots
			if under != nil {
				tops = under.children
			}
			walk(tops, q.IncludeDeleted, func(n *node) {
				if q.match(n) {
					matched = append(matched, n)
				}
			})
			start = min(tok.offset, len(matched))
		} else {
			for _, n := range f.byID {
				if n.item.Revision > *q.ChangedSince && (under == nil || n.under(under)) && q.match(n) {
					matched = append(matched, n)
				}
			}
			slices.SortFunc(matched, func(a, b *node) int {
				return cmp.Or(cmp.Compare(a.item.Revision, b.item.Revision), strings.Compare(a.item.ID, b.item.ID))
			})
			if tok.kind == tokenRevision {
				start = len(matched)
				if i := slices.IndexFunc(matched, tok.before); i >= 0 {
					start = i
				}
			}
		}
		end := min(start+limit, len(matched))
		for _, n := range matched[start:end] {
			rows = append(rows, row(n, n.depth-base, c))
		}
		if end < len(matched) {
			if q.ChangedSince == nil {
				next = pageToken{kind: tokenTree, offset: end}.String()
			} else {
				last := matched[end-1].item
				next = pageToken{kind: tokenRevision, revision: last.Revision, id: last.ID}.String()
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", 0, err
	}
	return rows, next, revision, nil
}

// checkList validates q and returns it with its values parsed, the page
// size and the decoded page token.
func checkList(q ListQuery) (ListQuery, int, pageToken, error) {
	limit := q.Limit
	if limit == 0 {
		limit = defaultListLimit
	}
	if limit < 1 || limit > maxListLimit {
		return ListQuery{}, 0, pageToken{}, errListLimit
	}
	var err error
	if q.Type != "" {
		if q.Type, err = ParseType(string(q.Type)); err != nil {
			return ListQuery{}, 0, pageToken{}, err
		}
	}
	if q.State != "" {
		if q.State, err = ParseState(string(q.State)); err != nil {
			return ListQuery{}, 0, pageToken{}, err
		}
	}
	if q.Area, err = ParseArea(string(q.Area)); err != nil {
		return ListQuery{}, 0, pageToken{}, err
	}
	if q.Inbox && q.Under != "" {
		return ListQuery{}, 0, pageToken{}, errInboxUnder
	}
	if q.ChangedSince != nil && *q.ChangedSince < 0 {
		return ListQuery{}, 0, pageToken{}, errChangedSince
	}
	tok, err := parseToken(q.PageToken)
	if err != nil {
		return ListQuery{}, 0, pageToken{}, err
	}
	want := tokenTree
	if q.ChangedSince != nil {
		want = tokenRevision
	}
	if tok.kind != "" && tok.kind != want {
		return ListQuery{}, 0, pageToken{}, errPageToken
	}
	return q, limit, tok, nil
}

// match reports whether n passes the filters other than Under and
// ChangedSince, which List applies while walking.
func (q ListQuery) match(n *node) bool {
	it := n.item
	switch {
	case q.Type != "" && it.Type != q.Type,
		q.State != "" && it.State != q.State,
		q.Area != "" && it.EffectiveArea != q.Area,
		q.Ongoing != nil && it.Ongoing != *q.Ongoing,
		q.Inbox && (it.Type != TypeTask || it.ParentID != "" || !n.live()):
		return false
	}
	return true
}

// walk visits nodes and their descendants depth first, in sibling order.
// Deleted items and everything below them are skipped unless withDeleted.
func walk(nodes []*node, withDeleted bool, visit func(*node)) {
	for _, n := range nodes {
		if !withDeleted && !n.live() {
			continue
		}
		visit(n)
		walk(n.children, withDeleted, visit)
	}
}

// Page token kinds.
const (
	tokenTree     = "t" // t:<offset>
	tokenRevision = "r" // r:<revision>:<id>
)

// pageToken is a decoded page token. The zero value is no token.
type pageToken struct {
	kind     string
	offset   int
	revision int64
	id       string
}

// String encodes t as base64url without padding; the zero token is "".
func (t pageToken) String() string {
	var raw string
	switch t.kind {
	case tokenTree:
		raw = tokenTree + ":" + strconv.Itoa(t.offset)
	case tokenRevision:
		raw = tokenRevision + ":" + strconv.FormatInt(t.revision, 10) + ":" + t.id
	default:
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// before reports whether n comes after the row t names, in changed_since order.
func (t pageToken) before(n *node) bool {
	return n.item.Revision > t.revision || (n.item.Revision == t.revision && n.item.ID > t.id)
}

// parseToken decodes s. Only the exact encoding String produces is
// accepted; anything else is errPageToken.
func parseToken(s string) (pageToken, error) {
	if s == "" {
		return pageToken{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return pageToken{}, errPageToken
	}
	kind, rest, _ := strings.Cut(string(b), ":")
	var t pageToken
	switch kind {
	case tokenTree:
		n, err := strconv.Atoi(rest)
		if err != nil || n < 0 {
			return pageToken{}, errPageToken
		}
		t = pageToken{kind: tokenTree, offset: n}
	case tokenRevision:
		revText, id, ok := strings.Cut(rest, ":")
		rev, err := strconv.ParseInt(revText, 10, 64)
		if !ok || err != nil || rev < 0 || !ValidID(id) {
			return pageToken{}, errPageToken
		}
		t = pageToken{kind: tokenRevision, revision: rev, id: id}
	default:
		return pageToken{}, errPageToken
	}
	if t.String() != s {
		return pageToken{}, errPageToken
	}
	return t, nil
}
