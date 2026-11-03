package cache

import (
	"testing"
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
