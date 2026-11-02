package storetest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// WatchHarness gives RunWatch a fresh account for each case.
type WatchHarness struct {
	// Folder is the folder the cases sync and watch: INBOX on a standard
	// account, GmailAllMail on a Google one. It starts empty.
	Folder string
	// Gmail says the Watcher is a Google account, whose passes carry
	// X-GM-MSGID. CondStore says the server advertises CONDSTORE and Folder
	// keeps mod-sequences, and Idle that the server advertises IDLE.
	Gmail     bool
	CondStore bool
	Idle      bool
	// RenumberDropsSessions says WatchRemote.NewUIDValidity drops every
	// session of the account, the Watcher's watch connection included, as
	// Dovecot's does. Only then may the first watch pass after it redial
	// once, as the supervisor would. Otherwise the same watch connection
	// must keep working across the renumbering.
	RenumberDropsSessions bool
	// New returns a Watcher on an account of the case's own and a
	// WatchRemote that changes the same account. The harness closes the
	// Watcher when t ends.
	New func(t *testing.T) (mailstore.Watcher, WatchRemote)
}

// WatchRemote changes an account as another mail client would, on a
// connection of its own and never through the Watcher under test.
type WatchRemote struct {
	// Append stores m in folder with m.Flags, so without \Seen unless
	// m.Flags has it, and returns its UID there.
	Append func(t *testing.T, folder string, m Message) uint32
	// SetFlag adds flag to the message with uid in folder, or removes it
	// when on is false.
	SetFlag func(t *testing.T, folder string, uid uint32, flag string, on bool)
	// Expunge removes the message with uid from folder.
	Expunge func(t *testing.T, folder string, uid uint32)
	// NewUIDValidity gives folder a new UIDVALIDITY and its messages new UIDs.
	// When it drops the account's sessions, the Watcher's included, as a
	// server-side reset can, the harness sets RenumberDropsSessions.
	NewUIDValidity func(t *testing.T, folder string)
	// Flags returns the flags of the message with uid in folder, read
	// without changing them.
	Flags func(t *testing.T, folder string, uid uint32) []string
}

// missingWatchFolder is a folder no harness account has.
const missingWatchFolder = "chainmail-watch-missing"

// passFunc runs one sync pass on the folder under test and fails t on an error.
type passFunc func(t *testing.T, req mailstore.SyncRequest) mailstore.Delta

// passCase is one case of the pass semantics, run on a watch connection and
// on a pooled one.
type passCase struct {
	name string
	run  func(t *testing.T, h WatchHarness, r WatchRemote, pass passFunc)
}

var passCases = []passCase{
	{"baseline", watchBaseline},
	{"baseline of an empty folder", watchBaselineEmpty},
	{"new mail", watchNewMail},
	{"empty range answer dropped", watchEmptyRange},
	{"batching", watchBatching},
	{"flag change", watchFlagChange},
	{"sweep", watchSweep},
	{"sweep of an emptied folder", watchSweepEmptied},
	{"uidvalidity change", watchUIDValidity},
	{"reads never set seen", watchNeverSeen},
}

// RunWatch runs the mailstore.Watcher conformance cases: the sync pass
// semantics of mailstore.SyncRequest on a watch connection and on a pooled
// one, FolderState, a missing folder, and IDLE when the harness has it.
func RunWatch(t *testing.T, h WatchHarness) {
	t.Helper()
	paths := []struct {
		name string
		open func(t *testing.T, w mailstore.Watcher, folder string) (passFunc, func())
	}{
		{"watch", watchPass},
		{"pooled", pooledPass},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			for _, c := range passCases {
				t.Run(c.name, func(t *testing.T) {
					w, r := h.New(t)
					pass, renumbered := p.open(t, w, h.Folder)
					if renumbered != nil && h.RenumberDropsSessions {
						renumber := r.NewUIDValidity
						r.NewUIDValidity = func(t *testing.T, folder string) {
							renumber(t, folder)
							renumbered()
						}
					}
					c.run(t, h, r, pass)
				})
			}
		})
	}
	t.Run("missing folder", func(t *testing.T) { watchMissingFolder(t, h) })
	t.Run("folder state", func(t *testing.T) { watchFolderState(t, h) })
	t.Run("idle supported", func(t *testing.T) {
		w, _ := h.New(t)
		if got := openWatch(t, w, h.Folder).IdleSupported(); got != h.Idle {
			t.Errorf("IdleSupported = %v, want %v", got, h.Idle)
		}
	})
	idleCases := []struct {
		name string
		run  func(t *testing.T, h WatchHarness)
	}{
		{"idle wakes on append", watchIdleAppend},
		{"idle wakes on expunge", watchIdleExpunge},
		{"idle returns after max", watchIdleMax},
		{"idle returns when ctx ends", watchIdleCancel},
		{"sync after a cancelled idle", watchSyncAfterCancel},
	}
	for _, c := range idleCases {
		t.Run(c.name, func(t *testing.T) {
			if !h.Idle {
				t.Skip("the server has no IDLE")
			}
			c.run(t, h)
		})
	}
}

// openWatch opens a watch connection on folder and closes it when t ends.
func openWatch(t *testing.T, w mailstore.Watcher, folder string) mailstore.WatchConn {
	t.Helper()
	wc, err := w.OpenWatch(t.Context(), folder)
	if err != nil {
		t.Fatalf("OpenWatch(%q) = %v", folder, err)
	}
	t.Cleanup(func() { _ = wc.Close() }) // the case is over; a failed LOGOUT changes nothing
	return wc
}

// watchPass runs passes on one watch connection, as the supervisor does,
// and returns the hook RunWatch calls after NewUIDValidity when the
// harness has RenumberDropsSessions. A dropped watch connection is the
// caller's to redial (WatchConn.Sync), so the first pass after the hook
// closes the lost connection and redials once. Any other pass that loses
// the connection fails the case, and without RenumberDropsSessions the
// hook never runs, so the same connection must keep working.
func watchPass(t *testing.T, w mailstore.Watcher, folder string) (passFunc, func()) {
	t.Helper()
	wc := openWatch(t, w, folder)
	renumbered := false
	pass := func(t *testing.T, req mailstore.SyncRequest) mailstore.Delta {
		t.Helper()
		d, err := wc.Sync(t.Context(), req)
		if err != nil && renumbered && errors.Is(err, mailstore.ErrUnavailable) {
			t.Logf("the renumbering dropped the watch connection (%v); dialing again", err)
			_ = wc.Close() // release the lost connection; its error says only that it was lost
			wc = openWatch(t, w, folder)
			d, err = wc.Sync(t.Context(), req)
		}
		renumbered = false
		if err != nil {
			t.Fatalf("WatchConn.Sync(%+v) = %v", req, err)
		}
		return d
	}
	return pass, func() { renumbered = true }
}

// pooledPass runs passes on pooled connections. SyncFolder redials a
// dropped connection itself, so it needs no hook after a renumbering.
func pooledPass(_ *testing.T, w mailstore.Watcher, folder string) (passFunc, func()) {
	return func(t *testing.T, req mailstore.SyncRequest) mailstore.Delta {
		t.Helper()
		d, err := w.SyncFolder(t.Context(), folder, req)
		if err != nil {
			t.Fatalf("SyncFolder(%q, %+v) = %v", folder, req, err)
		}
		return d
	}, nil
}

// watchMsg is a message for the watch cases, told apart by token.
func watchMsg(token string) Message {
	return Message{
		Date: day(3), From: "sender@example.test", Subject: "watch " + token,
		MessageID: token + "@watch.example", Body: "body of " + token,
	}
}

// checkpoint is what a caller stores after d, as the sync engine does: the
// folder's state, or after a batch with More, the UID after the batch's last.
func checkpoint(d mailstore.Delta) mailstore.Checkpoint {
	cp := mailstore.Checkpoint{UIDValidity: d.State.UIDValidity, UIDNext: d.State.UIDNext, HighestModSeq: d.State.HighestModSeq}
	if d.More && len(d.New) > 0 {
		cp.UIDNext = d.New[len(d.New)-1].Ref.UID + 1
	}
	return cp
}

func newUIDs(d mailstore.Delta) []uint32 {
	uids := make([]uint32, len(d.New))
	for i, s := range d.New {
		uids[i] = s.Ref.UID
	}
	return uids
}

func changedUIDs(d mailstore.Delta) []uint32 {
	uids := make([]uint32, len(d.Changed))
	for i, f := range d.Changed {
		uids[i] = f.UID
	}
	return uids
}

func changedOf(d mailstore.Delta, uid uint32) (mailstore.FlagState, bool) {
	i := slices.IndexFunc(d.Changed, func(f mailstore.FlagState) bool { return f.UID == uid })
	if i < 0 {
		return mailstore.FlagState{}, false
	}
	return d.Changed[i], true
}

// checkState checks d.State against what the harness says the server offers.
func checkState(t *testing.T, h WatchHarness, d mailstore.Delta) {
	t.Helper()
	st := d.State
	if st.UIDValidity == 0 || st.UIDNext == 0 {
		t.Errorf("State = %+v, want UIDVALIDITY and UIDNEXT", st)
	}
	if st.CondStore != h.CondStore || (st.HighestModSeq != 0) != h.CondStore {
		t.Errorf("State = %+v, want CondStore %v with a HIGHESTMODSEQ exactly when CONDSTORE", st, h.CondStore)
	}
}

func watchBaseline(t *testing.T, h WatchHarness, r WatchRemote, pass passFunc) {
	u1 := r.Append(t, h.Folder, watchMsg("b1"))
	u2 := r.Append(t, h.Folder, watchMsg("b2"))
	d := pass(t, mailstore.SyncRequest{})
	checkState(t, h, d)
	if !d.Rebased || len(d.New) != 0 || d.More {
		t.Errorf("baseline: Rebased %v, %d new, More %v; want a rebase with nothing new", d.Rebased, len(d.New), d.More)
	}
	if !slices.Equal(d.UIDs, []uint32{u1, u2}) {
		t.Errorf("baseline UIDs = %v, want [%d %d]", d.UIDs, u1, u2)
	}
	if d.State.UIDNext <= u2 {
		t.Errorf("baseline UIDNext = %d, want above %d", d.State.UIDNext, u2)
	}
	checkRebaseChanged(t, h, d)
}

// checkRebaseChanged checks the Changed of a rebase. Without CONDSTORE it is
// every message with its flags, and FullFlags is set. With CONDSTORE it is
// every message on Gmail, for its X-GM-MSGID, and nothing elsewhere, and
// FullFlags is unset. Entries carry an X-GM-MSGID exactly on Gmail.
// CONDSTORE is the pass's own d.State.CondStore, not the harness's: a
// server may report a folder without mod-sequences (NOMODSEQ) after a
// renumbering, and the pass then owes the rebase without CONDSTORE. The
// callers check d.State against the harness first.
func checkRebaseChanged(t *testing.T, h WatchHarness, d mailstore.Delta) {
	t.Helper()
	switch {
	case !d.State.CondStore:
		if !d.FullFlags || !slices.Equal(changedUIDs(d), d.UIDs) {
			t.Errorf("rebase without CONDSTORE: FullFlags %v, Changed UIDs %v; want every message's flags, %v", d.FullFlags, changedUIDs(d), d.UIDs)
		}
	case h.Gmail:
		if d.FullFlags || !slices.Equal(changedUIDs(d), d.UIDs) {
			t.Errorf("Gmail rebase with CONDSTORE: FullFlags %v, Changed UIDs %v; want every message, %v, and no FullFlags", d.FullFlags, changedUIDs(d), d.UIDs)
		}
	default:
		if len(d.Changed) != 0 || d.FullFlags {
			t.Errorf("rebase with CONDSTORE: Changed %v, FullFlags %v; want neither", d.Changed, d.FullFlags)
		}
	}
	for _, f := range d.Changed {
		if (f.GmailMsgID != 0) != h.Gmail {
			t.Errorf("rebase Changed entry %+v: want an X-GM-MSGID exactly when Gmail is %v", f, h.Gmail)
		}
	}
}

func watchBaselineEmpty(t *testing.T, h WatchHarness, _ WatchRemote, pass passFunc) {
	d := pass(t, mailstore.SyncRequest{})
	checkState(t, h, d)
	if !d.Rebased || d.UIDs == nil || len(d.UIDs) != 0 || len(d.Changed) != 0 {
		t.Errorf("baseline of an empty folder: Rebased %v, UIDs %#v, Changed %v; want a rebase with an empty, non-nil UID set", d.Rebased, d.UIDs, d.Changed)
	}
}

func watchNewMail(t *testing.T, h WatchHarness, r WatchRemote, pass passFunc) {
	r.Append(t, h.Folder, watchMsg("n1"))
	d0 := pass(t, mailstore.SyncRequest{})
	u2 := r.Append(t, h.Folder, watchMsg("n2"))
	u3 := r.Append(t, h.Folder, watchMsg("n3"))
	d := pass(t, mailstore.SyncRequest{From: checkpoint(d0)})
	if d.Rebased || d.More {
		t.Errorf("Rebased %v, More %v; want neither", d.Rebased, d.More)
	}
	if got := newUIDs(d); !slices.Equal(got, []uint32{u2, u3}) {
		t.Fatalf("new UIDs = %v, want [%d %d]", got, u2, u3)
	}
	for i, s := range d.New {
		want := []string{"watch n2", "watch n3"}[i]
		if s.Subject != want || s.Ref.Folder != h.Folder || s.Ref.UIDValidity != d.State.UIDValidity {
			t.Errorf("new[%d] = %q in %s/%d, want %q in %s/%d", i, s.Subject, s.Ref.Folder, s.Ref.UIDValidity, want, h.Folder, d.State.UIDValidity)
		}
	}
	if again := pass(t, mailstore.SyncRequest{From: checkpoint(d)}); len(again.New) != 0 {
		t.Errorf("a pass from the new checkpoint found %v new, want none", newUIDs(again))
	}
}

// watchEmptyRange makes UIDNEXT grow with nothing at or above the old
// UIDNEXT: the server answers <old uidnext>:* with its highest UID, which
// the pass must drop.
func watchEmptyRange(t *testing.T, h WatchHarness, r WatchRemote, pass passFunc) {
	r.Append(t, h.Folder, watchMsg("e1"))
	d0 := pass(t, mailstore.SyncRequest{})
	u2 := r.Append(t, h.Folder, watchMsg("e2"))
	r.Expunge(t, h.Folder, u2)
	d := pass(t, mailstore.SyncRequest{From: checkpoint(d0)})
	if d.State.UIDNext <= d0.State.UIDNext {
		t.Fatalf("UIDNext %d did not grow past %d", d.State.UIDNext, d0.State.UIDNext)
	}
	if len(d.New) != 0 || d.More {
		t.Errorf("new UIDs = %v, More %v; want none: UIDs below the checkpoint's UIDNext are dropped", newUIDs(d), d.More)
	}
}

func watchBatching(t *testing.T, h WatchHarness, r WatchRemote, pass passFunc) {
	d0 := pass(t, mailstore.SyncRequest{})
	u1 := r.Append(t, h.Folder, watchMsg("t1"))
	u2 := r.Append(t, h.Folder, watchMsg("t2"))
	u3 := r.Append(t, h.Folder, watchMsg("t3"))
	d1 := pass(t, mailstore.SyncRequest{From: checkpoint(d0), Batch: 2})
	if got := newUIDs(d1); !slices.Equal(got, []uint32{u1, u2}) || !d1.More {
		t.Fatalf("first batch = %v, More %v; want [%d %d] and More", got, d1.More, u1, u2)
	}
	d2 := pass(t, mailstore.SyncRequest{From: checkpoint(d1), Batch: 2})
	if got := newUIDs(d2); !slices.Equal(got, []uint32{u3}) || d2.More {
		t.Errorf("second batch = %v, More %v; want [%d] and no More", got, d2.More, u3)
	}
}

func watchFlagChange(t *testing.T, h WatchHarness, r WatchRemote, pass passFunc) {
	u1 := r.Append(t, h.Folder, watchMsg("f1"))
	d0 := pass(t, mailstore.SyncRequest{})
	r.SetFlag(t, h.Folder, u1, `\Flagged`, true)
	d := pass(t, mailstore.SyncRequest{From: checkpoint(d0)})
	if fs, ok := changedOf(d, u1); !ok || !slices.Contains(fs.Flags, `\Flagged`) {
		t.Errorf("Changed = %+v, want UID %d with \\Flagged", d.Changed, u1)
	}
	if len(d.New) != 0 {
		t.Errorf("a flag change gave new messages %v", newUIDs(d))
	}
	if h.CondStore && d.FullFlags {
		t.Error("FullFlags set on a CONDSTORE folder")
	}
	if !h.CondStore && (!d.FullFlags || !slices.Equal(d.UIDs, []uint32{u1})) {
		t.Errorf("without CONDSTORE: FullFlags %v, UIDs %v; want the full set [%d]", d.FullFlags, d.UIDs, u1)
	}
	r.SetFlag(t, h.Folder, u1, `\Flagged`, false)
	d2 := pass(t, mailstore.SyncRequest{From: checkpoint(d)})
	if fs, ok := changedOf(d2, u1); !ok || slices.Contains(fs.Flags, `\Flagged`) {
		t.Errorf("after clearing: Changed = %+v, want UID %d without \\Flagged", d2.Changed, u1)
	}
}

func watchSweep(t *testing.T, h WatchHarness, r WatchRemote, pass passFunc) {
	u1 := r.Append(t, h.Folder, watchMsg("s1"))
	u2 := r.Append(t, h.Folder, watchMsg("s2"))
	d0 := pass(t, mailstore.SyncRequest{})
	r.Expunge(t, h.Folder, u1)
	plain := pass(t, mailstore.SyncRequest{From: checkpoint(d0)})
	if h.CondStore && plain.UIDs != nil {
		t.Errorf("a CONDSTORE pass without Sweep gave UIDs %v, want nil", plain.UIDs)
	}
	if !h.CondStore && !slices.Equal(plain.UIDs, []uint32{u2}) {
		t.Errorf("a pass without CONDSTORE gave UIDs %v, want [%d]", plain.UIDs, u2)
	}
	d := pass(t, mailstore.SyncRequest{From: checkpoint(d0), Sweep: true})
	if d.Rebased || !slices.Equal(d.UIDs, []uint32{u2}) {
		t.Errorf("sweep: Rebased %v, UIDs %v; want [%d] without the expunged %d", d.Rebased, d.UIDs, u2, u1)
	}
}

// watchSweepEmptied expunges the folder's only message: a sweep must report
// an empty UID set, not a nil one, or the engine could not tell "nothing
// left" from "not fetched".
func watchSweepEmptied(t *testing.T, h WatchHarness, r WatchRemote, pass passFunc) {
	u1 := r.Append(t, h.Folder, watchMsg("z1"))
	d0 := pass(t, mailstore.SyncRequest{})
	r.Expunge(t, h.Folder, u1)
	d := pass(t, mailstore.SyncRequest{From: checkpoint(d0), Sweep: true})
	if d.Rebased || d.UIDs == nil || len(d.UIDs) != 0 {
		t.Errorf("sweep of an emptied folder: Rebased %v, UIDs %#v; want an empty, non-nil UID set", d.Rebased, d.UIDs)
	}
}

func watchUIDValidity(t *testing.T, h WatchHarness, r WatchRemote, pass passFunc) {
	r.Append(t, h.Folder, watchMsg("v1"))
	d0 := pass(t, mailstore.SyncRequest{})
	r.NewUIDValidity(t, h.Folder)
	d := pass(t, mailstore.SyncRequest{From: checkpoint(d0)})
	if !d.Rebased || d.State.UIDValidity == d0.State.UIDValidity {
		t.Errorf("after a UIDVALIDITY change: Rebased %v, UIDVALIDITY %d (was %d); want a rebase under a new one", d.Rebased, d.State.UIDValidity, d0.State.UIDValidity)
	}
	if len(d.UIDs) != 1 || len(d.New) != 0 {
		t.Errorf("after a UIDVALIDITY change: UIDs %v, %d new; want one UID and nothing new", d.UIDs, len(d.New))
	}
	// A renumbered folder may have no mod-sequences yet (NOMODSEQ), so the
	// pass may report no CONDSTORE where the harness has it, never the
	// reverse, and a HIGHESTMODSEQ exactly when it reports CONDSTORE.
	if st := d.State; (st.CondStore && !h.CondStore) || st.CondStore != (st.HighestModSeq != 0) {
		t.Errorf("after a UIDVALIDITY change: State = %+v; want CONDSTORE only where the server has it (%v), with a HIGHESTMODSEQ exactly when CONDSTORE", st, h.CondStore)
	}
	checkRebaseChanged(t, h, d)
}

func watchNeverSeen(t *testing.T, h WatchHarness, r WatchRemote, pass passFunc) {
	d0 := pass(t, mailstore.SyncRequest{})
	u1 := r.Append(t, h.Folder, watchMsg("u1"))
	d := pass(t, mailstore.SyncRequest{From: checkpoint(d0), Snippets: true})
	if len(d.New) != 1 {
		t.Fatalf("new = %v, want [%d]", newUIDs(d), u1)
	}
	if s := d.New[0]; s.Preview == nil && s.Sample == nil {
		t.Error("Snippets gave neither a preview nor a text sample")
	}
	pass(t, mailstore.SyncRequest{From: checkpoint(d), Sweep: true})
	if flags := r.Flags(t, h.Folder, u1); slices.Contains(flags, `\Seen`) {
		t.Errorf("flags after sync passes = %v; a read must never set \\Seen", flags)
	}
}

func watchMissingFolder(t *testing.T, h WatchHarness) {
	w, _ := h.New(t)
	wantMissing := func(what string, err error) {
		t.Helper()
		var nf *mailstore.FolderNotFoundError
		if !errors.As(err, &nf) || nf.Requested != missingWatchFolder {
			t.Errorf("%s = %v, want a FolderNotFoundError for %q", what, err, missingWatchFolder)
		}
	}
	wc, err := w.OpenWatch(t.Context(), missingWatchFolder)
	if err == nil {
		_ = wc.Close() // only the error matters
	}
	wantMissing("OpenWatch", err)
	_, err = w.SyncFolder(t.Context(), missingWatchFolder, mailstore.SyncRequest{})
	wantMissing("SyncFolder", err)
	_, err = w.FolderState(t.Context(), missingWatchFolder)
	wantMissing("FolderState", err)
}

func watchFolderState(t *testing.T, h WatchHarness) {
	w, r := h.New(t)
	r.Append(t, h.Folder, watchMsg("st1"))
	st, err := w.FolderState(t.Context(), h.Folder)
	if err != nil {
		t.Fatal(err)
	}
	d, err := w.SyncFolder(t.Context(), h.Folder, mailstore.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st != d.State {
		t.Errorf("FolderState = %+v, want the SELECT state %+v", st, d.State)
	}
	checkState(t, h, d)
}

// idleResult is what an Idle running on another goroutine returned.
type idleResult struct {
	wake mailstore.Wake
	err  error
}

// idleAsync runs Idle on its own goroutine, which t joins before it ends.
func idleAsync(t *testing.T, wc mailstore.WatchConn, limit time.Duration) <-chan idleResult {
	t.Helper()
	done := make(chan idleResult, 1)
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait) // t.Context ends first, which ends the Idle
	wg.Go(func() {
		wake, err := wc.Idle(t.Context(), limit)
		done <- idleResult{wake: wake, err: err}
	})
	return done
}

// waitWake waits up to 2 seconds for an Idle to return.
func waitWake(t *testing.T, done <-chan idleResult, what string) idleResult {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Idle after %s = %v", what, res.err)
		}
		return res
	case <-timer.C:
		t.Fatalf("Idle did not return within 2s of %s", what)
		return idleResult{}
	}
}

func watchIdleAppend(t *testing.T, h WatchHarness) {
	w, r := h.New(t)
	wc := openWatch(t, w, h.Folder)
	d0, err := wc.Sync(t.Context(), mailstore.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	done := idleAsync(t, wc, time.Minute)
	u1 := r.Append(t, h.Folder, watchMsg("i1"))
	if res := waitWake(t, done, "an append"); !res.wake.Changed || res.wake.Expunged {
		t.Errorf("Idle after an append = %+v, want Changed only", res.wake)
	}
	d, err := wc.Sync(t.Context(), mailstore.SyncRequest{From: checkpoint(d0)})
	if err != nil {
		t.Fatal(err)
	}
	if got := newUIDs(d); !slices.Equal(got, []uint32{u1}) {
		t.Errorf("Sync after the wake found new %v, want [%d]", got, u1)
	}
}

func watchIdleExpunge(t *testing.T, h WatchHarness) {
	w, r := h.New(t)
	u1 := r.Append(t, h.Folder, watchMsg("x1"))
	wc := openWatch(t, w, h.Folder)
	if _, err := wc.Sync(t.Context(), mailstore.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	done := idleAsync(t, wc, time.Minute)
	r.Expunge(t, h.Folder, u1)
	if res := waitWake(t, done, "an expunge"); !res.wake.Changed || !res.wake.Expunged {
		t.Errorf("Idle after an expunge = %+v, want Changed and Expunged", res.wake)
	}
}

func watchIdleMax(t *testing.T, h WatchHarness) {
	w, r := h.New(t)
	r.Append(t, h.Folder, watchMsg("m1"))
	wc := openWatch(t, w, h.Folder)
	d0, err := wc.Sync(t.Context(), mailstore.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	const limit = 50 * time.Millisecond
	start := time.Now()
	wake, err := wc.Idle(t.Context(), limit)
	if elapsed := time.Since(start); err != nil || wake != (mailstore.Wake{}) || elapsed < limit || elapsed > 2*time.Second {
		t.Errorf("Idle(%v) = %+v, %v after %v; want Wake{} and nil after about %v", limit, wake, err, elapsed, limit)
	}
	if _, err := wc.Sync(t.Context(), mailstore.SyncRequest{From: checkpoint(d0)}); err != nil {
		t.Errorf("Sync after an Idle that ran out = %v; the connection must stay usable", err)
	}
}

func watchIdleCancel(t *testing.T, h WatchHarness) {
	w, _ := h.New(t)
	wc := openWatch(t, w, h.Folder)
	if _, err := wc.Sync(t.Context(), mailstore.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := wc.Idle(ctx, time.Minute)
	if elapsed := time.Since(start); !errors.Is(err, context.DeadlineExceeded) || elapsed > 2*time.Second {
		t.Errorf("Idle with a 30ms context = %v after %v, want context.DeadlineExceeded at once", err, elapsed)
	}
}

// watchSyncAfterCancel ends an Idle through its context and syncs on the
// same connection: a cancelled Idle ends IDLE with DONE and leaves the
// folder selected, so the watcher need not dial again.
func watchSyncAfterCancel(t *testing.T, h WatchHarness) {
	w, r := h.New(t)
	wc := openWatch(t, w, h.Folder)
	d0, err := wc.Sync(t.Context(), mailstore.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := wc.Idle(ctx, time.Minute); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Idle with a 30ms context = %v, want context.DeadlineExceeded", err)
	}
	u1 := r.Append(t, h.Folder, watchMsg("c1"))
	d, err := wc.Sync(t.Context(), mailstore.SyncRequest{From: checkpoint(d0)})
	if err != nil {
		t.Fatalf("Sync after a cancelled Idle = %v; the connection must stay usable", err)
	}
	if got := newUIDs(d); !slices.Equal(got, []uint32{u1}) {
		t.Errorf("Sync after a cancelled Idle found new %v, want [%d]", got, u1)
	}
}
