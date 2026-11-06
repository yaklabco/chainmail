package cache

import "github.com/yaklabco/chainmail/internal/ref"

// readGuard records the flags and expunged changes the builder applies to
// the account while the read path downloads messages or flags with wmu
// released. The epoch catches a folder drop in that window; a guard catches
// a change to one message, which a write of the download would undo: an
// expunged message would come back as a ghost row, a newer flags change
// would be overwritten by older flags, and no later event would repair
// either. Its changed set is written by the builder and read by the read
// path, both under wmu and Account.readsMu.
type readGuard struct {
	changed map[guardKey]bool
}

// guardKey names a message within the account, without the X-GM-MSGID a
// ref may or may not carry.
type guardKey struct {
	folder      string
	uidValidity uint32
	uid         uint32
}

func guardKeyOf(r ref.Ref) guardKey {
	return guardKey{folder: r.Folder, uidValidity: r.UIDValidity, uid: r.UID}
}

// guardReads starts a read guard. The caller starts it before its server
// call and ends it with endGuard once its writes are done.
func (a *Account) guardReads() *readGuard {
	g := &readGuard{changed: map[guardKey]bool{}}
	a.readsMu.Lock()
	defer a.readsMu.Unlock()
	if a.reads == nil {
		a.reads = map[*readGuard]struct{}{}
	}
	a.reads[g] = struct{}{}
	return g
}

// endGuard ends g, which notes no more changes.
func (a *Account) endGuard(g *readGuard) {
	a.readsMu.Lock()
	defer a.readsMu.Unlock()
	delete(a.reads, g)
}

// noteChange records that the builder applied a flags or expunged change to
// r: in the backfill's in-flight batch and in every running read guard.
// The caller holds wmu.
func (a *Account) noteChange(r ref.Ref) {
	a.inflight.note(r)
	a.readsMu.Lock()
	defer a.readsMu.Unlock()
	k := guardKeyOf(r)
	for g := range a.reads {
		g.changed[k] = true
	}
}

// changedSince reports whether the builder applied a change to r since g
// started. The caller holds wmu, so no change is applied meanwhile.
func (a *Account) changedSince(g *readGuard, r ref.Ref) bool {
	a.readsMu.Lock()
	defer a.readsMu.Unlock()
	return g.changed[guardKeyOf(r)]
}
