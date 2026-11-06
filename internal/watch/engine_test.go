package watch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/db"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
)

// stubWatcher records the one SyncFolder call storeSource makes.
type stubWatcher struct {
	folder string
	req    mailstore.SyncRequest
	delta  mailstore.Delta
	err    error
}

func (w *stubWatcher) OpenWatch(context.Context, string) (mailstore.WatchConn, error) {
	return nil, errors.New("stubWatcher: OpenWatch is not used")
}

func (w *stubWatcher) FolderState(context.Context, string) (mailstore.FolderState, error) {
	return mailstore.FolderState{}, errors.New("stubWatcher: FolderState is not used")
}

func (w *stubWatcher) SyncFolder(_ context.Context, folder string, req mailstore.SyncRequest) (mailstore.Delta, error) {
	w.folder, w.req = folder, req
	return w.delta, w.err
}

var _ Source = storeSource{}

// storeSource syncs its folder through the watcher's pooled SyncFolder and
// returns the delta and error unchanged.
func TestStoreSourceForwardsToSyncFolder(t *testing.T) {
	t.Parallel()
	errDown := errors.New("pool exhausted")
	tests := []struct {
		name  string
		delta mailstore.Delta
		err   error
	}{
		{name: "delta", delta: mailstore.Delta{State: diffState(9, 3, true), New: []mailstore.Summary{diffSummary(8, 0)}}},
		{name: "error", err: errDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := &stubWatcher{delta: tt.delta, err: tt.err}
			req := mailstore.SyncRequest{From: mailstore.Checkpoint{UIDValidity: 7, UIDNext: 8, HighestModSeq: 2}, Sweep: true, Batch: 50, Snippets: true}
			got, err := storeSource{W: w, Folder: "Sent"}.Sync(t.Context(), req)
			if !errors.Is(err, tt.err) {
				t.Errorf("err = %v, want %v", err, tt.err)
			}
			if !reflect.DeepEqual(got, tt.delta) {
				t.Errorf("delta = %+v, want %+v", got, tt.delta)
			}
			if w.folder != "Sent" || !reflect.DeepEqual(w.req, req) {
				t.Errorf("SyncFolder(%q, %+v), want (\"Sent\", %+v)", w.folder, w.req, req)
			}
		})
	}
}

var engineAt = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

// engineHarness is an engine over a real state.db in a temp dir.
type engineHarness struct {
	db     *sql.DB
	log    *Log
	state  *State
	engine *Engine
}

func newEngineHarness(t *testing.T, opts EngineOptions) engineHarness {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	now := func() time.Time { return engineAt }
	if opts.Now == nil {
		opts.Now = now
	}
	lg := NewLog(conn, now, nil)
	st := NewState(conn)
	return engineHarness{db: conn, log: lg, state: st, engine: NewEngine(conn, lg, st, opts)}
}

// pass runs a Pass on host's INBOX that must succeed.
func (h engineHarness) pass(t *testing.T, src Source, opts PassOptions) PassResult {
	t.Helper()
	res, err := h.engine.Pass(t.Context(), "host", "INBOX", src, opts)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return res
}

// events reads every event in the log, oldest first, the internal
// unreadable events included.
func (h engineHarness) events(t *testing.T) []Event {
	t.Helper()
	page, err := h.log.Read(t.Context(), Query{Filter: Filter{Internal: true}, Limit: 1000})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return page.Events
}

func (h engineHarness) tail(t *testing.T) int64 {
	t.Helper()
	tail, err := h.log.Tail(t.Context())
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	return tail
}

func (h engineHarness) row(t *testing.T) FolderRow {
	t.Helper()
	row, ok, err := h.state.Folder(t.Context(), "host", "INBOX")
	if err != nil || !ok {
		t.Fatalf("Folder = %v, %v; want the INBOX row", ok, err)
	}
	return row
}

// uids lists host's INBOX known UIDs, ascending.
func (h engineHarness) uids(t *testing.T) []uint32 {
	t.Helper()
	known, err := h.state.uids(t.Context(), "host", "INBOX")
	if err != nil {
		t.Fatalf("UIDs: %v", err)
	}
	return slices.Sorted(maps.Keys(known))
}

type scriptResult struct {
	delta mailstore.Delta
	err   error
}

// scriptSource returns queued results in order and records every request.
type scriptSource struct {
	results []scriptResult
	reqs    []mailstore.SyncRequest
}

func (s *scriptSource) Sync(_ context.Context, req mailstore.SyncRequest) (mailstore.Delta, error) {
	s.reqs = append(s.reqs, req)
	if len(s.results) == 0 {
		return mailstore.Delta{}, errors.New("scriptSource: no result queued")
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r.delta, r.err
}

// probeSource runs probe, then syncs through inner.
type probeSource struct {
	inner Source
	probe func(ctx context.Context) error
}

func (p probeSource) Sync(ctx context.Context, req mailstore.SyncRequest) (mailstore.Delta, error) {
	if err := p.probe(ctx); err != nil {
		return mailstore.Delta{}, err
	}
	return p.inner.Sync(ctx, req)
}

// baselineDelta is a CONDSTORE INBOX at UIDVALIDITY 7 and HIGHESTMODSEQ 10
// holding uids.
func baselineDelta(uids ...uint32) mailstore.Delta {
	return mailstore.Delta{State: diffState(uids[len(uids)-1]+1, 10, true), Rebased: true, UIDs: uids}
}

// A first pass baselines silently; the next one commits its flags and new
// events, UID changes and checkpoint, reading From from the stored row.
func TestPassCommitsBaselineThenChanges(t *testing.T) {
	t.Parallel()
	h := newEngineHarness(t, EngineOptions{})
	src := &scriptSource{results: []scriptResult{
		{delta: baselineDelta(1, 2)},
		{delta: mailstore.Delta{
			State:   diffState(4, 12, true),
			New:     []mailstore.Summary{diffSummary(3, 0)},
			Changed: []mailstore.FlagState{{UID: 1, Flags: []string{`\Seen`}}},
		}},
	}}
	if res, want := h.pass(t, src, PassOptions{Mode: "idle"}), (PassResult{Events: 1, Baselined: true, CondStore: true, Batches: 1}); res != want {
		t.Errorf("first Pass = %+v, want %+v", res, want)
	}
	if res, want := h.pass(t, src, PassOptions{Mode: "idle"}), (PassResult{Events: 2, CondStore: true, Batches: 1}); res != want {
		t.Errorf("second Pass = %+v, want %+v", res, want)
	}
	wantReqs := []mailstore.SyncRequest{
		{Batch: mailstore.DefaultSyncBatch},
		{From: mailstore.Checkpoint{UIDValidity: 7, UIDNext: 3, HighestModSeq: 10}, Batch: mailstore.DefaultSyncBatch},
	}
	if !reflect.DeepEqual(src.reqs, wantReqs) {
		t.Errorf("requests = %+v\nwant       %+v", src.reqs, wantReqs)
	}
	events := h.events(t)
	wantLines := []string{"status baselined", flagsLine(1, 0, []string{`\Seen`}, nil), newLine(3, 0)}
	if lines := diffLines(events); !slices.Equal(lines, wantLines) {
		t.Errorf("events = %q, want %q", lines, wantLines)
	}
	for i, e := range events {
		if e.ID <= 0 || (i > 0 && e.ID <= events[i-1].ID) {
			t.Errorf("event %d has ID %d, want IDs assigned in ascending order", i, e.ID)
		}
	}
	row := h.row(t)
	if row.UIDValidity != 7 || row.UIDNext != 4 || row.HighestModSeq != 12 || !row.CondStore || row.Mode != "idle" || !row.LastSyncAt.Equal(engineAt) {
		t.Errorf("row = %+v, want UIDVALIDITY 7, UIDNEXT 4, HIGHESTMODSEQ 12, CONDSTORE, idle, synced at %v", row, engineAt)
	}
	if got := h.uids(t); !slices.Equal(got, []uint32{1, 2, 3}) {
		t.Errorf("known UIDs = %v, want [1 2 3]", got)
	}
}

// A row that SetStatus created has no checkpoint, so the pass baselines from
// zero and emits no gap.
func TestPassBaselinesARowWithoutCheckpoint(t *testing.T) {
	t.Parallel()
	h := newEngineHarness(t, EngineOptions{})
	if err := h.state.SetStatus(t.Context(), "host", "INBOX", "idle", StateWatching, ""); err != nil {
		t.Fatal(err)
	}
	src := &scriptSource{results: []scriptResult{{delta: baselineDelta(1)}}}
	h.pass(t, src, PassOptions{Mode: "idle"})
	if src.reqs[0].From != (mailstore.Checkpoint{}) {
		t.Errorf("From = %+v, want the zero checkpoint", src.reqs[0].From)
	}
	if lines := diffLines(h.events(t)); !slices.Equal(lines, []string{"status baselined"}) {
		t.Errorf("events = %q, want only the baselined status", lines)
	}
	if row := h.row(t); row.State != StateBaselined || row.UIDNext != 2 {
		t.Errorf("row = %+v, want state baselined at UIDNEXT 2", row)
	}
}

// Pass repeats while the source has more new mail, each batch starting at
// the checkpoint the last one stored. Only the first batch sweeps.
func TestPassLoopsUntilCurrent(t *testing.T) {
	t.Parallel()
	h := newEngineHarness(t, EngineOptions{
		Batch:   2,
		Snippet: func(s mailstore.Summary) string { return "about " + s.Subject },
	})
	src := &scriptSource{results: []scriptResult{
		{delta: baselineDelta(1, 2)},
		{delta: mailstore.Delta{
			State: diffState(6, 14, true),
			New:   []mailstore.Summary{diffSummary(3, 0), diffSummary(4, 0)},
			More:  true,
			UIDs:  []uint32{1, 2, 3, 4, 5},
		}},
		{delta: mailstore.Delta{State: diffState(6, 14, true), New: []mailstore.Summary{diffSummary(5, 0)}}},
	}}
	h.pass(t, src, PassOptions{Mode: "poll"})
	res := h.pass(t, src, PassOptions{Sweep: true, Mode: "poll"})
	if want := (PassResult{Events: 3, CondStore: true, Batches: 2}); res != want {
		t.Errorf("Pass = %+v, want %+v", res, want)
	}
	wantReqs := []mailstore.SyncRequest{
		{Batch: 2, Snippets: true},
		{From: mailstore.Checkpoint{UIDValidity: 7, UIDNext: 3, HighestModSeq: 10}, Sweep: true, Batch: 2, Snippets: true},
		{From: mailstore.Checkpoint{UIDValidity: 7, UIDNext: 5, HighestModSeq: 14}, Batch: 2, Snippets: true},
	}
	if !reflect.DeepEqual(src.reqs, wantReqs) {
		t.Errorf("requests = %+v\nwant       %+v", src.reqs, wantReqs)
	}
	events := h.events(t)
	wantLines := []string{"status baselined", newLine(3, 0), newLine(4, 0), newLine(5, 0)}
	if lines := diffLines(events); !slices.Equal(lines, wantLines) {
		t.Fatalf("events = %q, want %q", lines, wantLines)
	}
	if m := events[1].Message; m == nil || m.Snippet == nil || *m.Snippet != "about message 3" {
		t.Errorf("first new message = %+v, want snippet %q", m, "about message 3")
	}
	if row := h.row(t); row.UIDNext != 6 || row.Mode != "poll" || !row.LastSweepAt.Equal(engineAt) {
		t.Errorf("row = %+v, want UIDNEXT 6, poll, swept at %v", row, engineAt)
	}
	if got := h.uids(t); !slices.Equal(got, []uint32{1, 2, 3, 4, 5}) {
		t.Errorf("known UIDs = %v, want [1 2 3 4 5]", got)
	}
}

// A failed pass changes nothing: no events, no UID changes and no
// checkpoint advance, even when the failure comes after the events were
// appended inside the transaction.
func TestPassFailureLeavesStateUntouched(t *testing.T) {
	t.Parallel()
	next := diffState(4, 11, true)
	tests := []struct {
		name    string
		result  scriptResult
		mode    Mode
		wantErr error
		wantMsg string
	}{
		{
			name:    "source fails",
			result:  scriptResult{err: fmt.Errorf("dial: %w", mailstore.ErrUnavailable)},
			mode:    "idle",
			wantErr: mailstore.ErrUnavailable,
		},
		{
			name:    "source reports more without new mail",
			result:  scriptResult{delta: mailstore.Delta{State: next, More: true}},
			mode:    "idle",
			wantMsg: "returned none",
		},
		{
			// PutFolder rejects the mode after Append ran.
			name:    "database rejects the batch",
			result:  scriptResult{delta: mailstore.Delta{State: next, New: []mailstore.Summary{diffSummary(3, 0)}}},
			mode:    "bogus",
			wantMsg: "storing sync checkpoint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newEngineHarness(t, EngineOptions{})
			src := &scriptSource{results: []scriptResult{{delta: baselineDelta(1, 2)}, tt.result}}
			h.pass(t, src, PassOptions{Mode: "idle"})
			row, uids, lines, tail := h.row(t), h.uids(t), diffLines(h.events(t)), h.tail(t)

			_, err := h.engine.Pass(t.Context(), "host", "INBOX", src, PassOptions{Mode: tt.mode})
			switch {
			case err == nil:
				t.Fatal("Pass succeeded, want an error")
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Errorf("err = %v, want one matching %v", err, tt.wantErr)
			case tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg):
				t.Errorf("err = %v, want it to mention %q", err, tt.wantMsg)
			}
			if got := h.row(t); !reflect.DeepEqual(got, row) {
				t.Errorf("row = %+v, want it unchanged: %+v", got, row)
			}
			if got := h.uids(t); !slices.Equal(got, uids) {
				t.Errorf("known UIDs = %v, want %v", got, uids)
			}
			if got := diffLines(h.events(t)); !slices.Equal(got, lines) {
				t.Errorf("events = %q, want %q", got, lines)
			}
			if got := h.tail(t); got != tail {
				t.Errorf("Tail = %d, want %d: the appended events must roll back", got, tail)
			}
		})
	}
}

// A committed pass wakes a reader blocked in Wait through Notify, not
// because Wait happened to read the log after the commit.
func TestPassWakesWaiters(t *testing.T) {
	t.Parallel()
	h := newEngineHarness(t, EngineOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// Hold state.db's one connection, so the waiter's first Tail queues
	// for it. Wait takes its wake channel before that Tail, and the
	// connection goes to the queued Tail ahead of the pass, so the waiter
	// sees an empty log: only the pass's Notify can wake it.
	hold, err := h.db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	queued := h.db.Stats().WaitCount
	woke := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { woke <- h.log.Wait(ctx, 0) })
	defer func() {
		cancel() // a failed step must not leave Wait blocked until the timeout
		wg.Wait()
	}()
	engineAwaitConnQueue(ctx, t, h.db, queued+1)
	if err := hold.Close(); err != nil {
		t.Fatalf("releasing the connection: %v", err)
	}

	h.pass(t, &scriptSource{results: []scriptResult{{delta: baselineDelta(1)}}}, PassOptions{Mode: "idle"})
	if err := <-woke; err != nil {
		t.Errorf("Wait = %v, want it woken by the pass's Notify", err)
	}
}

// A message that arrives between a baseline's SELECT and its FETCH is past
// the UIDNEXT the checkpoint stores, so the baseline leaves it out of the
// known UIDs and the next pass reports it as new. Known, it would never be
// reported as new, and if it left before the next SELECT it would come back
// later as an expunged event the message count could not explain.
func TestBaselineKeepsOnlyUIDsBelowItsUIDNext(t *testing.T) {
	t.Parallel()
	h := newEngineHarness(t, EngineOptions{})
	baseline := mailstore.Delta{State: diffState(2, 10, true), Rebased: true, UIDs: []uint32{1, 2}}
	h.pass(t, &scriptSource{results: []scriptResult{{delta: baseline}}}, PassOptions{Mode: "idle"})
	if got := h.uids(t); !slices.Equal(got, []uint32{1}) {
		t.Errorf("known UIDs after the baseline = %v, want [1]: UID 2 is at the selected UIDNEXT", got)
	}
	// UID 2 leaves before the next SELECT, which then reports UIDNEXT 3
	// and no new mail. Sweeps find nothing gone.
	gone := mailstore.Delta{State: diffState(3, 12, true), UIDs: []uint32{1}}
	h.pass(t, &scriptSource{results: []scriptResult{{delta: gone}}}, PassOptions{Mode: "idle", Sweep: true})
	h.pass(t, &scriptSource{results: []scriptResult{{delta: gone}}}, PassOptions{Mode: "idle", Sweep: true})
	for _, e := range h.events(t) {
		if e.Kind == KindExpunged {
			t.Errorf("expunged event for %s, a message no new event reported", e.Ref)
		}
	}
}

// A pass that appends no events, such as a Google account's check every
// google_check that finds nothing, still commits its checkpoint but wakes
// no reader: Wait has nothing new to return.
func TestPassWithoutEventsWakesNoWaiter(t *testing.T) {
	t.Parallel()
	h := newEngineHarness(t, EngineOptions{})
	src := &scriptSource{results: []scriptResult{
		{delta: baselineDelta(1)},
		{delta: mailstore.Delta{State: diffState(2, 10, true)}},
	}}
	h.pass(t, src, PassOptions{Mode: "idle"})
	woken := h.log.changed()
	h.pass(t, src, PassOptions{Mode: "idle"})
	select {
	case <-woken:
		t.Error("a pass with no events woke the log's waiters")
	default:
	}
}

// engineAwaitConnQueue polls until db has queued want requests for a
// connection in total, or ctx ends.
func engineAwaitConnQueue(ctx context.Context, t *testing.T, db *sql.DB, want int64) {
	t.Helper()
	for db.Stats().WaitCount < want {
		select {
		case <-ctx.Done():
			t.Fatalf("connection queue never reached %d: %v", want, ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

// Pass holds no transaction while the source runs: state.db has one
// connection, so a read from inside Sync would block until its deadline if
// one were open.
func TestPassHoldsNoTransactionAcrossSync(t *testing.T) {
	t.Parallel()
	h := newEngineHarness(t, EngineOptions{})
	src := probeSource{
		inner: &scriptSource{results: []scriptResult{{delta: baselineDelta(1)}}},
		probe: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, err := h.log.Tail(ctx)
			return err
		},
	}
	h.pass(t, src, PassOptions{Mode: "idle"})
}

// folderSource is a CONDSTORE INBOX at UIDVALIDITY 7 that answers sync
// requests as C1 describes, for new mail and sweeps (its flags never change).
type folderSource struct {
	uids []uint32
	reqs []mailstore.SyncRequest
}

// add delivers n messages.
func (f *folderSource) add(n int) {
	for range n {
		f.uids = append(f.uids, uint32(len(f.uids)+1))
	}
}

func (f *folderSource) Sync(_ context.Context, req mailstore.SyncRequest) (mailstore.Delta, error) {
	f.reqs = append(f.reqs, req)
	d := mailstore.Delta{State: diffState(uint32(len(f.uids)+1), 10, true)}
	if req.From == (mailstore.Checkpoint{}) || req.From.UIDValidity != d.State.UIDValidity {
		d.Rebased = true
		d.UIDs = slices.Clone(f.uids)
		return d, nil
	}
	batch := req.Batch
	if batch == 0 {
		batch = mailstore.DefaultSyncBatch
	}
	for _, uid := range f.uids {
		if uid < req.From.UIDNext {
			continue
		}
		if len(d.New) == batch {
			d.More = true
			break
		}
		d.New = append(d.New, diffSummary(uid, 0))
	}
	if req.Sweep {
		d.UIDs = slices.Clone(f.uids)
	}
	return d, nil
}

// newUIDs lists the UIDs of the new events, in log order.
func newUIDs(t *testing.T, events []Event) []uint32 {
	t.Helper()
	var uids []uint32
	for _, e := range events {
		if e.Kind != KindNew {
			continue
		}
		r, err := ref.Parse(e.Ref)
		if err != nil {
			t.Fatalf("new event ref %q: %v", e.Ref, err)
		}
		uids = append(uids, r.UID)
	}
	return uids
}

// A commit that fails part way through a catch-up keeps the batches before
// it and loses the failed one whole. Running Pass again then emits every
// message exactly once overall.
func TestPassRerunAfterFailedCommitEmitsEachEventOnce(t *testing.T) {
	t.Parallel()
	h := newEngineHarness(t, EngineOptions{Batch: 2})
	src := &folderSource{}
	src.add(2)
	h.pass(t, src, PassOptions{Mode: "idle"}) // baseline: UIDs 1 and 2
	src.add(5)                                // UIDs 3 to 7 arrive

	errCrash := errors.New("disk I/O error")
	commits := 0
	h.engine.commitTx = func(tx *sql.Tx) error {
		commits++
		if commits == 2 {
			_ = tx.Rollback() // the crash: this batch never lands
			return errCrash
		}
		return tx.Commit()
	}
	res, err := h.engine.Pass(t.Context(), "host", "INBOX", src, PassOptions{Mode: "idle"})
	if !errors.Is(err, errCrash) {
		t.Fatalf("Pass err = %v, want %v", err, errCrash)
	}
	if want := (PassResult{Events: 2, CondStore: true, Batches: 1}); res != want {
		t.Errorf("failed Pass = %+v, want the first batch only: %+v", res, want)
	}
	if got := newUIDs(t, h.events(t)); !slices.Equal(got, []uint32{3, 4}) {
		t.Errorf("after the failure, new UIDs = %v, want [3 4]", got)
	}
	if row := h.row(t); row.UIDNext != 5 {
		t.Errorf("after the failure, UIDNEXT = %d, want 5 (the last committed batch)", row.UIDNext)
	}

	h.engine.commitTx = (*sql.Tx).Commit
	src.reqs = nil
	res = h.pass(t, src, PassOptions{Mode: "idle"})
	if want := (PassResult{Events: 3, CondStore: true, Batches: 2}); res != want {
		t.Errorf("rerun = %+v, want %+v", res, want)
	}
	if got := src.reqs[0].From.UIDNext; got != 5 {
		t.Errorf("rerun started at UIDNEXT %d, want 5", got)
	}
	if got := newUIDs(t, h.events(t)); !slices.Equal(got, []uint32{3, 4, 5, 6, 7}) {
		t.Errorf("new UIDs = %v, want each of 3 to 7 exactly once", got)
	}
	if row := h.row(t); row.UIDNext != 8 {
		t.Errorf("UIDNEXT = %d, want 8", row.UIDNext)
	}
	if got := h.uids(t); !slices.Equal(got, []uint32{1, 2, 3, 4, 5, 6, 7}) {
		t.Errorf("known UIDs = %v, want 1 to 7", got)
	}
}

// The batch reaching the source never exceeds the 500 messages one
// transaction may hold, whatever EngineOptions.Batch says.
func TestPassCapsBatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		batch int
		want  int
	}{
		{name: "oversized", batch: mailstore.DefaultSyncBatch + 1, want: mailstore.DefaultSyncBatch},
		{name: "at the cap", batch: mailstore.DefaultSyncBatch, want: mailstore.DefaultSyncBatch},
		{name: "below the cap", batch: 2, want: 2},
		{name: "zero", batch: 0, want: mailstore.DefaultSyncBatch},
		{name: "negative", batch: -1, want: mailstore.DefaultSyncBatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newEngineHarness(t, EngineOptions{Batch: tt.batch})
			src := &scriptSource{results: []scriptResult{{delta: baselineDelta(1)}}}
			h.pass(t, src, PassOptions{Mode: "idle"})
			if got := src.reqs[0].Batch; got != tt.want {
				t.Errorf("SyncRequest.Batch = %d, want %d", got, tt.want)
			}
		})
	}
}

// A batch whose new mail holds a message the source could not read still
// emits the rest, and the checkpoint moves past that message. A batch whose
// new mail is all unreadable is progress too, not errNoProgress.
func TestPassMovesPastUnreadableNewMail(t *testing.T) {
	t.Parallel()
	h := newEngineHarness(t, EngineOptions{Batch: 3})
	src := &scriptSource{results: []scriptResult{
		{delta: baselineDelta(1, 2)},
		{delta: mailstore.Delta{
			State:      diffState(8, 14, true),
			New:        []mailstore.Summary{diffSummary(3, 0), diffSummary(4, 0)},
			Unreadable: []uint32{5},
			More:       true,
		}},
		{delta: mailstore.Delta{State: diffState(8, 14, true), Unreadable: []uint32{6}, More: true}},
		{delta: mailstore.Delta{State: diffState(8, 14, true), New: []mailstore.Summary{diffSummary(7, 0)}}},
	}}
	h.pass(t, src, PassOptions{Mode: "poll"})
	if res, want := h.pass(t, src, PassOptions{Mode: "poll"}), (PassResult{Events: 5, CondStore: true, Batches: 3}); res != want {
		t.Errorf("Pass = %+v, want %+v", res, want)
	}
	var froms []uint32
	for _, r := range src.reqs[1:] {
		froms = append(froms, r.From.UIDNext)
	}
	if want := []uint32{3, 6, 7}; !slices.Equal(froms, want) {
		t.Errorf("batches started at UIDs %v, want %v", froms, want)
	}
	want := []string{"status baselined", newLine(3, 0), newLine(4, 0), unreadableLine(5), unreadableLine(6), newLine(7, 0)}
	if lines := diffLines(h.events(t)); !slices.Equal(lines, want) {
		t.Errorf("events = %q, want %q", lines, want)
	}
	if row := h.row(t); row.UIDNext != 8 {
		t.Errorf("row UIDNEXT = %d, want 8", row.UIDNext)
	}
}

// New mail the pass could not read joins the known UIDs, so a later sweep
// that finds it gone reports it expunged, and the cache can forget it.
func TestUnreadableNewMailIsExpungedLater(t *testing.T) {
	t.Parallel()
	h := newEngineHarness(t, EngineOptions{})
	src := &scriptSource{results: []scriptResult{
		{delta: baselineDelta(1, 2)},
		{delta: mailstore.Delta{State: diffState(5, 14, true), New: []mailstore.Summary{diffSummary(3, 0)}, Unreadable: []uint32{4}}},
		{delta: mailstore.Delta{State: diffState(5, 15, true), UIDs: []uint32{1, 2, 3}}},
	}}
	h.pass(t, src, PassOptions{Mode: "poll"})
	h.pass(t, src, PassOptions{Mode: "poll"})
	h.pass(t, src, PassOptions{Mode: "poll", Sweep: true})
	want := []string{"status baselined", newLine(3, 0), unreadableLine(4), expungedLine(4, 0)}
	if lines := diffLines(h.events(t)); !slices.Equal(lines, want) {
		t.Errorf("events = %q, want %q", lines, want)
	}
}
