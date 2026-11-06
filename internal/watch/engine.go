package watch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// Source runs a sync pass. mailstore.WatchConn is one; storeSource adapts
// a mailstore.Watcher's pooled SyncFolder.
type Source interface {
	Sync(ctx context.Context, req mailstore.SyncRequest) (mailstore.Delta, error)
}

// A watch connection syncs the folder it stays selected on, so it is a
// Source as it is.
var _ Source = mailstore.WatchConn(nil)

// storeSource syncs Folder through W's pooled connections.
type storeSource struct {
	W      mailstore.Watcher
	Folder string
}

// Sync runs one pass on Folder with W.SyncFolder.
func (s storeSource) Sync(ctx context.Context, req mailstore.SyncRequest) (mailstore.Delta, error) {
	return s.W.SyncFolder(ctx, s.Folder, req)
}

// EngineOptions configure an Engine.
type EngineOptions struct {
	// Now stamps events and checkpoints. Nil means time.Now.
	Now func() time.Time
	// Snippet renders a summary's snippet: the daemon passes the function
	// search rows use. Nil means no snippets.
	Snippet func(mailstore.Summary) string
	// Batch caps new messages per transaction. 0 or less means
	// mailstore.DefaultSyncBatch, which is also the most it may be: a larger
	// value is lowered to it.
	Batch int
	// OnNew, when set, runs inside each batch transaction after the batch's
	// events are appended, with the batch's new messages in UID order. The
	// events it returns are appended in the same transaction, and Append
	// sets each one's ID, and its At when zero, in place on the returned
	// slice. It may use only tx: state.db has one connection, and tx holds
	// it. An error rolls the batch back, so the next Pass fetches the same
	// messages and calls OnNew with them again. It is not called for a batch
	// with no new messages, such as a baseline. It must not modify news.
	OnNew func(ctx context.Context, tx *sql.Tx, account string, news []NewMail) ([]Event, error)
}

// NewMail is one new message of a batch: its summary and the new event
// already appended for it (ID set).
type NewMail struct {
	Summary mailstore.Summary
	Event   Event
}

// errNoProgress means a source reported more new messages but returned
// none, not even as unreadable. Pass stops rather than ask again from the same checkpoint forever.
var errNoProgress = errors.New("sync source reported more new messages but returned none")

// Engine turns sync passes into events and checkpoints in state.db. It
// keeps no state of its own between passes, so one Engine serves every
// folder of every account. Passes on different folders may run at once;
// state.db's single connection serializes their transactions.
type Engine struct {
	db      *sql.DB
	log     *Log
	state   *State
	now     func() time.Time
	snippet func(mailstore.Summary) string
	batch   int
	onNew   func(ctx context.Context, tx *sql.Tx, account string, news []NewMail) ([]Event, error)
	// commitTx commits a batch's transaction. Tests replace it to fail a
	// commit the way a crash or a full disk would.
	commitTx func(*sql.Tx) error
}

// NewEngine returns an Engine writing to db, which must hold the state
// schema, through log and state.
func NewEngine(db *sql.DB, log *Log, state *State, opts EngineOptions) *Engine {
	e := &Engine{
		db: db, log: log, state: state, now: opts.Now, snippet: opts.Snippet, batch: opts.Batch, onNew: opts.OnNew,
		commitTx: (*sql.Tx).Commit,
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.batch <= 0 || e.batch > mailstore.DefaultSyncBatch {
		e.batch = mailstore.DefaultSyncBatch
	}
	return e
}

// PassOptions control one Pass.
type PassOptions struct {
	// Sweep fetches the full UID set and emits expunged events.
	Sweep bool
	// Mode is recorded on the folder row: ModeIdle or ModePoll.
	Mode Mode
}

// PassResult reports what a Pass did.
type PassResult struct {
	Events    int  // events appended across every batch
	Baselined bool // the folder was baselined (first pass or UIDVALIDITY change)
	CondStore bool
	Batches   int
}

// Pass syncs one folder until it is current: it repeats while Delta.More,
// committing each batch's events, UID changes and checkpoint in one
// transaction, and calls Log.Notify after each commit that appended events.
//
// Each batch reads the stored checkpoint, so it starts where the last
// committed batch stopped, and runs src.Sync with no transaction open. Only
// the first batch sweeps. On any error the batch's transaction rolls back,
// the checkpoint stays where the last commit put it, and the next Pass
// fetches the same range again, so no event is lost or repeated. The result
// then counts the batches committed before the error. Errors from src keep
// their chain, such as mailstore.ErrUnavailable or a
// *mailstore.FolderNotFoundError. A src that needs a redial after a batch
// (mailstore.Redialer) ends the pass there, with no error, and the caller
// dials again.
func (e *Engine) Pass(ctx context.Context, account, folder string, src Source, opts PassOptions) (PassResult, error) {
	var res PassResult
	sweep := opts.Sweep
	for {
		more, err := e.step(ctx, account, folder, src, sweep, opts.Mode, &res)
		if err != nil {
			return res, err
		}
		if !more {
			return res, nil
		}
		if r, ok := src.(mailstore.Redialer); ok && r.NeedsRedial() {
			// The batch committed, but its connection is dead. The next
			// connection's pass resumes from the checkpoint.
			return res, nil
		}
		sweep = false // the first batch already compared the full UID set
	}
}

// step runs one batch and reports whether more new mail remains. Every
// read happens before the transaction begins: state.db has one connection,
// so a read inside it would wait for itself.
func (e *Engine) step(ctx context.Context, account, folder string, src Source, sweep bool, mode Mode, res *PassResult) (bool, error) {
	prev, ok, err := e.state.Folder(ctx, account, folder)
	if err != nil {
		return false, fmt.Errorf("reading sync checkpoint: %w", err)
	}
	req := mailstore.SyncRequest{Sweep: sweep, Batch: e.batch, Snippets: e.snippet != nil}
	if ok && prev.UIDValidity != 0 {
		req.From = mailstore.Checkpoint{UIDValidity: prev.UIDValidity, UIDNext: prev.UIDNext, HighestModSeq: prev.HighestModSeq}
	}
	d, err := src.Sync(ctx, req)
	if err != nil {
		return false, fmt.Errorf("syncing folder: %w", err)
	}
	if d.More && len(d.New) == 0 && len(d.Unreadable) == 0 {
		return false, errNoProgress
	}
	// Known UIDs matter only for expunges and comparisons without
	// CONDSTORE: a sweep (the first batch only) or full flags, on a folder
	// with a checkpoint.
	var known map[uint32]knownUID
	if !d.Rebased && (sweep || d.FullFlags) {
		if known, err = e.state.uids(ctx, account, folder); err != nil {
			return false, fmt.Errorf("reading known UIDs: %w", err)
		}
	}
	dr := diff(prev, known, d, diffInput{Account: account, Folder: folder, Sweep: sweep, At: e.now(), Snippet: e.snippet})
	if mode != "" {
		dr.Next.Mode = mode
	}
	appended, err := e.commit(ctx, dr)
	if err != nil {
		return false, err
	}
	res.Events += appended
	res.Batches++
	res.Baselined = res.Baselined || d.Rebased
	res.CondStore = d.State.CondStore
	return d.More, nil
}

// commit writes one batch in one transaction and, once it has committed,
// wakes blocked readers when it appended events. It returns how many events
// it appended, the OnNew hook's included. A batch without events, such as
// a Google account's check every google_check that finds nothing, gives a
// reader nothing to read.
func (e *Engine) commit(ctx context.Context, dr diffResult) (int, error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning sync batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report

	account, folder := dr.Next.Account, dr.Next.Folder
	if len(dr.Events) > 0 {
		if err := e.log.Append(ctx, tx, dr.Events); err != nil {
			return 0, fmt.Errorf("appending events: %w", err)
		}
	}
	extra, err := e.runOnNew(ctx, tx, account, dr)
	if err != nil {
		return 0, err
	}
	if err := PutFolder(ctx, tx, dr.Next); err != nil {
		return 0, fmt.Errorf("storing sync checkpoint: %w", err)
	}
	if dr.Replace != nil {
		if err := replaceUIDs(ctx, tx, account, folder, dr.Replace); err != nil {
			return 0, fmt.Errorf("replacing known UIDs: %w", err)
		}
	}
	if len(dr.Delete) > 0 {
		if err := deleteUIDs(ctx, tx, account, folder, dr.Delete); err != nil {
			return 0, fmt.Errorf("deleting expunged UIDs: %w", err)
		}
	}
	if len(dr.Put) > 0 {
		if err := putUIDs(ctx, tx, account, folder, dr.Put); err != nil {
			return 0, fmt.Errorf("storing known UIDs: %w", err)
		}
	}
	if err := e.commitTx(tx); err != nil {
		return 0, fmt.Errorf("committing sync batch: %w", err)
	}
	appended := len(dr.Events) + len(extra)
	if appended > 0 {
		e.log.Notify()
	}
	return appended, nil
}

// runOnNew calls the OnNew hook in tx with the batch's new messages, after
// their events are appended, and appends the events it returns. It does
// nothing without a hook or without new messages.
func (e *Engine) runOnNew(ctx context.Context, tx *sql.Tx, account string, dr diffResult) ([]Event, error) {
	if e.onNew == nil || len(dr.New) == 0 {
		return nil, nil
	}
	news, err := pairNewMail(dr)
	if err != nil {
		return nil, err
	}
	extra, err := e.onNew(ctx, tx, account, news)
	if err != nil {
		return nil, fmt.Errorf("running the new mail hook: %w", err)
	}
	if err := e.log.Append(ctx, tx, extra); err != nil {
		return nil, fmt.Errorf("appending new mail hook events: %w", err)
	}
	return extra, nil
}

// pairNewMail pairs each of dr.New with its new event, which Append has
// stamped with an ID. diff builds the new events in the order of dr.New, so
// the i-th new event belongs to dr.New[i]; a mismatch is a bug in diff.
func pairNewMail(dr diffResult) ([]NewMail, error) {
	var events []Event
	for _, ev := range dr.Events {
		if ev.Kind == KindNew {
			events = append(events, ev)
		}
	}
	if len(events) != len(dr.New) {
		return nil, fmt.Errorf("sync batch has %d new events for %d new messages", len(events), len(dr.New))
	}
	news := make([]NewMail, len(dr.New))
	for i, s := range dr.New {
		if events[i].Ref != s.Ref.String() {
			return nil, fmt.Errorf("new event %d is for %s, not %s", i, events[i].Ref, s.Ref)
		}
		news[i] = NewMail{Summary: s, Event: events[i]}
	}
	return news, nil
}
