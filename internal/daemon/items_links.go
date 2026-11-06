package daemon

import (
	"context"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/audit"
)

// maxViewRows caps GetItem's children and its threads.
const maxViewRows = 50

// GetItem implements chainmailv1connect.ItemsServiceHandler. It returns the
// item, deleted or not, with its criteria, ancestors, children, blockers
// and linked threads. It is audited as item.get with the item and, when it
// succeeds, its type.
func (i *itemsService) GetItem(ctx context.Context, req *connect.Request[chainmailv1.GetItemRequest]) (*connect.Response[chainmailv1.GetItemResponse], error) {
	return itemCall(ctx, i.s, req, "item.get", func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.GetItemResponse, error) {
		id := req.Msg.GetId()
		target, err := itemTarget("item", id)
		entry.Targets = []string{target}
		if err != nil {
			return nil, err
		}
		resp, err := i.s.itemView(ctx, id)
		if err != nil {
			return nil, err
		}
		entry.Targets = append(entry.Targets, "type="+resp.GetItem().GetType())
		return resp, nil
	})
}

// itemView builds GetItem's answer from the items store: the item, its
// criteria, its ancestors from the root, up to maxViewRows live children
// with their total, its live blockers both ways, and how many threads are
// linked. Every read is its own short query; nothing is held open.
func (s *Server) itemView(ctx context.Context, id string) (*chainmailv1.GetItemResponse, error) {
	it, err := s.items.Get(ctx, id)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	crit, err := s.items.Criteria(ctx, id)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	ancestors, err := s.items.Ancestors(ctx, id)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	children, total, err := s.items.Children(ctx, id, maxViewRows)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	blockedBy, blocks, err := s.items.Blockers(ctx, id)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	links, err := s.items.Links(ctx, id)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	return &chainmailv1.GetItemResponse{
		Item: workItemProto(it), Criteria: criteriaProto(crit), Ancestors: workItemRows(ancestors),
		Children: workItemRows(children), ChildrenTotal: int32(total),
		BlockedBy: workItemRows(blockedBy), Blocks: workItemRows(blocks),
		ThreadsTotal: int32(len(links)),
	}, nil
}
