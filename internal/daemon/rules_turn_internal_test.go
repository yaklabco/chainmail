package daemon

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/rules"
	"github.com/yaklabco/chainmail/internal/watch"
)

// goroutineID is the calling goroutine's ID, as its stack trace names it.
func goroutineID() string {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	return strings.Fields(string(buf))[1]
}

// waitInTake waits until goroutine id is parked in the runner's take,
// waiting for the turn. It reads every goroutine's stack, so the package's
// other runners, which park there too, do not confuse it.
func waitInTake(t *testing.T, id string) {
	t.Helper()
	head := "goroutine " + id + " [select"
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		for g := range strings.SplitSeq(string(buf[:n]), "\n\n") {
			if strings.HasPrefix(g, head) && strings.Contains(g, ".(*ruleRunner).take(") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine %s never waited for the runner's turn", id)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRuleStepReadsOwedWritesOnItsTurn checks that a step reads the writes
// the runner owes only once it holds the turn. A manual run holds the turn
// while the step waits for it: it pays the owed drop of a pending row,
// copies afresh and leaves a new pending row when its next action fails
// for now. The step must not drop that newer row, or its replay would copy
// the message again.
func TestRuleStepReadsOwedWritesOnItsTurn(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	ctx := t.Context()
	rule := g.newRule(t, "host", "copy-then-flag", func(d *rules.Definition) {
		d.Query, d.Actions = "subject:lunch", []rules.Action{{Kind: rules.ActCopy, Folder: "Archive"}, {Kind: rules.ActFlag}}
	})
	lunch := g.refs["host lunch"]
	key := keyOf(false, lunch)
	// A copy refused for good left a pending row that state.db would not
	// drop, so the runner owes the drop.
	if err := g.f.s.rules.MarkPending(ctx, rule.ID, "host", key, 0); err != nil {
		t.Fatal(err)
	}
	g.r.owe(owedWrite{kind: owedClear, ruleID: rule.ID, key: key})
	if _, err := g.f.stateDB.ExecContext(ctx,
		`CREATE TRIGGER keep_applied BEFORE DELETE ON rule_applied BEGIN SELECT RAISE(ABORT, 'kept'); END`); err != nil {
		t.Fatal(err)
	}
	last := g.publish(t, lunch)

	var wg sync.WaitGroup
	var manual rules.Run
	var manualErr, stepErr error
	// The manual run takes the turn, meets the owed drop, and waits on the
	// clock between its tries.
	wg.Go(func() { manual, manualErr = g.r.runRule(ctx, rule, rules.TriggerManual, 0) })
	retry := g.clk.next(t)
	// The step starts and waits for the turn the manual run holds.
	stepID := make(chan string, 1)
	wg.Go(func() {
		stepID <- goroutineID()
		stepErr = g.step(ctx)
	})
	waitInTake(t, <-stepID)
	// The drop goes through now. The manual run copies afresh, and its flag
	// fails for now, which leaves its own pending row.
	if _, err := g.f.stateDB.ExecContext(ctx, `DROP TRIGGER keep_applied`); err != nil {
		t.Fatal(err)
	}
	g.fk.FailWrite("StoreFlags", mailstore.ErrUnavailable)
	retry.fire <- g.clk.Now()
	wg.Wait()

	if manualErr != nil || manual.Outcome != rules.OutcomeInterrupted || manual.Reason != api.ReasonServerUnavailable {
		t.Fatalf("manual run = %+v, %v, want interrupted with SERVER_UNAVAILABLE", manual, manualErr)
	}
	if stepErr != nil {
		t.Fatal(stepErr)
	}
	if n := g.fk.Calls("Copy"); n != 1 {
		t.Errorf("copied %d times, want 1: the step dropped the manual run's pending row", n)
	}
	if row, ok := g.appliedRow(t, rule.ID, "host", key); !ok || row.Outcome != rules.AppliedDone || row.Note != rules.NoteReplayed {
		t.Errorf("act-once row = %+v, %t, want applied, replayed", row, ok)
	}
	if !g.flagged([]ref.Ref{lunch})[0] {
		t.Error("lunch is not flagged after the step's replay")
	}
	if got := g.acked(t); got != last {
		t.Errorf("acked %d, want %d", got, last)
	}
}

// TestRuleStepAcksOnTheTurnThatDrained checks that a step drains what the
// runner owes and acknowledges its batch on one hold of the turn. A manual
// run that waits for the turn while the batch runs goes only once the
// batch is acknowledged, whatever the batch left owed. The write that
// manual run leaves owed then holds the next batch until it is made.
func TestRuleStepAcksOnTheTurnThatDrained(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	ctx := t.Context()
	// The batch's first rule leaves a drop owed: its copy is refused for
	// good, and state.db will not drop the pending row yet.
	g.newRule(t, "host", "copy-nowhere", func(d *rules.Definition) {
		d.Priority, d.Query, d.Actions = 10, "subject:lunch", []rules.Action{{Kind: rules.ActCopy, Folder: "Nowhere"}}
	})
	// The second waits on the clock for a body, holding the turn.
	notes := g.uncached("host", fake.Message{
		Folder: "INBOX", Date: routeNow, Subject: "weekly notes", MessageID: "notes@x", Body: "the plan for the week",
	})
	g.cacheHeader(t, notes)
	g.newRule(t, "host", "flag-plans", func(d *rules.Definition) { d.Priority, d.Query = 20, "plan" })
	// The manual rule's copy guard is refused, so it leaves a write owed.
	manualRule := g.newRule(t, "host", "copy-invoices", func(d *rules.Definition) {
		d.OnNew, d.Query, d.Actions = false, "subject:invoice", []rules.Action{{Kind: rules.ActCopy, Folder: "Archive"}}
	})
	for _, stmt := range []string{
		`CREATE TRIGGER keep_applied BEFORE DELETE ON rule_applied BEGIN SELECT RAISE(ABORT, 'kept'); END`,
		`CREATE TRIGGER refuse_applied BEFORE INSERT ON rule_applied WHEN NEW.outcome = 'applied'
		 BEGIN SELECT RAISE(ABORT, 'refused'); END`,
		// manual_began records where the group was acknowledged when a
		// manual run began.
		`CREATE TABLE manual_began (acked INTEGER NOT NULL)`,
		fmt.Sprintf(`CREATE TRIGGER see_manual AFTER INSERT ON rule_runs WHEN NEW.trigger_kind = 'manual'
		 BEGIN INSERT INTO manual_began SELECT acked_id FROM event_groups WHERE name = '%s'; END`, g.r.group),
	} {
		if _, err := g.f.stateDB.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	last := g.publish(t, g.refs["host lunch"], notes)

	var wg sync.WaitGroup
	var manual rules.Run
	var manualErr, stepErr error
	wg.Go(func() { stepErr = g.step(ctx) })
	body := g.clk.next(t)
	if body.d != bodyPoll {
		t.Fatalf("first wait %v, want the body poll %v", body.d, bodyPoll)
	}
	manualID := make(chan string, 1)
	wg.Go(func() {
		manualID <- goroutineID()
		manual, manualErr = g.r.runRule(ctx, manualRule, rules.TriggerManual, 0)
	})
	waitInTake(t, <-manualID)
	// The owed drop can go through now, and the body never comes.
	if _, err := g.f.stateDB.ExecContext(ctx, `DROP TRIGGER keep_applied`); err != nil {
		t.Fatal(err)
	}
	g.clk.advance(bodyWait)
	body.fire <- g.clk.Now()
	wg.Wait()

	if stepErr != nil {
		t.Fatal(stepErr)
	}
	if g.pos != last || g.acked(t) != last {
		t.Fatalf("position %d, acked %d, want both %d", g.pos, g.acked(t), last)
	}
	if manualErr != nil || manual.Outcome != rules.OutcomeInterrupted {
		t.Fatalf("manual run = %+v, %v, want interrupted", manual, manualErr)
	}
	var began []int64
	rows, err := g.f.stateDB.QueryContext(ctx, `SELECT acked FROM manual_began`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var acked int64
		if err := rows.Scan(&acked); err != nil {
			t.Fatal(err)
		}
		began = append(began, acked)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(began) != 1 || began[0] != last {
		t.Errorf("the manual run began with the group acked at %v, want after the step acknowledged %d", began, last)
	}

	// The copy guard the manual run owes holds the next batch.
	start := g.pos
	g.publish(t, g.refs["host lunch"])
	var heldErr error
	wg.Go(func() { heldErr = g.step(ctx) })
	for i := range owedWriteTries - 1 {
		w := g.clk.next(t)
		if want := watch.Backoff(i, 0.5); w.d != want {
			t.Errorf("retry %d waited %v, want %v", i+1, w.d, want)
		}
		w.fire <- g.clk.Now()
	}
	wg.Wait()
	if reason, _ := failureOf(heldErr); reason != api.ReasonInternal {
		t.Fatalf("held step = %v, want an INTERNAL failure", heldErr)
	}
	if g.pos != start || g.acked(t) != start {
		t.Errorf("held step: position %d, acked %d, want both %d", g.pos, g.acked(t), start)
	}
	if _, err := g.f.stateDB.ExecContext(ctx, `DROP TRIGGER refuse_applied`); err != nil {
		t.Fatal(err)
	}
	if err := g.step(ctx); err != nil {
		t.Fatal(err)
	}
	if got := g.acked(t); got <= start || got != g.pos {
		t.Errorf("acked %d at position %d, want both past %d", got, g.pos, start)
	}
	copied := g.foundIn(t, "host", "Archive", "inv42@x")
	if row, ok := g.appliedRow(t, manualRule.ID, "host", keyOf(false, copied)); !ok || row.Outcome != rules.AppliedDone {
		t.Errorf("the manual copy's row = %+v, %t, want applied", row, ok)
	}
}
