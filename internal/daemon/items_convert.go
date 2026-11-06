package daemon

import (
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/items"
	"github.com/jamesainslie/chainmail/internal/render"
)

// untrustedBlock wraps text that can run to several lines, an item's
// description or a criterion's evidence, cleaned as a message body is: line
// breaks stay, controls go. Item text is untrusted whoever wrote it, since
// agents copy it from mail.
func untrustedBlock(s string) *chainmailv1.Untrusted {
	return &chainmailv1.Untrusted{Untrusted: render.CleanText(s)}
}

// workItemProto builds the API form of an item. The title is wrapped on one
// line and the description with its line breaks. A zero time stays unset,
// so deleted_at is unset while the item is live.
func workItemProto(it items.Item) *chainmailv1.WorkItem {
	out := &chainmailv1.WorkItem{
		Id: it.ID, Type: string(it.Type), Title: untrusted(it.Title), Description: untrustedBlock(it.Description),
		State: string(it.State), Area: string(it.Area), Priority: string(it.Priority),
		EffectiveArea: string(it.EffectiveArea), EffectivePriority: string(it.EffectivePriority),
		StartDate: it.StartDate, DueDate: it.DueDate, ParentId: it.ParentID, Ongoing: it.Ongoing,
		DeletedAt: timestamp(it.DeletedAt), Revision: it.Revision,
		CreatedAt: timestamp(it.CreatedAt), UpdatedAt: timestamp(it.UpdatedAt),
		CreatedBy: it.CreatedBy, UpdatedBy: it.UpdatedBy,
	}
	if it.Estimate != nil {
		out.Estimate = proto.Float64(*it.Estimate)
	}
	return out
}

// workItemRowProto builds the compact API row of an item.
func workItemRowProto(r items.Row) *chainmailv1.WorkItemRow {
	return &chainmailv1.WorkItemRow{
		Id: r.ID, Type: string(r.Type), Title: untrusted(r.Title), State: string(r.State),
		EffectiveArea: string(r.EffectiveArea), EffectivePriority: string(r.EffectivePriority),
		DueDate: r.DueDate, ParentId: r.ParentID, Ongoing: r.Ongoing, Deleted: !r.DeletedAt.IsZero(),
		Depth: int32(r.Depth), Children: int32(r.Children), CriteriaTotal: int32(r.CriteriaTotal),
		CriteriaTicked: int32(r.CriteriaTicked), Links: int32(r.Links), Revision: r.Revision,
	}
}

// workItemRows builds the API rows of rows, in order.
func workItemRows(rows []items.Row) []*chainmailv1.WorkItemRow {
	out := make([]*chainmailv1.WorkItemRow, len(rows))
	for i, r := range rows {
		out[i] = workItemRowProto(r)
	}
	return out
}

// criterionProto builds the API form of a criterion: its text on one line,
// its evidence with its line breaks.
func criterionProto(c items.Criterion) *chainmailv1.Criterion {
	return &chainmailv1.Criterion{
		Id: c.ID, Position: int32(c.Position), Text: untrusted(c.Text), Ticked: c.Ticked,
		Evidence: untrustedBlock(c.Evidence), TickedAt: timestamp(c.TickedAt), TickedBy: c.TickedBy,
	}
}

// criteriaProto builds the API form of criteria, in order.
func criteriaProto(cs []items.Criterion) []*chainmailv1.Criterion {
	out := make([]*chainmailv1.Criterion, len(cs))
	for i, c := range cs {
		out[i] = criterionProto(c)
	}
	return out
}
