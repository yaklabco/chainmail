package calsync_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/calsync"
)

// editSync is a store whose Sync results a test edits.
type editSync struct {
	calstore.Store
	edit func(calID string, ch *calendar.Changes)
}

func (e editSync) Sync(ctx context.Context, calID, token string) (calendar.Changes, error) {
	ch, err := e.Store.Sync(ctx, calID, token)
	if err == nil {
		e.edit(calID, &ch)
	}
	return ch, err
}

// TestOversizedObjectsSkipped covers objects the backend skipped and an
// object with more rows than calendar.MaxObjectEvents: neither is stored,
// the sync still succeeds, the calendar records the skip with fixed text
// and the log counts it.
func TestOversizedObjectsSkipped(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	grow := false
	r := newRig(t, func(o *calsync.Options) {
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		o.Store = editSync{Store: o.Store, edit: func(calID string, ch *calendar.Changes) {
			if !grow || calID != "work" {
				return
			}
			start := t0.Add(24 * time.Hour)
			for i := range calendar.MaxObjectEvents + 1 {
				ev := single("huge@x", start.AddDate(0, 0, i))
				ev.ProviderID = "huge"
				ev.Raw = []byte("raw")
				if i > 0 {
					ev.RecurrenceID = ev.Start
				} else {
					ev.RRule = "FREQ=DAILY"
				}
				ch.Upserts = append(ch.Upserts, ev)
			}
			ch.Skipped = append(ch.Skipped, "backend-skipped")
		}}
	})
	r.store.Add("work", single("a@x", t0.Add(time.Hour)))
	r.sync(t)
	grow = true
	r.sync(t)

	if st := r.syncer.Status(); st.State != calsync.StateReady {
		t.Errorf("Status = %+v, want ready", st)
	}
	rows, err := r.cache.Series(t.Context(), account, "work", "huge")
	if err != nil || len(rows) != 0 {
		t.Errorf("stored %d rows of the oversized object, %v", len(rows), err)
	}
	row, err := r.cache.Calendar(t.Context(), account, "work")
	if err != nil {
		t.Fatal(err)
	}
	if row.LastError != "calendar objects too large to sync were skipped" {
		t.Errorf("work LastError = %q", row.LastError)
	}
	if home, _ := r.cache.Calendar(t.Context(), account, "home"); home.LastError != "" {
		t.Errorf("home LastError = %q", home.LastError)
	}
	if !strings.Contains(logs.String(), "skipped=2") {
		t.Errorf("log does not count the two skipped objects:\n%s", logs.String())
	}
	if len(r.events(t)) != 0 {
		t.Errorf("events = %+v, want none", r.events(t))
	}
}
