package daemon

import (
	"context"
	"time"

	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/query"
)

// relaxed is what relaxing a query's text terms left of a node.
type relaxed int

const (
	// relaxedNode is a condition on headers, labels and flags alone.
	relaxedNode relaxed = iota
	// relaxedTrue matches every message.
	relaxedTrue
	// relaxedFalse matches no message.
	relaxedFalse
)

// relaxText returns n with its text terms relaxed away, so that it needs
// no message body. With superset it matches every message n can match
// whatever the bodies hold, a text term reading as true; without it, only
// messages n matches, a text term reading as false. A NOT flips the
// direction for its child. The node is nil unless the verdict is
// relaxedNode, and it shares n's terms.
func relaxText(n *query.Node, superset bool) (*query.Node, relaxed) {
	switch n.Op {
	case query.OpTerm:
		switch {
		case n.Field != query.FieldText:
			return n, relaxedNode
		case superset:
			return nil, relaxedTrue
		default:
			return nil, relaxedFalse
		}
	case query.OpNot:
		if len(n.Children) != 1 {
			return n, relaxedNode // malformed: Compile refuses it
		}
		child, v := relaxText(n.Children[0], !superset)
		switch v {
		case relaxedTrue:
			return nil, relaxedFalse
		case relaxedFalse:
			return nil, relaxedTrue
		default:
			return query.Not(child), relaxedNode
		}
	case query.OpAnd, query.OpOr:
		// absorb decides the whole node; neutral drops out of it.
		absorb, neutral := relaxedFalse, relaxedTrue
		if n.Op == query.OpOr {
			absorb, neutral = relaxedTrue, relaxedFalse
		}
		var kept []*query.Node
		for _, ch := range n.Children {
			c, v := relaxText(ch, superset)
			switch v {
			case absorb:
				return nil, absorb
			case neutral:
				continue
			default:
				kept = append(kept, c)
			}
		}
		switch len(kept) {
		case 0:
			return nil, neutral
		case 1:
			return kept[0], relaxedNode
		default:
			return &query.Node{Op: n.Op, Children: kept}, relaxedNode
		}
	default:
		return n, relaxedNode // malformed: Compile refuses it
	}
}

// setHeaderFilter sets sc's header-only filter for a query with text
// terms: rest, the query without in: and account:, relaxed to a superset
// that needs no body and compiled as scopeRule compiles rest. A batch
// message the filter refuses cannot match whatever its body holds, so a
// text rule never waits for its body or counts it as failed.
func (sc *ruleScope) setHeaderFilter(rest *query.Node, in string, now time.Time, threads map[string][]string) error {
	var node *query.Node
	if rest != nil {
		var v relaxed
		node, v = relaxText(rest, true)
		switch v {
		case relaxedFalse:
			sc.headerNone = true
			return nil
		case relaxedTrue:
			node = nil
		}
	}
	plan, err := cache.Compile(node, in, sc.acct.Provider.Gmail, now, threads)
	if err != nil {
		return err
	}
	sc.header = plan.Where
	return nil
}

// headerCandidates keeps the batch UIDs, uids, whose cached rows pass sc's
// header-only filter in sc's sources without its excluded labels: the
// messages a text rule can match once their bodies are known. A message
// without a row is dropped, as matchBatch drops it (amendment 11).
func (s *Server) headerCandidates(ctx context.Context, sc ruleScope, uids map[string][]uint32) (map[string][]uint32, error) {
	if sc.headerNone {
		return nil, nil
	}
	hs := sc
	hs.where = sc.header
	rows, err := s.matchBatch(ctx, hs, uids)
	if err != nil {
		return nil, err
	}
	out := map[string][]uint32{}
	for _, row := range rows {
		out[row.Folder] = append(out[row.Folder], row.UID)
	}
	return out, nil
}
