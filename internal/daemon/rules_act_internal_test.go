package daemon

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/idempotency"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/rules"
	"github.com/yaklabco/chainmail/internal/sender"
	"github.com/yaklabco/chainmail/internal/testutil/smtptest"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

// TestClassifyReason checks spec 5.5's classes: an account or deadline
// trouble and an internal failure are transient, a held send is held, an
// outcome that may have happened counts as done, and every other reason,
// known or new, is permanent.
func TestClassifyReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason string
		want   actClass
	}{
		{"", actOK},
		{api.ReasonSendOutcomeUnknown, actOK},
		{api.ReasonHeld, actHeld},
		{api.ReasonServerUnavailable, actTransient},
		{api.ReasonTimeout, actTransient},
		{api.ReasonAuthRequired, actTransient},
		{api.ReasonAuthRejected, actTransient},
		{api.ReasonCacheNotReady, actTransient},
		{api.ReasonInternal, actTransient},
		{api.ReasonInvalidArgument, actPermanent},
		{api.ReasonNotFound, actPermanent},
		{api.ReasonStaleRef, actPermanent},
		{api.ReasonMalformedResponse, actPermanent},
		{api.ReasonPolicyDenied, actPermanent},
		{api.ReasonForbidden, actPermanent},
		{"SOMETHING_NEW", actPermanent},
	}
	for _, tt := range tests {
		if got := classifyReason(tt.reason); got != tt.want {
			t.Errorf("classifyReason(%q) = %d, want %d", tt.reason, got, tt.want)
		}
	}
}

// TestActRunsActionsInOrder runs three actions on a Google message: each
// goes through the organize path as the daemon's uid and client kind rule,
// in order, and its audit row carries the ref, the rule and the run.
func TestActRunsActionsInOrder(t *testing.T) {
	t.Parallel()
	g := newActRig(t, nil)
	rule := g.newRule(t, "gmail", "file-acme", func(d *rules.Definition) {
		d.Actions = []rules.Action{{Kind: rules.ActFlag}, {Kind: rules.ActLabel, Label: "Receipts"}, {Kind: rules.ActArchive}}
	})
	inv := g.refs["gmail invoice"]
	res := g.f.s.act(t.Context(), rule, 7, g.acct(t, "gmail"), g.row(t, "gmail invoice"))
	if res.class != actOK || res.newRef != "" || res.note != "" || res.reason != "" {
		t.Fatalf("act = %+v, want ok with the ref kept", res)
	}
	labels := labelsOf(t, g.f.fakes["gmail"], inv)
	if !slices.Contains(labels, "Receipts") || slices.Contains(labels, `\Inbox`) {
		t.Errorf("labels = %q, want Receipts and no \\Inbox", labels)
	}
	rows := g.audit(t)
	if got := auditActions(rows); !slices.Equal(got, []string{"flags", "labels", "archive"}) {
		t.Fatalf("audited %v, want flags, labels, archive", got)
	}
	want := []string{inv.String(), "rule=" + rule.ID, "run=7"}
	for _, r := range rows {
		if r.uid != uint32(os.Getuid()) || r.account != "gmail" || r.result != "ok" || !slices.Equal(r.targets, want) {
			t.Errorf("audit row %+v, want uid %d, gmail, ok, targets %v", r, os.Getuid(), want)
		}
	}
}

// TestActReportsTheNewRef moves a message off Google, which gives it a
// new UID in the destination: act reports where it went.
func TestActReportsTheNewRef(t *testing.T) {
	t.Parallel()
	g := newActRig(t, nil)
	rule := g.newRule(t, "host", "archive-lunch", func(d *rules.Definition) {
		d.Actions = []rules.Action{{Kind: rules.ActMove, Folder: "Archive"}}
	})
	res := g.f.s.act(t.Context(), rule, 1, g.acct(t, "host"), g.row(t, "host lunch"))
	if res.class != actOK {
		t.Fatalf("act = %+v, want ok", res)
	}
	if moved := g.foundIn(t, "host", "Archive", "lunch@x"); res.newRef != moved.String() {
		t.Errorf("newRef = %q, want %q", res.newRef, moved.String())
	}
}

// TestActClassifiesFailures checks that the first failing action ends the
// message with its class, reason and fixed hint, and that the actions
// after it never run.
func TestActClassifiesFailures(t *testing.T) {
	t.Parallel()
	g := newActRig(t, nil)
	ctx := t.Context()
	acct := g.acct(t, "host")
	it, _, _, err := g.f.s.items.Create(ctx, ruleActor, items.NewItem{Type: items.TypeTask, Title: "Gone", Area: items.AreaWork})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.f.s.items.Delete(ctx, ruleActor, it.ID, false); err != nil {
		t.Fatal(err)
	}
	host := g.f.fakes["host"]
	tests := []struct {
		name         string
		actions      []rules.Action
		before       func()
		class        actClass
		reason, hint string
	}{
		{
			name: "missing folder", actions: []rules.Action{{Kind: rules.ActMove, Folder: "Nowhere"}},
			class: actPermanent, reason: api.ReasonNotFound, hint: folderHint,
		},
		{
			name: "label off Google", actions: []rules.Action{{Kind: rules.ActLabel, Label: "Receipts"}},
			class: actPermanent, reason: api.ReasonInvalidArgument, hint: "this account does not support that action",
		},
		{
			name: "server down", actions: []rules.Action{{Kind: rules.ActArchive}},
			before: func() { host.FailWrite("Archive", mailstore.ErrUnavailable) },
			class:  actTransient, reason: api.ReasonServerUnavailable,
		},
		{
			name: "deleted item", actions: []rules.Action{{Kind: rules.ActLinkItem, ItemID: it.ID}},
			class: actPermanent, reason: api.ReasonInvalidArgument, hint: errItemDeleted.hint,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := g.newRule(t, "host", fmt.Sprintf("fails-%d", i), func(d *rules.Definition) {
				d.Actions = slices.Concat(tt.actions, []rules.Action{{Kind: rules.ActFlag}})
			})
			flags := host.Calls("StoreFlags")
			if tt.before != nil {
				tt.before()
			}
			res := g.f.s.act(t.Context(), rule, 1, acct, g.row(t, "host lunch"))
			if res.class != tt.class || res.reason != tt.reason || res.hint != tt.hint {
				t.Errorf("act = %+v, want class %d, %s, hint %q", res, tt.class, tt.reason, tt.hint)
			}
			if n := host.Calls("StoreFlags") - flags; n != 0 {
				t.Errorf("the flag after the failure ran %d times", n)
			}
		})
	}
}

// TestActLinksTheThread files a message's thread into an item through
// the LinkThread path, audited as item.link under client kind rule.
func TestActLinksTheThread(t *testing.T) {
	t.Parallel()
	g := newActRig(t, nil)
	ctx := t.Context()
	it, _, _, err := g.f.s.items.Create(ctx, ruleActor, items.NewItem{Type: items.TypeTask, Title: "Trip", Area: items.AreaWork})
	if err != nil {
		t.Fatal(err)
	}
	rule := g.newRule(t, "gmail", "file-trip", func(d *rules.Definition) {
		d.Actions = []rules.Action{{Kind: rules.ActLinkItem, ItemID: it.ID, Anchor: true}}
	})
	row := g.row(t, "gmail trip")
	if res := g.f.s.act(ctx, rule, 3, g.acct(t, "gmail"), row); res.class != actOK {
		t.Fatalf("act = %+v, want ok", res)
	}
	links, err := g.f.s.items.Links(ctx, it.ID)
	if err != nil || len(links) != 1 || links[0].Account != "gmail" || links[0].ThreadKey != row.ThreadID {
		t.Fatalf("links = %+v, %v, want the trip's thread on gmail", links, err)
	}
	rows := g.audit(t)
	if len(rows) != 1 || rows[0].action != "item.link" {
		t.Fatalf("audit = %+v, want one item.link row", rows)
	}
	for _, target := range []string{"item=" + it.ID, "rule=" + rule.ID, "run=3"} {
		if !slices.Contains(rows[0].targets, target) {
			t.Errorf("targets %v lack %s", rows[0].targets, target)
		}
	}
}

// TestActForwardHeldWritesPendingFirst forwards to an address the policy
// does not allow: the forward is held as a draft, which is held and not
// an error, and its pending row was written before it went to the
// compose path, with the run that wrote it.
func TestActForwardHeldWritesPendingFirst(t *testing.T) {
	t.Parallel()
	g := newActRig(t, nil)
	g.password(t)
	g.failDial(t, false)
	rule := g.newRule(t, "host", "fwd-blocked", func(d *rules.Definition) {
		d.Actions = []rules.Action{{Kind: rules.ActForward, To: []string{"you@blocked.invalid"}}}
	})
	res := g.f.s.act(t.Context(), rule, 9, g.acct(t, "host"), g.row(t, "host invoice"))
	if res.class != actHeld || res.reason != "" {
		t.Fatalf("act = %+v, want held", res)
	}
	if n := len(g.f.fakes["host"].Appended()); n != 1 {
		t.Errorf("drafts appended = %d, want 1", n)
	}
	row, ok := g.appliedRow(t, rule.ID, "host", keyOf(false, g.refs["host invoice"]))
	if !ok || row.Outcome != rules.AppliedPending || row.RunID != 9 {
		t.Errorf("act-once row = %+v, %t, want pending from run 9", row, ok)
	}
	rows := g.audit(t)
	if len(rows) != 1 || rows[0].action != "forward" || rows[0].decision != "held" ||
		!slices.Contains(rows[0].targets, "rule="+rule.ID) || !slices.Contains(rows[0].targets, "run=9") {
		t.Errorf("audit = %+v, want one held forward with the rule and run", rows)
	}
}

// TestActForwardSends forwards to an allowed address over a real SMTP
// server, with the rule's note, under the rule's idempotency key.
func TestActForwardSends(t *testing.T) {
	t.Parallel()
	g := newActRig(t, nil)
	ca := tlstest.New(t)
	srv := smtptest.StartWith(t, ca, provider.SecurityStartTLS, smtptest.Options{},
		smtptest.User{Username: "me@example.com", Password: "pw"})
	g.f.s.opts.RootCAs = ca.Pool
	g.f.s.compose.dial = func(ctx context.Context, _ provider.Endpoint, creds auth.Credentials, opts sender.DialOptions) (*sender.Conn, error) {
		return sender.Dial(ctx, srv.Endpoint(), creds, opts)
	}
	g.password(t)
	rule := g.newRule(t, "host", "fwd-books", func(d *rules.Definition) {
		d.Actions = []rules.Action{{Kind: rules.ActForward, To: []string{"you@example.net"}, Note: "for the books"}}
	})
	res := g.f.s.act(t.Context(), rule, 4, g.acct(t, "host"), g.row(t, "host invoice"))
	if res.class != actOK || res.note != "" {
		t.Fatalf("act = %+v, want ok", res)
	}
	msgs := srv.Messages()
	if len(msgs) != 1 || !slices.Equal(msgs[0].To, []string{"you@example.net"}) {
		t.Fatalf("SMTP got %+v, want one message to you@example.net", msgs)
	}
	key := rules.ForwardKey(rule.ID, "host", keyOf(false, g.refs["host invoice"]))
	if st, ok, err := g.f.s.idem.Lookup(t.Context(), "host", key); err != nil || !ok || st.State != idempotency.Done {
		t.Errorf("idempotency key %s = %+v, %t, %v, want done", key, st, ok, err)
	}
}

// TestActCopyGuard checks the pending row around a copy: written just
// before the copy runs, left for the runner when the copy ran, and marked
// for clearing when the copy failed for good. A failure before the copy
// writes none.
func TestActCopyGuard(t *testing.T) {
	t.Parallel()
	g := newActRig(t, nil)
	acct := g.acct(t, "host")
	key := keyOf(false, g.refs["host lunch"])
	tests := []struct {
		name    string
		actions []rules.Action
		class   actClass
		pending bool
		clear   bool
		copied  bool
	}{
		{name: "copy", actions: []rules.Action{{Kind: rules.ActCopy, Folder: "Archive"}}, class: actOK, pending: true, copied: true},
		{name: "missing folder", actions: []rules.Action{{Kind: rules.ActCopy, Folder: "Nowhere"}}, class: actPermanent, pending: true, clear: true},
		{
			name:    "failure before the copy",
			actions: []rules.Action{{Kind: rules.ActLabel, Label: "X"}, {Kind: rules.ActCopy, Folder: "Archive"}},
			class:   actPermanent,
		},
		{
			name:    "failure after the copy",
			actions: []rules.Action{{Kind: rules.ActCopy, Folder: "Archive"}, {Kind: rules.ActMove, Folder: "Nowhere"}},
			class:   actPermanent, pending: true, copied: true,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := g.newRule(t, "host", fmt.Sprintf("copy-%d", i), func(d *rules.Definition) { d.Actions = tt.actions })
			res := g.f.s.act(t.Context(), rule, 5, acct, g.row(t, "host lunch"))
			if res.class != tt.class || res.clearPending != tt.clear {
				t.Errorf("act = %+v, want class %d, clearPending %t", res, tt.class, tt.clear)
			}
			if tt.copied {
				c, err := ref.Parse(res.copyRef)
				if err != nil || c.Folder != "Archive" {
					t.Errorf("copyRef = %q, want a ref into Archive", res.copyRef)
				}
			} else if res.copyRef != "" {
				t.Errorf("copyRef = %q, want none", res.copyRef)
			}
			row, ok := g.appliedRow(t, rule.ID, "host", key)
			if ok != tt.pending || (ok && (row.Outcome != rules.AppliedPending || row.RunID != 5)) {
				t.Errorf("act-once row = %+v, %t, want pending %t", row, ok, tt.pending)
			}
		})
	}
}

// TestActReplaysPendingRows replays a message whose pending row an earlier
// attempt left: the copy is not made again, a forward that may have gone
// is asked of the idempotency store, and one older than the store's keys
// is not sent at all.
func TestActReplaysPendingRows(t *testing.T) {
	t.Parallel()
	g := newActRig(t, nil)
	g.failDial(t, true)
	acct := g.acct(t, "host")
	host := g.f.fakes["host"]
	fresh := &rules.AppliedRow{Outcome: rules.AppliedPending, RunID: 1, At: routeNow}
	old := &rules.AppliedRow{Outcome: rules.AppliedPending, RunID: 1, At: routeNow.Add(-7 * 24 * time.Hour)}

	t.Run("copy", func(t *testing.T) {
		rule := g.newRule(t, "host", "replay-copy", func(d *rules.Definition) {
			d.Actions = []rules.Action{{Kind: rules.ActCopy, Folder: "Archive"}, {Kind: rules.ActFlag}}
		})
		copies, flags := host.Calls("Copy"), host.Calls("StoreFlags")
		res := g.f.s.actOn(t.Context(), rule, 2, acct, g.row(t, "host lunch"), fresh)
		if res.class != actOK || res.note != rules.NoteReplayed || res.copyRef != "" {
			t.Errorf("actOn = %+v, want ok, replayed, no copy", res)
		}
		if host.Calls("Copy") != copies || host.Calls("StoreFlags") != flags+1 {
			t.Errorf("copies %d, flags %d, want the copy skipped and the flag run", host.Calls("Copy")-copies, host.Calls("StoreFlags")-flags)
		}
		if _, ok := g.appliedRow(t, rule.ID, "host", keyOf(false, g.refs["host lunch"])); ok {
			t.Error("a replay wrote a pending row of its own")
		}
	})
	t.Run("forward that may have gone", func(t *testing.T) {
		rule := g.newRule(t, "host", "replay-fwd", func(d *rules.Definition) {
			d.Actions = []rules.Action{{Kind: rules.ActForward, To: []string{"you@example.net"}}}
		})
		g.crashedForward(t, rule.ID, "host", keyOf(false, g.refs["host invoice"]))
		res := g.f.s.actOn(t.Context(), rule, 2, acct, g.row(t, "host invoice"), fresh)
		if res.class != actOK || res.note != rules.NoteOutcomeUnknown {
			t.Errorf("actOn = %+v, want ok, outcome_unknown", res)
		}
	})
	t.Run("forward older than its key", func(t *testing.T) {
		rule := g.newRule(t, "host", "replay-old", func(d *rules.Definition) {
			d.Actions = []rules.Action{{Kind: rules.ActForward, To: []string{"you@example.net"}}}
		})
		before := len(g.audit(t))
		res := g.f.s.actOn(t.Context(), rule, 2, acct, g.row(t, "host invoice"), old)
		if res.class != actOK || res.note != rules.NoteOutcomeUnknown {
			t.Errorf("actOn = %+v, want ok, outcome_unknown", res)
		}
		if n := len(g.audit(t)) - before; n != 0 {
			t.Errorf("%d audit rows, want none: the forward must not be tried", n)
		}
	})
}

// TestActAuditsRuleFolder checks that a rule's move and copy rows carry
// the ref, the destination folder as socket callers' rows do, then the
// rule and the run (James, 2026-11-08: validated folder names may appear
// in audit targets).
func TestActAuditsRuleFolder(t *testing.T) {
	t.Parallel()
	for _, kind := range []rules.ActionKind{rules.ActMove, rules.ActCopy} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			g := newActRig(t, nil)
			rule := g.newRule(t, "host", "file-lunch", func(d *rules.Definition) {
				d.Actions = []rules.Action{{Kind: kind, Folder: "Archive"}}
			})
			lunch := g.refs["host lunch"]
			if res := g.f.s.act(t.Context(), rule, 3, g.acct(t, "host"), g.row(t, "host lunch")); res.class != actOK {
				t.Fatalf("act = %+v, want ok", res)
			}
			rows := g.audit(t)
			if len(rows) != 1 {
				t.Fatalf("audit rows = %+v, want one", rows)
			}
			if want := []string{lunch.String(), "folder=Archive", "rule=" + rule.ID, "run=3"}; !slices.Equal(rows[0].targets, want) {
				t.Errorf("targets = %q, want %q", rows[0].targets, want)
			}
		})
	}
}
