package cache

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
)

// noteWrite wakes the evictor only when the file is over the cap, and two
// wakes before the evictor runs coalesce into one.
func TestNoteWriteKicksOnlyOverTheCap(t *testing.T) {
	t.Parallel()
	st := openEvictStore(t, nil)
	ctx := t.Context()
	size, err := st.SizeBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kick := st.evict.kickChan()
	if err := st.noteWrite(ctx, size); err != nil {
		t.Fatal(err)
	}
	if len(kick) != 0 {
		t.Fatal("a file at the cap kicked the evictor")
	}
	if err := st.noteWrite(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if len(kick) != 0 {
		t.Fatal("a zero cap, which means none, kicked the evictor")
	}
	for range 2 {
		if err := st.noteWrite(ctx, size-1); err != nil {
			t.Fatal(err)
		}
	}
	if len(kick) != 1 {
		t.Fatalf("kicks queued = %d, want 1", len(kick))
	}
	if st.OverCap() {
		t.Error("OverCap is set before any sweep")
	}
}

// SetWindowDone changes window_done and leaves the rest of the progress
// row alone, and creates no row for a folder without one.
func TestSetWindowDone(t *testing.T) {
	t.Parallel()
	st := openEvictStore(t, nil)
	ctx := t.Context()
	p := Progress{
		Account: "acme", Folder: "INBOX", UIDValidity: 7, BaselineUIDNext: 500,
		BackfillLowUID: 120, BackfillDone: true, WindowDone: true, LastAppliedAt: t10Start,
	}
	if err := st.PutProgress(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, done := range []bool{false, true} {
		if err := st.SetWindowDone(ctx, "acme", "INBOX", done); err != nil {
			t.Fatal(err)
		}
		got, ok, err := st.Progress(ctx, "acme", "INBOX")
		if err != nil || !ok {
			t.Fatalf("Progress = %v, %v", ok, err)
		}
		want := p
		want.WindowDone = done
		if !got.LastAppliedAt.Equal(want.LastAppliedAt) {
			t.Errorf("LastAppliedAt = %v, want %v", got.LastAppliedAt, want.LastAppliedAt)
		}
		// Equal instants may differ in location; compare the rest as values.
		got.LastAppliedAt = want.LastAppliedAt
		if got != want {
			t.Errorf("after SetWindowDone(%v): progress = %+v, want %+v", done, got, want)
		}
	}
	if err := st.SetWindowDone(ctx, "acme", "Archive", false); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Progress(ctx, "acme", "Archive"); err != nil || ok {
		t.Errorf("Progress(Archive) = %v, %v, want no row", ok, err)
	}
}

// The retention step removes bodies outside each account's window that
// were never read or were read longer ago than the retention, keeps the
// window and recent reads (both bounds inclusive), and rewrites the FTS
// rows so header terms still match and body terms no longer do.
func TestEvictRetention(t *testing.T) {
	t.Parallel()
	clk := newPrefetchClock(t10Start)
	st := openEvictStore(t, clk.Now)
	ctx := t.Context()
	now := t10Start
	seeds := []evictSeed{
		{account: "acme", uid: 1, arrived: now.Add(-oneDay), raw: evictTextRaw("bodyone")},
		{account: "acme", uid: 2, arrived: now.Add(-60 * oneDay), raw: evictTextRaw("bodytwo")},
		{account: "acme", uid: 3, arrived: now.Add(-60 * oneDay), readAt: now.Add(-3 * oneDay), raw: evictTextRaw("bodythree")},
		{account: "acme", uid: 4, arrived: now.Add(-60 * oneDay), readAt: now.Add(-10 * oneDay), raw: evictTextRaw("bodyfour")},
		{account: "acme", uid: 5, arrived: now.Add(-30 * oneDay), raw: evictTextRaw("bodyfive")},
		{account: "acme", uid: 6, arrived: now.Add(-60 * oneDay), readAt: now.Add(-7 * oneDay), raw: evictTextRaw("bodysix")},
		{account: "beta", uid: 1, arrived: now.Add(-10 * oneDay), raw: evictTextRaw("bodybeta")},
	}
	refs := seedEvict(t, st, clk, seeds...)
	// gone: never read outside the window (uid 2), read too long ago
	// (uid 4), and beta's, whose window is a week.
	gone := map[int]bool{1: true, 3: true, 6: true}
	windows := map[string]time.Duration{"acme": 30 * oneDay, "beta": 7 * oneDay}
	policy := EvictPolicy{
		Now:       now,
		Window:    func(a string) time.Duration { return windows[a] },
		Retention: constDur(7 * oneDay),
		MaxSize:   1 << 40,
	}
	res, err := st.Evict(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	var freed int64
	for i, r := range refs {
		if gone[i] {
			freed += int64(len(seeds[i].raw))
		}
		if got := hasBody(t, st, r); got == gone[i] {
			t.Errorf("seed %d (%s uid %d): body cached = %v, want %v", i, seeds[i].account, seeds[i].uid, got, !gone[i])
		}
	}
	want := EvictResult{Bodies: 3, FreedBytes: freed}
	if res != want {
		t.Errorf("Evict = %+v, want %+v", res, want)
	}

	src := mailstore.Source{Folder: "INBOX", UIDValidity: 7}
	match := func(term string) []uint32 {
		t.Helper()
		uids, _, err := st.SearchUIDs(ctx, "acme", src, Where{FTS: term}, 0, false)
		if err != nil {
			t.Fatalf("SearchUIDs(%s): %v", term, err)
		}
		return uids
	}
	if got := match("bodytwo"); len(got) != 0 {
		t.Errorf("body term of an evicted body matches %v", got)
	}
	if got := match("bodyone"); !slices.Equal(got, []uint32{1}) {
		t.Errorf("body term of a kept body = %v, want [1]", got)
	}
	for _, term := range []string{"keepword", "ann"} {
		if got := match(term); !slices.Equal(got, []uint32{6, 5, 4, 3, 2, 1}) {
			t.Errorf("header term %q = %v, want every acme row", term, got)
		}
	}
	again, err := st.Evict(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if again != (EvictResult{}) {
		t.Errorf("second sweep = %+v, want nothing", again)
	}
}

// seedCapOrder stores eight bodies of about 64 KiB for acme INBOX, returned
// in the order the size cap removes them: outside the 30 day window, read 6
// days ago (uid 1), read 5 days ago (uid 2, arrived before uid 1), read
// yesterday (uid 3, arrived first of all); then window bodies oldest
// arrival first (uids 4 to 8). The folder's progress has window_done set.
// Every body outside the window has been read, since step 1 always takes a
// never read one; TestCapVictimsNeverReadFirst covers that order.
func seedCapOrder(t *testing.T, st *Store, clk *prefetchClock) ([]ref.Ref, [][]byte) {
	t.Helper()
	now := t10Start
	seeds := []evictSeed{
		{account: "acme", uid: 1, arrived: now.Add(-200 * oneDay), readAt: now.Add(-6 * oneDay)},
		{account: "acme", uid: 2, arrived: now.Add(-300 * oneDay), readAt: now.Add(-5 * oneDay)},
		{account: "acme", uid: 3, arrived: now.Add(-400 * oneDay), readAt: now.Add(-oneDay)},
		{account: "acme", uid: 4, arrived: now.Add(-25 * oneDay)},
		{account: "acme", uid: 5, arrived: now.Add(-20 * oneDay)},
		{account: "acme", uid: 6, arrived: now.Add(-15 * oneDay)},
		{account: "acme", uid: 7, arrived: now.Add(-10 * oneDay)},
		{account: "acme", uid: 8, arrived: now.Add(-5 * oneDay)},
	}
	raws := make([][]byte, len(seeds))
	for i := range seeds {
		seeds[i].raw = evictPaddedRaw(fmt.Sprintf("padword%d", i+1))
		raws[i] = seeds[i].raw
	}
	refs := seedEvict(t, st, clk, seeds...)
	if err := st.PutProgress(t.Context(), Progress{
		Account: "acme", Folder: "INBOX", UIDValidity: 7, BaselineUIDNext: 9, BackfillLowUID: 1, BackfillDone: true, WindowDone: true,
	}); err != nil {
		t.Fatal(err)
	}
	return refs, raws
}

// capPolicy keeps every body under step 1 (30 day window, ten year
// retention), so only the cap removes anything.
func capPolicy(maxSize int64) EvictPolicy {
	return EvictPolicy{Now: t10Start, Window: constDur(30 * oneDay), Retention: constDur(3650 * oneDay), MaxSize: maxSize}
}

// Over the cap, bodies go in spec order until the file is at or under 90%
// of max_size: outside the window oldest read first with never read first,
// then window bodies oldest arrival first. Losing a window body clears the
// folder's window_done and sets the prefetch hold.
func TestEvictCapOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// free is how many bodies' worth of bytes the cap must free.
		free             float64
		minGone, maxGone int
		windowCleared    bool
	}{
		{name: "outside the window first, oldest read first", free: 1.5, minGone: 2, maxGone: 3},
		{name: "then window bodies, oldest arrival first", free: 4.5, minGone: 5, maxGone: 6, windowCleared: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clk := newPrefetchClock(t10Start)
			st := openEvictStore(t, clk.Now)
			ctx := t.Context()
			order, raws := seedCapOrder(t, st, clk)
			size, err := st.SizeBytes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			body := float64(len(raws[0]))
			maxSize := int64((float64(size) - tt.free*body) / 0.9)
			res, err := st.Evict(ctx, capPolicy(maxSize))
			if err != nil {
				t.Fatal(err)
			}
			gone := 0
			var freed int64
			for i, r := range order {
				if hasBody(t, st, r) {
					continue
				}
				if gone != i {
					t.Fatalf("uid %d was evicted while an earlier body in cap order was kept", r.UID)
				}
				gone++
				freed += int64(len(raws[i]))
			}
			if gone < tt.minGone || gone > tt.maxGone {
				t.Errorf("evicted %d bodies, want %d to %d", gone, tt.minGone, tt.maxGone)
			}
			if res.Bodies != uint64(gone) || res.FreedBytes != freed || res.OverCap {
				t.Errorf("Evict = %+v, want %d bodies, %d bytes, not over the cap", res, gone, freed)
			}
			after, err := st.SizeBytes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if limit := maxSize * evictTargetPercent / 100; after > limit {
				t.Errorf("size after = %d, want at most %d (90%% of %d)", after, limit, maxSize)
			}
			p, _, err := st.Progress(ctx, "acme", "INBOX")
			if err != nil {
				t.Fatal(err)
			}
			if p.WindowDone == tt.windowCleared {
				t.Errorf("window_done = %v, want %v", p.WindowDone, !tt.windowCleared)
			}
			if got := st.evict.hold.Load(); got != tt.windowCleared {
				t.Errorf("prefetch hold = %v, want %v", got, tt.windowCleared)
			}
			if st.OverCap() {
				t.Error("OverCap set though bodies were left")
			}
		})
	}
}

// When headers alone exceed the cap every body goes and the sweep reports
// OverCap, and a later sweep under a larger cap clears it.
func TestEvictOverCap(t *testing.T) {
	t.Parallel()
	clk := newPrefetchClock(t10Start)
	st := openEvictStore(t, clk.Now)
	ctx := t.Context()
	order, _ := seedCapOrder(t, st, clk)
	res, err := st.Evict(ctx, capPolicy(8192))
	if err != nil {
		t.Fatal(err)
	}
	if res.Bodies != uint64(len(order)) || !res.OverCap || !st.OverCap() {
		t.Errorf("Evict = %+v, OverCap() = %v, want every body gone and over the cap", res, st.OverCap())
	}
	src := mailstore.Source{Folder: "INBOX", UIDValidity: 7}
	if uids, _, err := st.SearchUIDs(ctx, "acme", src, Where{FTS: "keepword"}, 0, false); err != nil || len(uids) != len(order) {
		t.Errorf("header term after evicting every body = %v, %v, want every row", uids, err)
	}
	res, err = st.Evict(ctx, capPolicy(1<<40))
	if err != nil {
		t.Fatal(err)
	}
	if res.OverCap || st.OverCap() {
		t.Errorf("Evict under a large cap = %+v, OverCap() = %v, want not over the cap", res, st.OverCap())
	}
	if slices.ContainsFunc(order, func(r ref.Ref) bool { return hasBody(t, st, r) }) {
		t.Error("bodies came back")
	}
}

// Under the cap a never read body outside the window goes before any read
// one. Step 1 always takes such a body first, so this calls capVictims
// directly.
func TestCapVictimsNeverReadFirst(t *testing.T) {
	t.Parallel()
	clk := newPrefetchClock(t10Start)
	st := openEvictStore(t, clk.Now)
	now := t10Start
	refs := seedEvict(t, st, clk,
		evictSeed{account: "acme", uid: 1, arrived: now.Add(-300 * oneDay), readAt: now.Add(-9 * oneDay), raw: evictTextRaw("readold")},
		evictSeed{account: "acme", uid: 2, arrived: now.Add(-100 * oneDay), raw: evictTextRaw("neverread")},
		evictSeed{account: "acme", uid: 3, arrived: now.Add(-oneDay), raw: evictTextRaw("inwindow")},
	)
	starts := map[string]int64{"acme": now.Add(-30 * oneDay).Unix()}
	victims, err := st.capVictims(t.Context(), starts, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[int64]uint32, len(refs))
	for _, r := range refs {
		var id int64
		if err := st.db.QueryRowContext(t.Context(), "SELECT id FROM messages WHERE account = ? AND uid = ?", r.Account, r.UID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[id] = r.UID
	}
	var got []uint32
	var inWindow []bool
	for _, v := range victims {
		got = append(got, ids[v.msg])
		inWindow = append(inWindow, v.inWindow)
	}
	if !slices.Equal(got, []uint32{2, 1, 3}) || !slices.Equal(inWindow, []bool{false, false, true}) {
		t.Errorf("cap order = %v (in window %v), want [2 1 3] ([false false true])", got, inWindow)
	}
}

// newOldBodyEvictor seeds one body that arrived 400 days ago and was never
// read, which any sweep under a 90 day window removes, and returns an
// Evictor over the store with clk.
func newOldBodyEvictor(t *testing.T, clk *prefetchClock, onError func(error)) (*Evictor, *Store, ref.Ref) {
	t.Helper()
	st := openEvictStore(t, clk.Now)
	refs := seedEvict(t, st, clk, evictSeed{account: "acme", uid: 1, arrived: t10Start.Add(-400 * oneDay), raw: evictTextRaw("oldword")})
	e := NewEvictor(EvictorOptions{
		Store: st, Window: constDur(90 * oneDay), Retention: constDur(30 * oneDay),
		MaxSize: func() int64 { return 1 << 40 }, Clock: clk, OnError: onError,
	})
	return e, st, refs[0]
}

// runEvictor runs e until the test ends.
func runEvictor(t *testing.T, e *Evictor) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { e.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// A write that leaves the file over the cap wakes the evictor at once,
// without waiting for the hour.
func TestEvictorSweepsWhenKicked(t *testing.T) {
	t.Parallel()
	clk := newPrefetchClock(t10Start)
	e, st, old := newOldBodyEvictor(t, clk, nil)
	runEvictor(t, e)
	if err := st.noteWrite(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, "the kicked sweep", func() bool { return !hasBody(t, st, old) })
}

// Without a kick the evictor sweeps once an hour has passed.
func TestEvictorSweepsHourly(t *testing.T) {
	t.Parallel()
	clk := newPrefetchClock(t10Start)
	e, st, old := newOldBodyEvictor(t, clk, nil)
	runEvictor(t, e)
	pollUntil(t, "the evictor to wait on its hourly timer", func() bool { return clk.waiting() > 0 })
	if !hasBody(t, st, old) {
		t.Fatal("the evictor swept before the hour")
	}
	clk.Advance(time.Hour)
	pollUntil(t, "the hourly sweep", func() bool { return !hasBody(t, st, old) })
}

// Sweep returns the result and passes a failure to OnError.
func TestEvictorSweep(t *testing.T) {
	t.Parallel()
	clk := newPrefetchClock(t10Start)
	var reported []error
	e, st, _ := newOldBodyEvictor(t, clk, func(err error) { reported = append(reported, err) })
	res, err := e.Sweep(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Bodies != 1 || res.FreedBytes != int64(len(evictTextRaw("oldword"))) {
		t.Errorf("Sweep = %+v, want 1 body of the seeded size", res)
	}
	if len(reported) != 0 {
		t.Errorf("OnError called for a good sweep: %v", reported)
	}
	if _, err := st.db.ExecContext(t.Context(), "DROP TABLE bodies"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sweep(t.Context()); err == nil {
		t.Fatal("Sweep without a bodies table succeeded")
	}
	if len(reported) != 1 {
		t.Errorf("OnError calls = %d, want 1", len(reported))
	}
}

// An eviction victim whose row was deleted after the victim query, and
// whose integer id a newer message then reused, is left alone: its body and
// its folder's window_done stay, for retention and for the cap alike. The
// victim's identity is checked inside the deleting transaction.
func TestEvictDoesNotFollowAReusedRowID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		victims func(ctx context.Context, st *Store) ([]evictVictim, error)
	}{
		{name: "retention", victims: func(ctx context.Context, st *Store) ([]evictVictim, error) {
			return st.retentionVictims(ctx, "acme", t10Start.Add(-30*oneDay).Unix(), t10Start.Add(-7*oneDay).Unix())
		}},
		{name: "cap", victims: func(ctx context.Context, st *Store) ([]evictVictim, error) {
			return st.capVictims(ctx, map[string]int64{"acme": t10Start.Add(-30 * oneDay).Unix()}, 1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clk := newPrefetchClock(t10Start)
			st := openEvictStore(t, clk.Now)
			ctx := t.Context()
			old := seedEvict(t, st, clk, evictSeed{account: "acme", uid: 1, arrived: t10Start.Add(-400 * oneDay), raw: evictTextRaw("oldword")})[0]
			victims, err := tt.victims(ctx, st)
			if err != nil {
				t.Fatal(err)
			}
			if len(victims) != 1 {
				t.Fatalf("victims = %+v, want the old INBOX body", victims)
			}
			if err := st.DeleteRow(ctx, old); err != nil {
				t.Fatal(err)
			}
			if err := st.PutProgress(ctx, Progress{
				Account: "acme", Folder: "Archive", UIDValidity: 7, BaselineUIDNext: 10,
				BackfillLowUID: 1, BackfillDone: true, WindowDone: true, LastAppliedAt: t10Start,
			}); err != nil {
				t.Fatal(err)
			}
			fresh := seedEvict(t, st, clk, evictSeed{account: "acme", folder: "Archive", uid: 9, arrived: t10Start.Add(-oneDay), raw: evictTextRaw("newword")})[0]
			id, _, ok, err := loadRow(ctx, st.db, fresh)
			if err != nil || !ok {
				t.Fatalf("loading the new row: %v, %t", err, ok)
			}
			if id != victims[0].msg {
				t.Fatalf("the new row's id = %d, want the reused %d", id, victims[0].msg)
			}
			n, freed, err := st.dropBodies(ctx, victims)
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 || freed != 0 {
				t.Errorf("dropBodies = %d bodies, %d bytes, want none", n, freed)
			}
			if !hasBody(t, st, fresh) {
				t.Error("the Archive body under the reused id was evicted")
			}
			p, _, err := st.Progress(ctx, "acme", "Archive")
			if err != nil {
				t.Fatal(err)
			}
			if !p.WindowDone {
				t.Error("Archive's window_done was cleared")
			}
			if st.evict.hold.Load() {
				t.Error("the prefetch hold was set")
			}
		})
	}
}

// A window body evicted under the cap clears the window_done of the folder
// its row is in when it is deleted, which the deleting transaction reads
// from the row itself.
func TestEvictClearsTheWindowOfTheRowAsDeleted(t *testing.T) {
	t.Parallel()
	clk := newPrefetchClock(t10Start)
	st := openEvictStore(t, clk.Now)
	ctx := t.Context()
	if err := st.PutProgress(ctx, Progress{
		Account: "acme", Folder: "Archive", UIDValidity: 7, BaselineUIDNext: 10,
		BackfillLowUID: 1, BackfillDone: true, WindowDone: true, LastAppliedAt: t10Start,
	}); err != nil {
		t.Fatal(err)
	}
	r := seedEvict(t, st, clk, evictSeed{account: "acme", folder: "Archive", uid: 9, arrived: t10Start.Add(-oneDay), raw: evictTextRaw("newword")})[0]
	victims, err := st.capVictims(ctx, map[string]int64{"acme": t10Start.Add(-30 * oneDay).Unix()}, 1)
	if err != nil {
		t.Fatal(err)
	}
	n, _, err := st.dropBodies(ctx, victims)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || hasBody(t, st, r) {
		t.Fatalf("dropBodies = %d, body cached %t, want the window body gone", n, hasBody(t, st, r))
	}
	p, _, err := st.Progress(ctx, "acme", "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if p.WindowDone {
		t.Error("Archive's window_done stayed set after losing a window body")
	}
}
