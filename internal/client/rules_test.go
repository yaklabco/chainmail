package client

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
)

// stubRuleTime is the time stubRules stamps on what it returns.
var stubRuleTime = time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)

// stubRules answers the RulesService calls the client tests make from
// their requests, so a test can see each field arrive. It sends the client
// header of every call on headers, and fails a call once headers is full.
// accountBytes, when set, makes the first runner's account that long.
type stubRules struct {
	chainmailv1connect.UnimplementedRulesServiceHandler
	headers      chan<- string
	accountBytes int
}

// record sends req's client header on headers. A call past the buffer the
// test made for its calls fails at once instead of blocking on the send.
func (s stubRules) record(req connect.AnyRequest) error {
	select {
	case s.headers <- req.Header().Get(api.HeaderClient):
		return nil
	default:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("stubRules: a call past the headers buffer"))
	}
}

// CreateRule returns the definition as a saved rule, filling the defaults
// a daemon fills: priority 100 when unset, max_per_run 500 when 0, and
// enabled when unset.
func (s stubRules) CreateRule(_ context.Context, req *connect.Request[chainmailv1.CreateRuleRequest]) (*connect.Response[chainmailv1.CreateRuleResponse], error) {
	if err := s.record(req); err != nil {
		return nil, err
	}
	d := req.Msg.GetRule()
	priority := int32(100)
	if d.Priority != nil {
		priority = d.GetPriority()
	}
	perRun := d.GetMaxPerRun()
	if perRun == 0 {
		perRun = 500
	}
	return connect.NewResponse(&chainmailv1.CreateRuleResponse{Rule: &chainmailv1.Rule{
		Id: "ru_new", Name: d.GetName(), Description: &chainmailv1.Untrusted{Untrusted: d.GetDescription()},
		Accounts: d.GetAccounts(), Query: &chainmailv1.Untrusted{Untrusted: d.GetQuery()}, OnNew: d.GetOnNew(),
		Schedule: d.GetSchedule(), Priority: priority, Stop: d.GetStop(), Actions: d.GetActions(),
		MaxPerRun: perRun, Enabled: d.Enabled == nil || d.GetEnabled(), Revision: 1,
		CreatedAt: timestamppb.New(stubRuleTime), UpdatedAt: timestamppb.New(stubRuleTime),
		CreatedBy: "1000/test", UpdatedBy: "1000/test",
	}}), nil
}

// UpdateRule echoes the mask paths in the rule's name, so the test sees
// them arrive in order, and returns the revision after expected_revision.
func (s stubRules) UpdateRule(_ context.Context, req *connect.Request[chainmailv1.UpdateRuleRequest]) (*connect.Response[chainmailv1.UpdateRuleResponse], error) {
	if err := s.record(req); err != nil {
		return nil, err
	}
	m := req.Msg
	var revision int64
	if m.ExpectedRevision != nil {
		revision = m.GetExpectedRevision() + 1
	}
	return connect.NewResponse(&chainmailv1.UpdateRuleResponse{Rule: &chainmailv1.Rule{
		Id: m.GetRule(), Name: strings.Join(m.GetUpdateMask().GetPaths(), ","),
		Enabled: m.GetPatch().GetEnabled(), Revision: revision,
	}}), nil
}

// RulesStatus reports two runners and a next scheduled run, leaving the
// next due check unset.
func (s stubRules) RulesStatus(_ context.Context, req *connect.Request[chainmailv1.RulesStatusRequest]) (*connect.Response[chainmailv1.RulesStatusResponse], error) {
	if err := s.record(req); err != nil {
		return nil, err
	}
	account := "work"
	if s.accountBytes > 0 {
		account = strings.Repeat("x", s.accountBytes)
	}
	return connect.NewResponse(&chainmailv1.RulesStatusResponse{
		Runners: []*chainmailv1.RulesRunnerStatus{
			{Account: account, State: "waiting_for_cache", AckedId: 41, Lag: 2},
			{Account: "home", State: "cache_disabled"},
		},
		NextScheduledAt: timestamppb.New(stubRuleTime),
	}), nil
}

// serveRules serves h as the RulesService on a new Unix socket and returns
// the socket path.
func serveRules(t *testing.T, h chainmailv1connect.RulesServiceHandler) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(chainmailv1connect.NewRulesServiceHandler(h))
	return serveUnix(t, &http.Server{Handler: mux})
}

// TestRulesReachesTheDaemon checks that the Rules client calls
// RulesService methods over the socket, names the client kind, and carries
// optional presence, the field mask, actions in order and timestamps both
// ways.
func TestRulesReachesTheDaemon(t *testing.T) {
	t.Parallel()
	headers := make(chan string, 3)
	c := New(serveRules(t, stubRules{headers: headers}), "test")
	defer c.Close()

	zero := int32(0)
	actions := []*chainmailv1.RuleAction{
		{Kind: "label", Label: "Receipts"},
		{Kind: "link_item", ItemId: "it_admin_work", Anchor: true},
		{Kind: "forward", To: []string{"books@example.com", "me@example.com"}, Note: "for the books"},
		{Kind: "archive"},
	}
	created, err := c.Rules.CreateRule(t.Context(), connect.NewRequest(&chainmailv1.CreateRuleRequest{Rule: &chainmailv1.RuleDefinition{
		Name: "receipts", Description: "file receipts", Accounts: []string{"work"}, Query: "from:billing@example.com",
		OnNew: true, Schedule: "every:6h", Priority: &zero, Stop: true, Actions: actions,
	}}))
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	r := created.Msg.GetRule()
	if r.GetId() != "ru_new" || r.GetName() != "receipts" || r.GetDescription().GetUntrusted() != "file receipts" ||
		r.GetQuery().GetUntrusted() != "from:billing@example.com" || !r.GetOnNew() || r.GetSchedule() != "every:6h" || !r.GetStop() {
		t.Errorf("created rule = %v, want ru_new with the definition as sent", r)
	}
	if got := r.GetAccounts(); len(got) != 1 || got[0] != "work" {
		t.Errorf("accounts = %q, want [work]", got)
	}
	if r.GetPriority() != 0 {
		t.Errorf("priority = %d, want 0: a set zero must arrive as set, not as the default", r.GetPriority())
	}
	if r.GetMaxPerRun() != 500 || !r.GetEnabled() {
		t.Errorf("max_per_run %d, enabled %t; want the defaults 500 and true for unset fields", r.GetMaxPerRun(), r.GetEnabled())
	}
	if got := r.GetActions(); len(got) != len(actions) {
		t.Errorf("actions = %v, want %d", got, len(actions))
	} else {
		for i := range actions {
			if !proto.Equal(got[i], actions[i]) {
				t.Errorf("action %d = %v, want %v", i, got[i], actions[i])
			}
		}
	}
	if !r.GetCreatedAt().AsTime().Equal(stubRuleTime) || r.GetDeletedAt() != nil || r.GetNextRunAt() != nil {
		t.Errorf("created_at %v, deleted_at %v, next_run_at %v; want %v and two unset", r.GetCreatedAt(), r.GetDeletedAt(), r.GetNextRunAt(), stubRuleTime)
	}

	off, seen := false, int64(3)
	updated, err := c.Rules.UpdateRule(t.Context(), connect.NewRequest(&chainmailv1.UpdateRuleRequest{
		Rule: "receipts", Patch: &chainmailv1.RuleDefinition{Enabled: &off},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"enabled", "schedule"}}, ExpectedRevision: &seen,
	}))
	if err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	u := updated.Msg.GetRule()
	if u.GetId() != "receipts" || u.GetName() != "enabled,schedule" || u.GetEnabled() || u.GetRevision() != 4 {
		t.Errorf("updated = %v, want the name receipts, mask enabled,schedule, disabled, revision 4", u)
	}

	status, err := c.Rules.RulesStatus(t.Context(), connect.NewRequest(&chainmailv1.RulesStatusRequest{}))
	if err != nil {
		t.Fatalf("RulesStatus: %v", err)
	}
	runners := status.Msg.GetRunners()
	if len(runners) != 2 || runners[0].GetAccount() != "work" || runners[0].GetState() != "waiting_for_cache" ||
		runners[0].GetAckedId() != 41 || runners[0].GetLag() != 2 || runners[1].GetState() != "cache_disabled" {
		t.Errorf("runners = %v, want work waiting for the cache at 41 with lag 2, then home cache_disabled", runners)
	}
	if !status.Msg.GetNextScheduledAt().AsTime().Equal(stubRuleTime) || status.Msg.GetNextDueCheckAt() != nil {
		t.Errorf("next scheduled %v, next due check %v; want %v and unset", status.Msg.GetNextScheduledAt(), status.Msg.GetNextDueCheckAt(), stubRuleTime)
	}

	for range 3 {
		if got := <-headers; got != "test" {
			t.Errorf("%s = %q, want test", api.HeaderClient, got)
		}
	}

	// The stub leaves DeleteRule to the generated handler, which proves the
	// call reached RulesService and not some other route.
	if _, err := c.Rules.DeleteRule(t.Context(), connect.NewRequest(&chainmailv1.DeleteRuleRequest{Rule: "receipts"})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("DeleteRule: code %v (%v), want %v", connect.CodeOf(err), err, connect.CodeUnimplemented)
	}
}

// TestRulesReadLimit checks that the Rules client reads a response as large
// as the daemon's 16 MiB cap, as a dry run full of message rows can be, and
// refuses one past maxMessageBytes.
func TestRulesReadLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		account int
		refused bool
	}{
		{name: "16 MiB is read", account: 16 << 20},
		{name: "18 MiB is refused", account: 18 << 20, refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := make(chan string, 1)
			c := New(serveRules(t, stubRules{headers: headers, accountBytes: tt.account}), "test")
			defer c.Close()
			_, err := c.Rules.RulesStatus(t.Context(), connect.NewRequest(&chainmailv1.RulesStatusRequest{}))
			switch {
			case !tt.refused && err != nil:
				t.Errorf("RulesStatus: %v, want success", err)
			case tt.refused && connect.CodeOf(err) != connect.CodeResourceExhausted:
				t.Errorf("RulesStatus: code %v (%v), want %v", connect.CodeOf(err), err, connect.CodeResourceExhausted)
			}
		})
	}
}
