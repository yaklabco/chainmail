package calcache

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// rawObject is a stored object of provider p with raw content raw.
func rawObject(p, raw string, at time.Time) calendar.Event {
	return calendar.Event{
		ProviderID: p, ETag: raw, UID: p + "@example.com", Summary: "s",
		Start: at, End: at.Add(time.Hour), Status: calendar.StatusConfirmed, Raw: []byte(raw),
	}
}

func openPair(t *testing.T) (*Store, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetCalendars(t.Context(), "acme", []calendar.Calendar{{ID: "work", Access: calendar.AccessWrite}}, "work"); err != nil {
		t.Fatal(err)
	}
	other, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	return s, other
}

// A read sees its rows and their raw objects as of one moment: an Apply
// that replaces the object between the two reads, and deletes the raw the
// rows named, does not leave the rows without their raw.
func TestEventReadsAreOneSnapshot(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	s, other := openPair(t)
	if err := s.Apply(t.Context(), "acme", "work", calendar.Changes{Upserts: []calendar.Event{rawObject("w", "raw one", at)}}, nil, at); err != nil {
		t.Fatal(err)
	}
	ran := false
	s.rowsRead = func() {
		if ran {
			return
		}
		ran = true
		ch := calendar.Changes{Upserts: []calendar.Event{rawObject("w", "raw two", at)}, ObjectReplace: true}
		if err := other.Apply(t.Context(), "acme", "work", ch, nil, at); err != nil {
			t.Errorf("concurrent Apply: %v", err)
		}
	}
	rows, err := s.Series(t.Context(), "acme", "work", "w")
	if !ran {
		t.Fatal("the hook between the two reads did not run")
	}
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(rows) != 1 || !bytes.Equal(rows[0].Event.Raw, []byte("raw one")) || rows[0].Event.ETag != "raw one" {
		t.Errorf("rows = %+v, want the first version with its raw", rows)
	}
	s.rowsRead = nil
	rows, err = s.Series(t.Context(), "acme", "work", "w")
	if err != nil || len(rows) != 1 || !bytes.Equal(rows[0].Event.Raw, []byte("raw two")) {
		t.Errorf("after the Apply: rows = %+v, %v; want the second version", rows, err)
	}
}

// A row whose raw object is missing is an error, never a row without its
// raw.
func TestMissingRawIsAnError(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	s, _ := openPair(t)
	if err := s.Apply(t.Context(), "acme", "work", calendar.Changes{Upserts: []calendar.Event{rawObject("w", "raw one", at)}}, nil, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), "DELETE FROM cal_raws"); err != nil {
		t.Fatal(err)
	}
	ref := calendar.EventRef("acme", "work", "w")
	for name, read := range map[string]func() ([]EventRow, error){
		"Series": func() ([]EventRow, error) { return s.Series(t.Context(), "acme", "work", "w") },
		"Event": func() ([]EventRow, error) {
			r, err := s.Event(t.Context(), ref)
			return []EventRow{r}, err
		},
		"Range": func() ([]EventRow, error) {
			return s.Range(t.Context(), []string{"acme"}, nil, at.Add(-time.Hour), at.Add(time.Hour))
		},
	} {
		rows, err := read()
		if !errors.Is(err, errStoredRow) {
			t.Errorf("%s = %+v, %v; want errStoredRow", name, rows, err)
		}
	}
}

// Diff reads the stored rows and their raw objects as of one moment too.
func TestDiffReadsOneSnapshot(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	s, other := openPair(t)
	if err := s.Apply(t.Context(), "acme", "work", calendar.Changes{Upserts: []calendar.Event{rawObject("w", "raw one", at)}}, nil, at); err != nil {
		t.Fatal(err)
	}
	ran := false
	s.rowsRead = func() {
		if ran {
			return
		}
		ran = true
		ch := calendar.Changes{Upserts: []calendar.Event{rawObject("w", "raw two", at)}, ObjectReplace: true}
		if err := other.Apply(t.Context(), "acme", "work", ch, nil, at); err != nil {
			t.Errorf("concurrent Apply: %v", err)
		}
	}
	ch := calendar.Changes{Upserts: []calendar.Event{rawObject("w", "raw three", at)}, ObjectReplace: true}
	changes, err := s.Diff(t.Context(), "acme", "work", ch, nil)
	if !ran {
		t.Fatal("the hook between the two reads did not run")
	}
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Before == nil || !bytes.Equal(changes[0].Before.Event.Raw, []byte("raw one")) {
		t.Errorf("changes = %+v, want the first version before, with its raw", changes)
	}
}
