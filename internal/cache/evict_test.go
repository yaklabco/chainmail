package cache

import (
	"slices"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
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
