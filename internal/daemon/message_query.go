package daemon

import (
	"fmt"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/query"
)

// parseSearchQuery turns a request's query_string or query into a validated
// tree. Neither means match everything.
func parseSearchQuery(req *chainmailv1.SearchRequest) (*query.Node, error) {
	switch {
	case req.GetQueryString() != "" && req.GetQuery() != nil:
		return nil, &argError{msg: "set query_string or query, not both", hint: "query_string is the text form of the same tree"}
	case req.GetQuery() != nil:
		n, err := queryFromProto(req.GetQuery(), 1)
		if err != nil {
			return nil, err
		}
		if err := query.Validate(n); err != nil {
			return nil, err
		}
		return n, nil
	default:
		return query.Parse(req.GetQueryString())
	}
}

// queryFromProto converts an API query tree. Errors are *query.SyntaxError.
func queryFromProto(q *chainmailv1.Query, depth int) (*query.Node, error) {
	if depth > query.MaxDepth {
		return nil, &query.SyntaxError{Msg: "the query is nested too deeply", Expected: fmt.Sprintf("at most %d levels", query.MaxDepth)}
	}
	switch n := q.GetNode().(type) {
	case *chainmailv1.Query_And:
		children, err := queryListFromProto(n.And.GetNodes(), depth)
		if err != nil {
			return nil, err
		}
		return query.And(children...), nil
	case *chainmailv1.Query_Or:
		children, err := queryListFromProto(n.Or.GetNodes(), depth)
		if err != nil {
			return nil, err
		}
		return query.Or(children...), nil
	case *chainmailv1.Query_Not:
		child, err := queryFromProto(n.Not, depth+1)
		if err != nil {
			return nil, err
		}
		return query.Not(child), nil
	case *chainmailv1.Query_Term:
		return query.Term(query.Field(n.Term.GetField()), n.Term.GetValue()), nil
	default:
		return nil, &query.SyntaxError{Msg: "a query node is empty", Expected: "one of and, or, not or term"}
	}
}

func queryListFromProto(nodes []*chainmailv1.Query, depth int) ([]*query.Node, error) {
	out := make([]*query.Node, 0, len(nodes))
	for _, q := range nodes {
		n, err := queryFromProto(q, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// queryToProto converts a query tree to its API form.
func queryToProto(n *query.Node) *chainmailv1.Query {
	if n == nil {
		return nil
	}
	list := func() *chainmailv1.QueryList {
		l := &chainmailv1.QueryList{}
		for _, c := range n.Children {
			l.Nodes = append(l.Nodes, queryToProto(c))
		}
		return l
	}
	switch n.Op {
	case query.OpAnd:
		return &chainmailv1.Query{Node: &chainmailv1.Query_And{And: list()}}
	case query.OpOr:
		return &chainmailv1.Query{Node: &chainmailv1.Query_Or{Or: list()}}
	case query.OpNot:
		return &chainmailv1.Query{Node: &chainmailv1.Query_Not{Not: queryToProto(n.Children[0])}}
	default:
		return &chainmailv1.Query{Node: &chainmailv1.Query_Term{Term: &chainmailv1.QueryTerm{Field: string(n.Field), Value: n.Value}}}
	}
}
