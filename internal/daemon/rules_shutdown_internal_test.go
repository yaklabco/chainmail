package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/rules"
)

// holdStore is a fake mail store whose one StoreFlags call waits until the
// test releases it, carrying on after its context ends as an SMTP reply in
// flight does. It reports when that context ends, and when Close comes
// while the call is still running.
type holdStore struct {
	*fake.Store
	entered     chan struct{}
	cancelled   chan struct{}
	release     chan struct{}
	closedEarly chan struct{}

	mu     sync.Mutex
	inCall bool
	early  bool
}

func newHoldStore(fk *fake.Store) *holdStore {
	return &holdStore{
		Store: fk, entered: make(chan struct{}), cancelled: make(chan struct{}),
		release: make(chan struct{}), closedEarly: make(chan struct{}),
	}
}

func (h *holdStore) StoreFlags(ctx context.Context, refs []ref.Ref, add, remove []string) []mailstore.RefResult {
	h.mu.Lock()
	h.inCall = true
	h.mu.Unlock()
	close(h.entered)
	select {
	case <-ctx.Done():
		close(h.cancelled)
		<-h.release
	case <-h.release:
	}
	h.mu.Lock()
	h.inCall = false
	h.mu.Unlock()
	return h.Store.StoreFlags(context.WithoutCancel(ctx), refs, add, remove)
}

func (h *holdStore) Close() error {
	h.mu.Lock()
	if h.inCall && !h.early {
		h.early = true
		close(h.closedEarly)
	}
	h.mu.Unlock()
	return h.Store.Close()
}

// TestDrainJoinsTheRuleSchedulerBeforeClosingStores checks that shutdown
// stops the rule scheduler, and waits for the scheduled run it started,
// before it closes the mail stores. The scheduled run's flag is still in
// its store call, which goes on after its context ends: drain must not
// close the store under it, and nothing may open a store after drain.
func TestDrainJoinsTheRuleSchedulerBeforeClosingStores(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	hold := newHoldStore(g.fk)
	var buildMu sync.Mutex
	builds := 0
	g.f.s.stores = &storeSet{build: func(a config.ResolvedAccount) mailstore.Store {
		buildMu.Lock()
		defer buildMu.Unlock()
		builds++
		if a.Name == "host" {
			return hold
		}
		return g.f.fakes[a.Name]
	}}
	sc, err := rules.ParseSchedule("every:1h")
	if err != nil {
		t.Fatal(err)
	}
	g.newRule(t, "host", "flag-lunch-hourly", func(d *rules.Definition) {
		d.OnNew, d.Query, d.Schedule = false, "subject:lunch", sc
	})
	g.f.s.ruleRunners = map[string]*ruleRunner{"host": g.r}
	g.clk.advance(2 * time.Hour)
	stop := g.f.s.startBackground(t.Context())
	t.Cleanup(stop)
	// Cleanups run last first: the hold is released, then drain is joined,
	// then the background loops stop, so a failed wait below never leaves
	// the scheduled run parked on the hold or drain running.
	var drainWG sync.WaitGroup
	t.Cleanup(drainWG.Wait)
	release := sync.OnceFunc(func() { close(hold.release) })
	t.Cleanup(release)

	wait := func(what string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not happen within 10s", what)
		}
	}
	wait("the scheduled run's flag", hold.entered)
	drained := make(chan struct{})
	drainWG.Go(func() {
		defer close(drained)
		g.f.s.drain()
	})
	select {
	case <-hold.closedEarly:
		t.Error("drain closed the mail store while a scheduled run was still in a store call")
	case <-hold.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("drain neither stopped the scheduler nor closed the stores within 10s")
	}
	release()
	wait("drain", drained)
	buildMu.Lock()
	defer buildMu.Unlock()
	if builds != 1 {
		t.Errorf("stores built %d times, want 1: a store was opened again after drain closed them", builds)
	}
}
