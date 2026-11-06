package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/items"
)

// adminTimeout bounds creating the Admin items when the daemon starts.
const adminTimeout = 10 * time.Second

// maxListLimit is the largest ListItems page.
const maxListLimit = 500

// itemsService implements chainmailv1connect.ItemsServiceHandler. Every
// call is audited, and no audit target, error message, hint or log line
// holds item text: only IDs, types, field names, accounts, thread keys and
// counts. Error text reaches agents outside the untrusted wrapping, so it
// must never repeat what an agent copied from a message into an item.
type itemsService struct {
	s *Server
}

var _ chainmailv1connect.ItemsServiceHandler = (*itemsService)(nil)

// daemonActor is who the daemon's own item changes are recorded as: its
// uid and the client kind daemon.
func daemonActor() items.Actor {
	return items.Actor{UID: uint32(os.Getuid()), Client: daemonClient}
}

// ensureAdmin creates the two Admin items when they are missing. New runs
// it, so the items exist before any client connects. They are bootstrap
// rows: no event, no audit row. A second run changes nothing.
func (s *Server) ensureAdmin() error {
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	if err := s.items.EnsureAdmin(ctx, daemonActor()); err != nil {
		return fmt.Errorf("daemon: creating the Admin items: %w", err)
	}
	return nil
}

// itemListHint points at the list of items, where item IDs come from.
const itemListHint = "run: chainmail item list"

// updateMaskHint lists UpdateItem's mask paths.
const updateMaskHint = "paths are type, title, description, state, area, priority, start_date, due_date, estimate and parent_id"

// actorOf is who a caller's item change is recorded as: its uid and client
// kind, as the audit log records them.
func actorOf(c caller) items.Actor {
	return items.Actor{UID: c.uid, Client: c.client}
}

// itemsErr is the error to report for a failed items store call. The
// store's refusals pass unchanged: their text is fixed and never holds
// item text. Anything else reads as fixed text and keeps its cause, so its
// reason still maps (TIMEOUT for a deadline, else INTERNAL) and the cause
// reaches the daemon's log.
func (s *Server) itemsErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*items.InvalidError](err); ok {
		return err
	}
	if errors.Is(err, items.ErrNotFound) || errors.Is(err, items.ErrKeyMismatch) {
		return err
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.opts.Logger.Error("an items store call failed", "err", err)
	}
	return &fixedTextError{msg: "the item store failed", err: err}
}

// updatePaths are UpdateItem's mask paths. CreateItem reads every one of
// them.
var updatePaths = []string{"type", "title", "description", "state", "area", "priority", "start_date", "due_date", "estimate", "parent_id"}

var (
	errItemID       = &argError{msg: "invalid item ID", hint: "item IDs start with it_; " + itemListHint}
	errEstimate     = &argError{msg: "estimate must be a number, 0 or more", hint: "pass an estimate such as 1.5, or clear it"}
	errUpdateMask   = &argError{msg: "update_mask must name one or more fields", hint: updateMaskHint}
	errUpdatePath   = &argError{msg: "update_mask names an unknown field", hint: updateMaskHint}
	errItemLimit    = &argError{msg: "limit must be 0 to 500", hint: "0 means 100"}
	errChangedSince = &argError{msg: "changed_since must be 0 or more", hint: "pass the revision of an earlier ListItems response"}
	errCriterionID  = &argError{msg: "invalid criterion ID", hint: "criterion IDs start with cr_; run: chainmail item show <id>"}
)

// itemTarget is the audit target key=id, and the error when id cannot be an
// item ID. An invalid ID is recorded as invalidTarget: it can be any bytes.
func itemTarget(key, id string) (string, error) {
	if !items.ValidID(id) {
		return key + "=" + invalidTarget, errItemID
	}
	return key + "=" + id, nil
}

// enumTarget is the audit target key=v for a value that parsed, or
// key=invalidTarget when err says it did not.
func enumTarget[T ~string](key string, v T, err error) string {
	if err != nil {
		return key + "=" + invalidTarget
	}
	return key + "=" + string(v)
}

// itemFields are the item fields CreateItem and UpdateItem share, as sent.
type itemFields struct {
	typ, title, description, state, area, priority, startDate, dueDate, parentID string
	estimate                                                                     *float64
}

// values parses the fields paths names into the items store's form. A
// named field sent empty stays the zero value, which UpdateItem reads as a
// clear and CreateItem as unset; the store refuses clearing what cannot be
// cleared. Text passes through as sent: the store trims and bounds it,
// with errors that never quote it. Every named field is parsed, and the
// first failure in field order is returned.
func (f itemFields) values(paths []string) (items.NewItem, error) {
	named := func(p string) bool { return slices.Contains(paths, p) }
	var v items.NewItem
	if named("title") {
		v.Title = f.title
	}
	if named("description") {
		v.Description = f.description
	}
	err := cmp.Or(
		parseNamed(named("type"), f.typ, orUnset(items.ParseType), &v.Type),
		parseNamed(named("state"), f.state, orUnset(items.ParseState), &v.State),
		parseNamed(named("area"), f.area, items.ParseArea, &v.Area),
		parseNamed(named("priority"), f.priority, items.ParsePriority, &v.Priority),
		parseNamed(named("start_date"), f.startDate, items.ParseDate, &v.StartDate),
		parseNamed(named("due_date"), f.dueDate, items.ParseDate, &v.DueDate),
		parseNamed(named("parent_id"), f.parentID, checkParentID, &v.ParentID),
	)
	if err == nil && named("estimate") {
		v.Estimate, err = checkEstimate(f.estimate)
	}
	if err != nil {
		return items.NewItem{}, err
	}
	return v, nil
}

// parseNamed sets *dst to parse(s) when named, and returns parse's error.
func parseNamed[T any](named bool, s string, parse func(string) (T, error), dst *T) error {
	if !named {
		return nil
	}
	v, err := parse(s)
	*dst = v
	return err
}

// orUnset is parse for ParseType and ParseState, which refuse "": an empty
// value stays the zero value, which the store reads as unset or a clear.
func orUnset[T ~string](parse func(string) (T, error)) func(string) (T, error) {
	return func(s string) (T, error) {
		if s == "" {
			var zero T
			return zero, nil
		}
		return parse(s)
	}
}

// checkParentID checks a parent ID; empty means none.
func checkParentID(id string) (string, error) {
	if id != "" && !items.ValidID(id) {
		return "", errItemID
	}
	return id, nil
}

// checkEstimate copies an estimate, refusing a negative or non-finite one. Nil
// means none.
func checkEstimate(e *float64) (*float64, error) {
	if e == nil {
		return nil, nil
	}
	if *e < 0 || math.IsNaN(*e) || math.IsInf(*e, 0) {
		return nil, errEstimate
	}
	v := *e
	return &v, nil
}

// createFields are a CreateItem request's item fields.
func createFields(m *chainmailv1.CreateItemRequest) itemFields {
	return itemFields{
		typ: m.GetType(), title: m.GetTitle(), description: m.GetDescription(), state: m.GetState(),
		area: m.GetArea(), priority: m.GetPriority(), startDate: m.GetStartDate(), dueDate: m.GetDueDate(),
		parentID: m.GetParentId(), estimate: m.Estimate,
	}
}

// itemCall runs one item call the way every handler of the service does:
// it identifies the caller, gives the call its deadline, and runs run,
// which sets the audit entry's targets as it validates and returns the
// response and the action's error. It then records the entry, so refusals
// are audited too, and returns the response or the action's Connect error.
func itemCall[Req, Resp any](ctx context.Context, s *Server, req *connect.Request[Req], action string,
	run func(ctx context.Context, c caller, entry *audit.Entry) (*Resp, error),
) (*connect.Response[Resp], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	entry := audit.Entry{Action: action}
	resp, runErr := run(ctx, c, &entry)
	if err := s.record(ctx, c, entry, runErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// CreateItem implements chainmailv1connect.ItemsServiceHandler. It is
// audited as item.create with the new item's ID, its type and its number
// of criteria. A retry with the same idempotency_key and request returns
// the first item, marked duplicate, and records decision duplicate.
func (i *itemsService) CreateItem(ctx context.Context, req *connect.Request[chainmailv1.CreateItemRequest]) (*connect.Response[chainmailv1.CreateItemResponse], error) {
	return itemCall(ctx, i.s, req, "item.create", func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.CreateItemResponse, error) {
		typ, err := items.ParseType(req.Msg.GetType())
		entry.Targets = []string{enumTarget("type", typ, err)}
		if err != nil {
			return nil, err
		}
		resp, err := i.create(ctx, actorOf(c), typ, req.Msg)
		if err != nil {
			return nil, err
		}
		entry.Targets = []string{"item=" + resp.GetItem().GetId(), "type=" + string(typ), tally("criteria", len(resp.GetCriteria()))}
		if resp.GetDuplicate() {
			entry.Decision = decisionDuplicate
		}
		return resp, nil
	})
}

// create validates the rest of a CreateItem request and runs it.
func (i *itemsService) create(ctx context.Context, a items.Actor, typ items.Type, m *chainmailv1.CreateItemRequest) (*chainmailv1.CreateItemResponse, error) {
	key := m.GetIdempotencyKey()
	if key != "" && !items.ValidKey(key) {
		return nil, errKeyMalformed
	}
	n, err := createFields(m).values(updatePaths)
	if err != nil {
		return nil, err
	}
	n.Type, n.Criteria, n.IdempotencyKey = typ, m.GetCriteria(), key
	it, crit, duplicate, err := i.s.items.Create(ctx, a, n)
	if err != nil {
		return nil, i.s.itemsErr(err)
	}
	return &chainmailv1.CreateItemResponse{Item: workItemProto(it), Criteria: criteriaProto(crit), Duplicate: duplicate}, nil
}

// maskFields checks an update_mask's paths and returns them without
// repeats, in mask order, with their audit target. A path that is not one
// of updatePaths fails the whole mask and is never recorded: it can be any
// bytes.
func maskFields(paths []string) ([]string, string, error) {
	if len(paths) == 0 {
		return nil, "fields=", errUpdateMask
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if !slices.Contains(updatePaths, p) {
			return nil, "fields=" + invalidTarget, errUpdatePath
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, "fields=" + strings.Join(out, ","), nil
}

// updateFields are an UpdateItem request's item fields.
func updateFields(m *chainmailv1.UpdateItemRequest) itemFields {
	return itemFields{
		typ: m.GetType(), title: m.GetTitle(), description: m.GetDescription(), state: m.GetState(),
		area: m.GetArea(), priority: m.GetPriority(), startDate: m.GetStartDate(), dueDate: m.GetDueDate(),
		parentID: m.GetParentId(), estimate: m.Estimate,
	}
}

// UpdateItem implements chainmailv1connect.ItemsServiceHandler. Only the
// fields update_mask names change; a named field sent empty is cleared. It
// is audited as item.update with the item, the field names in mask order
// and, when it succeeds, the item's type.
func (i *itemsService) UpdateItem(ctx context.Context, req *connect.Request[chainmailv1.UpdateItemRequest]) (*connect.Response[chainmailv1.UpdateItemResponse], error) {
	return itemCall(ctx, i.s, req, "item.update", func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.UpdateItemResponse, error) {
		m := req.Msg
		idTarget, idErr := itemTarget("item", m.GetId())
		fields, fieldsTarget, maskErr := maskFields(m.GetUpdateMask().GetPaths())
		entry.Targets = []string{idTarget, fieldsTarget}
		if err := cmp.Or(idErr, maskErr); err != nil {
			return nil, err
		}
		it, err := i.update(ctx, actorOf(c), m, fields)
		if err != nil {
			return nil, err
		}
		entry.Targets = append(entry.Targets, "type="+string(it.Type))
		return &chainmailv1.UpdateItemResponse{Item: workItemProto(it)}, nil
	})
}

// update parses the named fields of an UpdateItem request and runs it.
func (i *itemsService) update(ctx context.Context, a items.Actor, m *chainmailv1.UpdateItemRequest, fields []string) (items.Item, error) {
	v, err := updateFields(m).values(fields)
	if err != nil {
		return items.Item{}, err
	}
	it, err := i.s.items.Update(ctx, a, m.GetId(), items.Patch{Fields: fields, Values: v})
	return it, i.s.itemsErr(err)
}

// listQuery validates a ListItems request into the store's query, with the
// audit targets of the filters it sets, in field order. A value that fails
// validation is recorded as invalidTarget. The first failure, in field
// order, is returned. A malformed page token is the store's refusal.
func listQuery(m *chainmailv1.ListItemsRequest) (items.ListQuery, []string, error) {
	q := items.ListQuery{
		Inbox: m.GetInbox(), Ongoing: m.Ongoing, IncludeDeleted: m.GetIncludeDeleted(),
		ChangedSince: m.ChangedSince, Limit: int(m.GetLimit()), PageToken: m.GetPageToken(),
	}
	var (
		targets []string
		errs    []error
	)
	if s := m.GetType(); s != "" {
		v, err := items.ParseType(s)
		q.Type = v
		targets, errs = append(targets, enumTarget("type", v, err)), append(errs, err)
	}
	if s := m.GetState(); s != "" {
		v, err := items.ParseState(s)
		q.State = v
		targets, errs = append(targets, enumTarget("state", v, err)), append(errs, err)
	}
	if s := m.GetArea(); s != "" {
		v, err := items.ParseArea(s)
		q.Area = v
		targets, errs = append(targets, enumTarget("area", v, err)), append(errs, err)
	}
	if s := m.GetUnder(); s != "" {
		target, err := itemTarget("under", s)
		q.Under = s
		targets, errs = append(targets, target), append(errs, err)
	}
	if q.Inbox {
		targets = append(targets, "inbox=1")
	}
	if v := m.ChangedSince; v != nil {
		targets = append(targets, fmt.Sprintf("changed_since=%d", *v))
		if *v < 0 {
			errs = append(errs, errChangedSince)
		}
	}
	if q.Limit < 0 || q.Limit > maxListLimit {
		errs = append(errs, errItemLimit)
	}
	return q, targets, cmp.Or(errs...)
}

// ListItems implements chainmailv1connect.ItemsServiceHandler. Rows come in
// tree order, or in revision order with changed_since, with the global
// revision at the time of the read. It is audited as item.list with the
// filters it sets and the number of rows.
func (i *itemsService) ListItems(ctx context.Context, req *connect.Request[chainmailv1.ListItemsRequest]) (*connect.Response[chainmailv1.ListItemsResponse], error) {
	return itemCall(ctx, i.s, req, "item.list", func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.ListItemsResponse, error) {
		q, targets, err := listQuery(req.Msg)
		entry.Targets = targets
		entry.Targets = append(entry.Targets, tally("rows", 0))
		if err != nil {
			return nil, err
		}
		rows, next, revision, err := i.s.items.List(ctx, q)
		if err != nil {
			return nil, i.s.itemsErr(err)
		}
		entry.Targets[len(entry.Targets)-1] = tally("rows", len(rows))
		return &chainmailv1.ListItemsResponse{Items: workItemRows(rows), NextPageToken: next, Revision: revision}, nil
	})
}

// cascade runs Delete or Restore on a valid ID, setting the audit targets:
// the item, cascade=1 when set and, when it succeeds, how many items it
// changed.
func (i *itemsService) cascade(ctx context.Context, c caller, entry *audit.Entry, id string, cascade bool,
	run func(context.Context, items.Actor, string, bool) ([]string, int64, error),
) ([]string, int64, error) {
	target, err := itemTarget("item", id)
	entry.Targets = []string{target}
	if cascade {
		entry.Targets = append(entry.Targets, "cascade=1")
	}
	if err != nil {
		return nil, 0, err
	}
	ids, revision, err := run(ctx, actorOf(c), id, cascade)
	if err != nil {
		return nil, 0, i.s.itemsErr(err)
	}
	entry.Targets = append(entry.Targets, tally("ids", len(ids)))
	return ids, revision, nil
}

// DeleteItem implements chainmailv1connect.ItemsServiceHandler. It soft
// deletes an item, and with cascade its live subtree, in one revision. It
// is audited as item.delete.
func (i *itemsService) DeleteItem(ctx context.Context, req *connect.Request[chainmailv1.DeleteItemRequest]) (*connect.Response[chainmailv1.DeleteItemResponse], error) {
	return itemCall(ctx, i.s, req, "item.delete", func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.DeleteItemResponse, error) {
		ids, revision, err := i.cascade(ctx, c, entry, req.Msg.GetId(), req.Msg.GetCascade(), i.s.items.Delete)
		if err != nil {
			return nil, err
		}
		return &chainmailv1.DeleteItemResponse{Ids: ids, Revision: revision}, nil
	})
}

// RestoreItem implements chainmailv1connect.ItemsServiceHandler. It
// restores a deleted item, and with cascade the items deleted with it, in
// one revision. It is audited as item.restore.
func (i *itemsService) RestoreItem(ctx context.Context, req *connect.Request[chainmailv1.RestoreItemRequest]) (*connect.Response[chainmailv1.RestoreItemResponse], error) {
	return itemCall(ctx, i.s, req, "item.restore", func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.RestoreItemResponse, error) {
		ids, revision, err := i.cascade(ctx, c, entry, req.Msg.GetId(), req.Msg.GetCascade(), i.s.items.Restore)
		if err != nil {
			return nil, err
		}
		return &chainmailv1.RestoreItemResponse{Ids: ids, Revision: revision}, nil
	})
}

// criterionTarget is the audit target criterion=id, and the error when id
// cannot be a criterion ID.
func criterionTarget(id string) (string, error) {
	if !items.ValidCriterionID(id) {
		return "criterion=" + invalidTarget, errCriterionID
	}
	return "criterion=" + id, nil
}

// criteriaEdit is an EditCriteria request in the store's form.
func criteriaEdit(m *chainmailv1.EditCriteriaRequest) items.CriteriaEdit {
	edits := make([]items.CriterionText, len(m.GetEdit()))
	for i, e := range m.GetEdit() {
		edits[i] = items.CriterionText{ID: e.GetId(), Text: e.GetText()}
	}
	return items.CriteriaEdit{Remove: m.GetRemove(), Edit: edits, Order: m.GetOrder(), Add: m.GetAdd()}
}

// EditCriteria implements chainmailv1connect.ItemsServiceHandler. It
// removes, edits, reorders and adds criteria in that order, in one
// revision. It is audited as item.criteria with the item and, when it
// succeeds, the number of criteria after the edit.
func (i *itemsService) EditCriteria(ctx context.Context, req *connect.Request[chainmailv1.EditCriteriaRequest]) (*connect.Response[chainmailv1.EditCriteriaResponse], error) {
	return itemCall(ctx, i.s, req, "item.criteria", func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.EditCriteriaResponse, error) {
		m := req.Msg
		target, err := itemTarget("item", m.GetItemId())
		entry.Targets = []string{target}
		if err != nil {
			return nil, err
		}
		crit, revision, err := i.s.items.EditCriteria(ctx, actorOf(c), m.GetItemId(), criteriaEdit(m))
		if err != nil {
			return nil, i.s.itemsErr(err)
		}
		entry.Targets = append(entry.Targets, tally("criteria", len(crit)))
		return &chainmailv1.EditCriteriaResponse{Criteria: criteriaProto(crit), Revision: revision}, nil
	})
}

// TickCriterion implements chainmailv1connect.ItemsServiceHandler. Ticking
// needs evidence; unticking clears it. It is audited as item.tick with the
// item and the criterion.
func (i *itemsService) TickCriterion(ctx context.Context, req *connect.Request[chainmailv1.TickCriterionRequest]) (*connect.Response[chainmailv1.TickCriterionResponse], error) {
	return itemCall(ctx, i.s, req, "item.tick", func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.TickCriterionResponse, error) {
		m := req.Msg
		itemT, itemErr := itemTarget("item", m.GetItemId())
		critT, critErr := criterionTarget(m.GetCriterionId())
		entry.Targets = []string{itemT, critT}
		if err := cmp.Or(itemErr, critErr); err != nil {
			return nil, err
		}
		crit, revision, err := i.s.items.TickCriterion(ctx, actorOf(c), m.GetItemId(), m.GetCriterionId(), m.GetTicked(), m.GetEvidence())
		if err != nil {
			return nil, i.s.itemsErr(err)
		}
		return &chainmailv1.TickCriterionResponse{Criterion: criterionProto(crit), Revision: revision}, nil
	})
}

// SetBlocker implements chainmailv1connect.ItemsServiceHandler. linked adds
// the blocker and false removes it; a call that changes nothing reports
// changed false at the current revision. It is audited as item.block with
// the item and its blocker.
func (i *itemsService) SetBlocker(ctx context.Context, req *connect.Request[chainmailv1.SetBlockerRequest]) (*connect.Response[chainmailv1.SetBlockerResponse], error) {
	return itemCall(ctx, i.s, req, "item.block", func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.SetBlockerResponse, error) {
		m := req.Msg
		itemT, itemErr := itemTarget("item", m.GetItemId())
		byT, byErr := itemTarget("blocked_by", m.GetBlockedById())
		entry.Targets = []string{itemT, byT}
		if err := cmp.Or(itemErr, byErr); err != nil {
			return nil, err
		}
		changed, revision, err := i.s.items.SetBlocker(ctx, actorOf(c), m.GetItemId(), m.GetBlockedById(), m.GetLinked())
		if err != nil {
			return nil, i.s.itemsErr(err)
		}
		return &chainmailv1.SetBlockerResponse{Changed: changed, Revision: revision}, nil
	})
}
