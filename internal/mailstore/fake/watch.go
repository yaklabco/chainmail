package fake

// Watch support. The fake is a mailstore.Watcher: FolderState is STATUS and
// SyncFolder one sync pass, with the pass semantics mailstore.WatchConn
// documents, over the messages it holds. Each folder keeps a HIGHESTMODSEQ
// that every change to it raises, and each message the MODSEQ of its last
// change, so a pass from a checkpoint finds flag changes as CHANGEDSINCE
// does on a CONDSTORE server. SetCondStore(false) makes passes fetch every
// message's flags instead, as on a server without CONDSTORE.
//
// The fake syncs only folders that hold mail: every folder on a standard
// account, and All Mail, Trash and Spam on Gmail. Gmail's label folders are
// views of All Mail that it does not number for sync, and chainmail
// watches All Mail there.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// Watch method names, as Calls counts them and Fail takes them.
const (
	opOpenWatch   = "OpenWatch"
	opFolderState = "FolderState"
	opSyncFolder  = "SyncFolder"
	opSync        = "Sync"
	opIdle        = "Idle"
)

// idleStartedCap is how many Idle starts IdleStarted holds unread.
const idleStartedCap = 64

// errWatchClosed is what a watch connection returns once closed. It does
// not match mailstore.ErrUnavailable, so a caller that uses a connection
// after Close fails its test instead of reconnecting.
var errWatchClosed = errors.New("fake: watch connection used after Close")

var (
	_ mailstore.Watcher   = (*Store)(nil)
	_ mailstore.WatchConn = (*watchConn)(nil)
)

// SetCondStore says whether the server advertises CONDSTORE. FolderState
// and SyncFolder follow it at once. A watch connection keeps what the
// server advertised when it opened. The default is true.
func (s *Store) SetCondStore(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.condstore = on
}

// FailWatch makes the next OpenWatch, WatchConn.Sync or SyncFolder fail
// with err, then clears itself. A nil err clears a pending failure. Fail
// with one of the watch method names gives a failure that stays.
func (s *Store) FailWatch(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchFail = err
}

// ExternalFlags adds, then removes, flags on the message with uid in
// folder, as another client's STORE would: its MODSEQ rises and watch
// connections on the folder wake, but no Ops entry is recorded and no
// failure hook applies. On Gmail \Flagged also stars the message. It panics
// when folder holds no such message.
func (s *Store) ExternalFlags(folder string, uid uint32, add, remove []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mustStoredLocked(folder, uid)
	m.Flags = editSet(m.Flags, add, remove)
	if s.gmail {
		starFromFlags(m, add, remove)
	}
	s.touchLocked(m)
}

// ExternalExpunge deletes the message with uid in folder, as another
// client's EXPUNGE would, with no Ops entry and no failure hook. It panics
// when folder holds no such message.
func (s *Store) ExternalExpunge(folder string, uid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mustStoredLocked(folder, uid)
	s.changedLocked(folder, true)
	s.msgs = slices.DeleteFunc(s.msgs, func(o *Message) bool { return o == m })
}

// NewUIDValidity gives folder a new UIDVALIDITY, one above any it has had,
// and renumbers its messages from UID 1 in their UID order, as a server
// that rebuilt the folder would. Old refs into it go stale. A Gmail label
// folder stores no messages, so there it renumbers none: it forgets the UIDs
// the folder's views gave out, and they are numbered again from 1 as refs
// into it are next needed. SetUIDValidity, by contrast, keeps the UIDs. It
// panics when the folder does not exist.
func (s *Store) NewUIDValidity(folder string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustHaveFolderLocked(folder)
	v := s.everValidity[folder] + 1
	s.uidValidity[folder], s.everValidity[folder] = v, v
	s.nextUID[folder] = 1
	for _, m := range s.storedLocked(folder) {
		m.uid = s.nextUID[folder]
		s.nextUID[folder]++
	}
	for _, m := range s.msgs {
		delete(m.views, folder)
	}
}

// FolderState implements mailstore.Watcher. HighestModSeq is 0 without
// CONDSTORE, as STATUS cannot report it then.
func (s *Store) FolderState(ctx context.Context, folder string) (mailstore.FolderState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(opFolderState); err != nil {
		return mailstore.FolderState{}, err
	}
	if err := s.closedErrLocked(); err != nil {
		return mailstore.FolderState{}, err
	}
	if err := ctx.Err(); err != nil {
		return mailstore.FolderState{}, err
	}
	if err := s.watchableLocked(folder); err != nil {
		return mailstore.FolderState{}, err
	}
	return s.stateLocked(folder, s.condstore), nil
}

// SyncFolder implements mailstore.Watcher: one pass on folder, as on a
// pooled connection, with CONDSTORE as SetCondStore last set it.
func (s *Store) SyncFolder(ctx context.Context, folder string, req mailstore.SyncRequest) (mailstore.Delta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[opSyncFolder]++
	if err := s.watchErrLocked(ctx, opSyncFolder); err != nil {
		return mailstore.Delta{}, err
	}
	return s.syncLocked(folder, req, s.condstore)
}

// watchErrLocked is the error a pass or OpenWatch fails with: a closed
// store, else a standing Fail for method, else a pending FailWatch, which
// it clears, else a done context.
func (s *Store) watchErrLocked(ctx context.Context, method string) error {
	if err := s.closedErrLocked(); err != nil {
		return err
	}
	if err := s.fail[method]; err != nil {
		return err
	}
	if err := s.watchFail; err != nil {
		s.watchFail = nil
		return err
	}
	return ctx.Err()
}

// closedErrLocked refuses watch work on a closed store, as imapstore's
// closed pool does, with an error matching mailstore.ErrUnavailable.
func (s *Store) closedErrLocked() error {
	if s.closed {
		return fmt.Errorf("%w: the fake store %s is closed", mailstore.ErrUnavailable, s.account)
	}
	return nil
}

// watchableLocked refuses a folder the fake cannot sync: a missing one,
// as IMAP SELECT does, and a Gmail label folder.
func (s *Store) watchableLocked(folder string) error {
	f, err := s.folderLocked(folder)
	if err != nil {
		return &mailstore.FolderNotFoundError{Name: folder, Requested: folder}
	}
	if s.gmail && !holdsMail(f.Role) {
		return fmt.Errorf("%w: the fake syncs Gmail's All Mail, Trash and Spam, not the label folder %s", mailstore.ErrUnsupported, folder)
	}
	return nil
}

// stateLocked is what SELECT or STATUS reports for folder. CondStore is
// set only when the server advertises CONDSTORE and the folder keeps
// mod-sequences (a non-zero HIGHESTMODSEQ), as imapstore reports it.
func (s *Store) stateLocked(folder string, condstore bool) mailstore.FolderState {
	st := mailstore.FolderState{UIDValidity: s.uidValidity[folder], UIDNext: s.nextUID[folder]}
	if modseq := s.modseq[folder]; condstore && modseq > 0 {
		st.HighestModSeq, st.CondStore = modseq, true
	}
	return st
}

// syncLocked runs one pass on folder, as a server whose capabilities
// include CONDSTORE when condstore is set would answer it. The pass takes
// the CONDSTORE path only when d.State.CondStore is set. Whenever the UID
// set is asked for, Delta.UIDs is non-nil, even for an empty folder.
func (s *Store) syncLocked(folder string, req mailstore.SyncRequest, condstore bool) (mailstore.Delta, error) {
	if err := s.watchableLocked(folder); err != nil {
		return mailstore.Delta{}, err
	}
	d := mailstore.Delta{State: s.stateLocked(folder, condstore)}
	cs := d.State.CondStore
	msgs := s.storedLocked(folder)
	if req.From == (mailstore.Checkpoint{}) || req.From.UIDValidity != d.State.UIDValidity {
		d.Rebased = true
		d.UIDs = uidsOf(msgs)
		switch {
		case !cs:
			d.Changed, d.FullFlags = s.flagStates(msgs), true
		case s.gmail:
			// Gmail rebases list every X-GM-MSGID, so expunged refs stay
			// complete. As imapstore fetches (UID X-GM-MSGID) alone, the
			// entries carry no flags or labels, and FullFlags stays false.
			d.Changed = idStates(msgs)
		}
		return d, nil
	}
	if d.State.UIDNext > req.From.UIDNext {
		batch := req.Batch
		if batch <= 0 {
			batch = mailstore.DefaultSyncBatch
		}
		var fresh []*Message
		for _, m := range msgs {
			// UIDs at or above the selected UIDNEXT arrived after the
			// SELECT on a server and wait for the next pass. The fake
			// snapshots under its lock, so none are dropped here.
			if m.uid >= req.From.UIDNext && m.uid < d.State.UIDNext {
				fresh = append(fresh, m)
			}
		}
		if len(fresh) > batch {
			fresh, d.More = fresh[:batch], true
		}
		for _, m := range fresh {
			d.New = append(d.New, s.summaryLocked(m, s.refLocked(m), mailstore.SummaryOptions{Snippets: req.Snippets}))
		}
	}
	switch {
	case !cs:
		d.Changed, d.FullFlags, d.UIDs = s.flagStates(msgs), true, uidsOf(msgs)
	case d.State.HighestModSeq > req.From.HighestModSeq && req.From.UIDNext > 1:
		for _, m := range msgs {
			if m.uid < req.From.UIDNext && m.modseq > req.From.HighestModSeq {
				d.Changed = append(d.Changed, s.flagState(m))
			}
		}
	}
	if req.Sweep && cs {
		d.UIDs = uidsOf(msgs)
	}
	return d, nil
}

// storedLocked returns the messages stored in folder, ascending by UID.
func (s *Store) storedLocked(folder string) []*Message {
	var out []*Message
	for _, m := range s.msgs {
		if m.Folder == folder {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b *Message) int { return cmp.Compare(a.uid, b.uid) })
	return out
}

// mustStoredLocked returns the message stored in folder under uid, not a
// Gmail label view of one, and panics when there is none.
func (s *Store) mustStoredLocked(folder string, uid uint32) *Message {
	i := slices.IndexFunc(s.msgs, func(m *Message) bool { return m.Folder == folder && m.uid == uid })
	if i < 0 {
		panic(fmt.Sprintf("fake: no message %d in %q", uid, folder))
	}
	return s.msgs[i]
}

// flagState is m's flags, and on Gmail its X-GM-MSGID and labels, as
// UID FETCH (UID FLAGS X-GM-MSGID X-GM-LABELS) reports them.
func (s *Store) flagState(m *Message) mailstore.FlagState {
	fs := mailstore.FlagState{UID: m.uid, Flags: slices.Clone(m.Flags)}
	if s.gmail {
		fs.GmailMsgID, fs.Labels = m.GmailMsgID, slices.Clone(m.Labels)
	}
	return fs
}

// flagStates is flagState of each message, never nil.
func (s *Store) flagStates(msgs []*Message) []mailstore.FlagState {
	out := make([]mailstore.FlagState, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, s.flagState(m))
	}
	return out
}

// idStates is each Gmail message's UID and X-GM-MSGID alone, as UID FETCH
// (UID X-GM-MSGID) reports them, never nil.
func idStates(msgs []*Message) []mailstore.FlagState {
	out := make([]mailstore.FlagState, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, mailstore.FlagState{UID: m.uid, GmailMsgID: m.GmailMsgID})
	}
	return out
}

// uidsOf is the UIDs of msgs in their order, never nil.
func uidsOf(msgs []*Message) []uint32 {
	out := make([]uint32, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.uid)
	}
	return out
}

// changedLocked records a change to folder: its HIGHESTMODSEQ rises, and
// every watch connection on it gets a pending wake, with Expunged when a
// message left it. It returns the new HIGHESTMODSEQ.
func (s *Store) changedLocked(folder string, expunged bool) uint64 {
	s.modseq[folder]++
	for c := range s.watches {
		if c.folder != folder {
			continue
		}
		c.pending.Changed = true
		c.pending.Expunged = c.pending.Expunged || expunged
		c.notifyLocked()
	}
	return s.modseq[folder]
}

// touchLocked stamps m with a new MODSEQ in its folder, after it arrived
// there or its flags or labels changed.
func (s *Store) touchLocked(m *Message) {
	m.modseq = s.changedLocked(m.Folder, false)
}

// SetIdle says whether the server advertises IDLE, for watch connections
// opened after the call. The default is true.
func (s *Store) SetIdle(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idle = on
}

// SetAfter replaces the timer Idle waits on for its max, so a test with a
// fake clock can end an Idle without waiting. Nil restores time.After.
func (s *Store) SetAfter(after func(time.Duration) <-chan time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if after == nil {
		after = time.After
	}
	s.after = after
}

// DropWatches drops every open watch connection, as a network failure
// would: an Idle in progress returns, and each one's next Sync or Idle
// fails with an error matching mailstore.ErrUnavailable. A dropped
// connection counts in WatchConns until it is closed. Connections opened
// afterwards work.
func (s *Store) DropWatches() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropWatchesLocked()
}

// dropWatchesLocked marks every open watch connection dropped and wakes
// any Idle waiting on one.
func (s *Store) dropWatchesLocked() {
	for c := range s.watches {
		c.dropped = true
		c.notifyLocked()
	}
}

// closeWatchesLocked closes every open watch connection without waiting
// for its owner, as Store.Close does: each is dropped, so an Idle in
// progress returns and the next Sync or Idle fails with an error matching
// mailstore.ErrUnavailable, and it no longer counts in WatchConns. The
// owner's own Close still succeeds.
func (s *Store) closeWatchesLocked() {
	s.dropWatchesLocked()
	clear(s.watches)
}

// WatchConns returns how many watch connections are open: opened and not
// yet closed, dropped ones included.
func (s *Store) WatchConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watches)
}

// IdleStarted delivers the folder of each Idle that starts to wait, once
// its max timer is running, so a test can change the folder or fire the
// timer knowing the Idle is in progress. An Idle that returns a pending
// change at once sends nothing. It holds the latest 64 unread starts and
// drops older ones, so it never blocks an Idle.
func (s *Store) IdleStarted() <-chan string { return s.idleStarted }

// noteIdle sends folder to IdleStarted, dropping the oldest unread start
// when it is full.
func (s *Store) noteIdle(folder string) {
	for {
		select {
		case s.idleStarted <- folder:
			return
		default:
		}
		select {
		case <-s.idleStarted:
		default:
		}
	}
}

// OpenWatch implements mailstore.Watcher. The connection keeps the
// CONDSTORE and IDLE the store advertised when it opened.
func (s *Store) OpenWatch(ctx context.Context, folder string) (mailstore.WatchConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[opOpenWatch]++
	if err := s.watchErrLocked(ctx, opOpenWatch); err != nil {
		return nil, err
	}
	if err := s.watchableLocked(folder); err != nil {
		return nil, err
	}
	c := &watchConn{s: s, folder: folder, condstore: s.condstore, idle: s.idle, wake: make(chan struct{}, 1)}
	s.watches[c] = true
	return c, nil
}

// watchConn is a fake watch connection. It stays on one folder.
type watchConn struct {
	s         *Store
	folder    string
	condstore bool
	idle      bool
	// wake holds a token when a waiting Idle should look again: a change
	// to the folder, a drop or Close.
	wake chan struct{}
	// Guarded by s.mu. pending is the wake the changes since Idle last took
	// it add up to, less what a Sync since then settled.
	pending mailstore.Wake
	dropped bool
	closed  bool
}

// notifyLocked wakes a waiting Idle without blocking.
func (c *watchConn) notifyLocked() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// errLocked is the error a closed or dropped connection fails with.
func (c *watchConn) errLocked() error {
	switch {
	case c.closed:
		return errWatchClosed
	case c.dropped:
		return fmt.Errorf("%w: watch connection on %s dropped", mailstore.ErrUnavailable, c.folder)
	}
	return nil
}

// takeLocked returns the pending wake and clears it.
func (c *watchConn) takeLocked() mailstore.Wake {
	w := c.pending
	c.pending = mailstore.Wake{}
	return w
}

// settleLocked updates the pending wake after a pass that returned d for
// req. The pass reports every arrival and flag change made before it, so a
// pending Changed alone clears. A pending Expunged clears only when the
// pass returned the full UID set (a rebase, a sweep, or the full-flags
// path), from which deletions show. Otherwise the whole wake stays for the
// next Idle, as imapstore keeps an EXPUNGE report until Idle takes it, so
// the watcher still learns that it should sweep.
func (c *watchConn) settleLocked(req mailstore.SyncRequest, d mailstore.Delta) {
	if c.pending.Expunged && !d.Rebased && !d.FullFlags && !req.Sweep {
		return
	}
	c.takeLocked()
}

// Sync implements mailstore.WatchConn. It settles the pending wake as
// settleLocked says.
func (c *watchConn) Sync(ctx context.Context, req mailstore.SyncRequest) (mailstore.Delta, error) {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[opSync]++
	if err := c.errLocked(); err != nil {
		return mailstore.Delta{}, err
	}
	if err := s.watchErrLocked(ctx, opSync); err != nil {
		return mailstore.Delta{}, err
	}
	d, err := s.syncLocked(c.folder, req, c.condstore)
	if err != nil {
		return mailstore.Delta{}, err
	}
	c.settleLocked(req, d)
	return d, nil
}

// IdleSupported implements mailstore.WatchConn.
func (c *watchConn) IdleSupported() bool { return c.idle }

// Idle implements mailstore.WatchConn. A pending wake (see settleLocked)
// returns at once. Otherwise Idle starts the store's After timer for
// limit, tells IdleStarted, and waits for a change, the timer, a drop,
// Close or ctx. An ended ctx wins over a ready wake or timer and leaves
// the wake pending.
func (c *watchConn) Idle(ctx context.Context, limit time.Duration) (mailstore.Wake, error) {
	s := c.s
	s.mu.Lock()
	s.calls[opIdle]++
	if err := c.idleErrLocked(ctx); err != nil {
		s.mu.Unlock()
		return mailstore.Wake{}, err
	}
	if w := c.takeLocked(); w != (mailstore.Wake{}) {
		s.mu.Unlock()
		return w, nil
	}
	after := s.after
	s.mu.Unlock()

	timer := after(limit)
	s.noteIdle(c.folder)
	for {
		select {
		case <-ctx.Done():
			return mailstore.Wake{}, ctx.Err()
		case <-timer:
			// ctx may have ended as the timer fired; it wins, as in imapstore.
			return mailstore.Wake{}, ctx.Err()
		case <-c.wake:
		}
		s.mu.Lock()
		err := c.errLocked()
		if err == nil {
			// Recheck ctx before taking the wake, as imapstore does after
			// DONE: an Idle whose ctx ended returns ctx.Err() and leaves
			// the wake pending for the next one.
			err = ctx.Err()
		}
		var w mailstore.Wake
		if err == nil {
			w = c.takeLocked()
		}
		s.mu.Unlock()
		if err != nil {
			return mailstore.Wake{}, err
		}
		if w != (mailstore.Wake{}) {
			return w, nil
		}
	}
}

// idleErrLocked is the error Idle fails with before it waits.
func (c *watchConn) idleErrLocked(ctx context.Context) error {
	if err := c.errLocked(); err != nil {
		return err
	}
	if !c.idle {
		return fmt.Errorf("%w: the server has no IDLE", mailstore.ErrUnsupported)
	}
	if err := c.s.fail[opIdle]; err != nil {
		return err
	}
	return ctx.Err()
}

// Close implements mailstore.WatchConn. It is idempotent, and an Idle in
// progress then fails as closed.
func (c *watchConn) Close() error {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c.closed = true
	delete(s.watches, c)
	c.notifyLocked()
	return nil
}
