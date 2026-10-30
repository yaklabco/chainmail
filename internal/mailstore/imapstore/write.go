package imapstore

// Writer methods of the IMAP store, with the standard IMAP semantics of spec
// 7.1. Google accounts take the gmail hooks in gmail_write.go at the top of
// each method whose semantics differ there. Folder changes are in
// folders_write.go.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// Append implements mailstore.Writer. Without APPENDUID, from a server
// lacking UIDPLUS, the message is found again by its Message-ID among the
// messages newer than the folder's UIDNEXT read before the APPEND. One
// without a Message-ID, or one the search cannot single out, gives a zero
// ref.
func (s *Store) Append(ctx context.Context, folder string, flags []string, date time.Time, raw []byte) (ref.Ref, error) {
	fl, err := imapFlags(flags)
	if err != nil {
		return ref.Ref{}, err
	}
	id := messageIDOf(raw)
	var out ref.Ref
	err = s.writeConn(ctx, "", false, func(c *conn, sent func()) error {
		out = ref.Ref{}
		var before boundary
		if id != "" && !c.client().Caps().Has(imap.CapUIDPlus) {
			// No APPENDUID will come. Read before the change, so a failure
			// here is retried as reads are.
			var err error
			if before, err = c.boundary(ctx, folder); err != nil {
				return err
			}
		}
		sent()
		data, err := c.append(ctx, folder, fl, date, raw)
		if err != nil {
			return s.destError(err, folder)
		}
		if data.UID != 0 && data.UIDValidity != 0 {
			out = ref.Ref{Account: s.acct.Name, Folder: folder, UIDValidity: data.UIDValidity, UID: uint32(data.UID)}
			return nil
		}
		out = s.findAfterWrite(ctx, c, folder, id, before)
		return nil
	})
	return out, err
}

// Move implements mailstore.Writer.
func (s *Store) Move(ctx context.Context, refs []ref.Ref, folder string) []mailstore.RefResult {
	if s.gmail() {
		return s.gmailMove(ctx, refs, folder)
	}
	return s.transfer(ctx, refs, folder, false)
}

// Copy implements mailstore.Writer.
func (s *Store) Copy(ctx context.Context, refs []ref.Ref, folder string) []mailstore.RefResult {
	if s.gmail() {
		return s.gmailCopy(ctx, refs, folder)
	}
	return s.transfer(ctx, refs, folder, true)
}

// Archive implements mailstore.Writer. The folder is Options.ArchiveFolder,
// else the account's archive_folder, else the one with the \Archive role.
func (s *Store) Archive(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	if s.gmail() {
		return s.gmailArchive(ctx, refs)
	}
	folder := cmp.Or(s.opts.ArchiveFolder, s.acct.ArchiveFolder)
	if folder == "" {
		var err error
		if folder, err = s.roleFolder(ctx, mailstore.RoleArchive); err != nil {
			return failAll(refs, err)
		}
	}
	return s.transfer(ctx, refs, folder, false)
}

// Trash implements mailstore.Writer. A ref already in Trash succeeds unchanged.
func (s *Store) Trash(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	if s.gmail() {
		return s.gmailTrash(ctx, refs)
	}
	folder, err := s.roleFolder(ctx, mailstore.RoleTrash)
	if err != nil {
		return failAll(refs, err)
	}
	return s.transfer(ctx, refs, folder, false)
}

// Expunge implements mailstore.Writer: UID STORE +FLAGS.SILENT (\Deleted)
// and UID EXPUNGE of the same UIDs, pipelined. Without UIDPLUS there is no
// UID EXPUNGE, and a plain EXPUNGE would also remove other mail marked
// \Deleted, so every ref fails with ErrUnsupported.
func (s *Store) Expunge(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	if s.gmail() {
		return s.gmailExpunge(ctx, refs)
	}
	return s.writeBatch(ctx, refs, expungeGroup)
}

// expungeGroup is Expunge's writeOp: it expunges a group's messages from
// the folder they are in. Gmail's Expunge uses it in Trash.
func expungeGroup(ctx context.Context, c *conn, g *groupWrite, sent func()) error {
	if !c.client().Caps().Has(imap.CapUIDPlus) {
		g.failAll(fmt.Errorf("%w: the server has no UID EXPUNGE (UIDPLUS), so expunging would remove other deleted mail too", mailstore.ErrUnsupported))
		return nil
	}
	set := uidSet(g.uids)
	var found []uint32
	sent()
	err := c.pipelined(ctx, func(cl *imapclient.Client) []func() error {
		exists := cl.Fetch(set, &imap.FetchOptions{UID: true})
		store := cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil)
		expunge := cl.UIDExpunge(set)
		return []func() error{collectUIDs(exists, &found), store.Close, expunge.Close}
	})
	if err != nil {
		return err
	}
	for _, uid := range found {
		g.set(uid, mailstore.RefResult{})
	}
	return nil
}

// StoreFlags implements mailstore.Writer: UID STORE +FLAGS.SILENT and
// -FLAGS.SILENT, pipelined, after a UID FETCH that learns which messages
// still exist. Google accounts share it: there \Flagged is the star.
func (s *Store) StoreFlags(ctx context.Context, refs []ref.Ref, add, remove []string) []mailstore.RefResult {
	addFlags, err := imapFlags(add)
	if err != nil {
		return failAll(refs, err)
	}
	removeFlags, err := imapFlags(remove)
	if err != nil {
		return failAll(refs, err)
	}
	return s.writeBatch(ctx, refs, func(ctx context.Context, c *conn, g *groupWrite, sent func()) error {
		set := uidSet(g.uids)
		var found []uint32
		sent()
		err := c.pipelined(ctx, func(cl *imapclient.Client) []func() error {
			waits := []func() error{collectUIDs(cl.Fetch(set, &imap.FetchOptions{UID: true}), &found)}
			if len(addFlags) > 0 {
				waits = append(waits, cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: addFlags}, nil).Close)
			}
			if len(removeFlags) > 0 {
				waits = append(waits, cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsDel, Silent: true, Flags: removeFlags}, nil).Close)
			}
			return waits
		})
		if err != nil {
			return err
		}
		for _, uid := range found {
			g.keep(uid)
		}
		return nil
	})
}

// StoreLabels implements mailstore.Writer. Labels exist only on Google accounts.
func (s *Store) StoreLabels(ctx context.Context, refs []ref.Ref, add, remove []string) []mailstore.RefResult {
	if s.gmail() {
		return s.gmailStoreLabels(ctx, refs, add, remove)
	}
	return failAll(refs, fmt.Errorf("%w: labels are a Gmail feature", mailstore.ErrUnsupported))
}

// FindByMessageID implements mailstore.Writer. It is a read: the folder is
// opened read-only, and the refs come in ascending UID order.
func (s *Store) FindByMessageID(ctx context.Context, folder, messageID string) ([]ref.Ref, error) {
	id := bareMessageID(messageID)
	if id == "" {
		return nil, nil
	}
	var out []ref.Ref
	err := s.withConn(ctx, folder, func(c *conn) error {
		var err error
		out, err = s.findByID(ctx, c, folder, id)
		return err
	})
	return out, err
}

// findByID searches folder, opened on c, for messages whose Message-ID is
// id, given without angle brackets.
func (s *Store) findByID(ctx context.Context, c *conn, folder, id string) ([]ref.Ref, error) {
	if err := s.open(ctx, c, folder); err != nil {
		return nil, err
	}
	// HEADER matches a substring; with the brackets that is the whole ID.
	criteria := &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: "<" + id + ">"}}}
	uids, _, err := c.search(ctx, criteria, false)
	if err != nil {
		return nil, err
	}
	slices.Sort(uids)
	out := make([]ref.Ref, len(uids))
	for i, uid := range uids {
		out[i] = ref.Ref{Account: s.acct.Name, Folder: folder, UIDValidity: c.uidValidity, UID: uid}
	}
	return out, nil
}

// boundary is a folder's UIDVALIDITY and UIDNEXT, read before a write: a
// message the write makes there gets a UID of at least next, under the same
// UIDVALIDITY. The zero boundary is unknown, and then nothing found
// afterwards can be shown to be new.
type boundary struct{ validity, next uint32 }

// isNew reports whether r was made after b was read.
func (b boundary) isNew(r ref.Ref) bool {
	return b.next != 0 && r.UIDValidity == b.validity && r.UID >= b.next
}

// findNew searches folder, opened on c, for the messages with Message-ID id
// made after before was read. Older messages with that ID, which a search
// also matches, are left out.
func (s *Store) findNew(ctx context.Context, c *conn, folder, id string, before boundary) ([]ref.Ref, error) {
	if id == "" || before.next == 0 {
		return nil, nil
	}
	refs, err := s.findByID(ctx, c, folder, id)
	return slices.DeleteFunc(refs, func(r ref.Ref) bool { return !before.isNew(r) }), err
}

// findAfterWrite finds, after an APPEND the server answered without the new
// UID, the message with Message-ID id made in folder since before. It gives
// a zero ref when there is no ID or boundary, when the search fails, and
// unless exactly one new message matches: with several, which one is this
// APPEND's is unknown. The write itself happened either way, so a failure
// here is not reported as its failure.
func (s *Store) findAfterWrite(ctx context.Context, c *conn, folder, id string, before boundary) ref.Ref {
	refs, err := s.findNew(ctx, c, folder, id, before)
	if err != nil {
		s.opts.Logger.Debug("finding a written message by Message-ID", "account", s.acct.Name, "folder", folder, "err", err)
		return ref.Ref{}
	}
	if len(refs) != 1 {
		return ref.Ref{}
	}
	return refs[0]
}

// findCopies finds in dest, by Message-ID, the copies of the source
// messages ids names, after a COPY or MOVE the server acknowledged without
// COPYUID. It only reports new places: it never confirms a copy, so nothing
// may be deleted on its word. A source gets a ref only when nothing else
// could be its copy: it has a Message-ID that no other source in ids
// shares, dest holds exactly one message with it made since before, and no
// other source matched that message. Any other source is left out of the
// map, since its new place is unknown.
func (s *Store) findCopies(ctx context.Context, c *conn, dest string, ids map[uint32]string, before boundary) map[uint32]ref.Ref {
	sources := map[string]int{}
	for _, id := range ids {
		sources[strings.ToLower(id)]++ // servers match header values case-insensitively
	}
	out := map[uint32]ref.Ref{}
	claims := map[uint32]int{} // sources matching each new UID
	for _, uid := range slices.Sorted(maps.Keys(ids)) {
		id := ids[uid]
		if id == "" || sources[strings.ToLower(id)] > 1 {
			continue
		}
		refs, err := s.findNew(ctx, c, dest, id, before)
		if err != nil {
			s.opts.Logger.Debug("finding copied messages by Message-ID", "account", s.acct.Name, "folder", dest, "err", err)
			break
		}
		if len(refs) == 1 {
			out[uid] = refs[0]
			claims[refs[0].UID]++
		}
	}
	maps.DeleteFunc(out, func(_ uint32, r ref.Ref) bool { return claims[r.UID] > 1 })
	return out
}

// errCopyUnconfirmed is the outcome of a move done by COPY that the server
// acknowledged without a readable COPYUID: the copy may exist, but nothing
// names it, so the original stays.
var errCopyUnconfirmed = fmt.Errorf("%w: copied, but the original was kept: the server did not confirm the copy", mailstore.ErrUnavailable)

// transfer moves, or with keepOriginal copies, refs into dest. Refs already
// in dest are left as they are by a move. New refs come from COPYUID, or,
// after a MOVE or COPY the server acknowledged without one, from a search
// of dest by Message-ID among the messages made since the transfer began.
func (s *Store) transfer(ctx context.Context, refs []ref.Ref, dest string, keepOriginal bool) []mailstore.RefResult {
	if dest == "" {
		return failAll(refs, fmt.Errorf("%w: no folder named", mailstore.ErrNotFound))
	}
	return s.writeBatch(ctx, refs, func(ctx context.Context, c *conn, g *groupWrite, sent func()) error {
		if !keepOriginal && c.Selected() == dest {
			for _, uid := range g.uids {
				g.keep(uid)
			}
			return nil
		}
		caps := c.client().Caps()
		uidplus := caps.Has(imap.CapUIDPlus)
		move := !keepOriginal && caps.Has(imap.CapMove)
		switch {
		case !keepOriginal && !move && !uidplus:
			g.failAll(fmt.Errorf("%w: the server has neither MOVE nor UID EXPUNGE (UIDPLUS)", mailstore.ErrUnsupported))
			return nil
		case !keepOriginal && !move:
			return s.moveByCopy(ctx, c, g, dest, sent)
		}
		t, err := s.sendTransfer(ctx, c, g.uids, dest, move, uidplus, sent)
		if err != nil {
			return s.destError(err, dest)
		}
		// From here on the server has acknowledged the MOVE or COPY, so each
		// message it named is a success, whatever happens to the reads that
		// look for the new places.
		if t.cu.validity != 0 {
			pairs, ok := t.cu.pairs(len(g.uids))
			if !ok {
				// The COPYUID cannot be read: the new places are unknown.
				g.setAll(mailstore.RefResult{})
				return nil
			}
			s.setPlaced(g, dest, t.cu.validity, pairs)
			return nil
		}
		// No COPYUID: there never is one without UIDPLUS, and a UIDPLUS
		// server leaves it out when it copied nothing, or, against the RFC,
		// always. The Message-IDs say which messages there were and how to
		// find their copies.
		ids := t.ids
		if ids == nil && !move {
			// A COPY leaves the originals, so they can still be read.
			if ids, err = c.messageIDs(ctx, uidSet(g.uids)); err != nil {
				s.opts.Logger.Debug("reading the Message-IDs of copied messages", "account", s.acct.Name, "err", err)
				ids = nil
			}
		}
		if ids == nil {
			// The Message-IDs could not be read: the new places are unknown.
			g.setAll(mailstore.RefResult{})
			return nil
		}
		maps.DeleteFunc(ids, func(uid uint32, _ string) bool { return g.byUID[uid] == nil })
		found := s.findCopies(ctx, c, dest, ids, t.before)
		for uid := range ids {
			g.set(uid, mailstore.RefResult{Ref: found[uid]})
		}
		return nil
	})
}

// moveByCopy moves g's messages into dest on a server with UIDPLUS but no
// MOVE: UID COPY, then UID STORE \Deleted and UID EXPUNGE of exactly the
// originals the COPY's COPYUID names, so a failed COPY never loses mail.
// Without a readable COPYUID nothing names the copies, so nothing is
// deleted and every message fails with errCopyUnconfirmed.
func (s *Store) moveByCopy(ctx context.Context, c *conn, g *groupWrite, dest string, sent func()) error {
	sent()
	cu, err := c.uidCopy(ctx, uidSet(g.uids), dest)
	if err != nil {
		return s.destError(err, dest)
	}
	pairs, ok := cu.pairs(len(g.uids))
	if cu.validity == 0 || !ok {
		g.failAll(errCopyUnconfirmed)
		return nil
	}
	copied := slices.Sorted(maps.Keys(pairs))
	copied = slices.DeleteFunc(copied, func(u uint32) bool { return g.byUID[u] == nil })
	if len(copied) > 0 {
		if err := c.expungeUIDs(ctx, uidSet(copied)); err != nil {
			return err
		}
	}
	s.setPlaced(g, dest, cu.validity, pairs)
	return nil
}

// setPlaced records the new refs a COPYUID's pairs give in dest.
func (s *Store) setPlaced(g *groupWrite, dest string, validity uint32, pairs map[uint32]uint32) {
	for src, dst := range pairs {
		g.set(src, mailstore.RefResult{Ref: ref.Ref{Account: s.acct.Name, Folder: dest, UIDValidity: validity, UID: dst}})
	}
}

// sentTransfer is what sendTransfer learned: the COPYUID, when the server
// sent one, and for finding the copies without it, the Message-IDs of the
// messages that existed and dest's boundary, both read before the change.
type sentTransfer struct {
	cu     copyUID
	ids    map[uint32]string
	before boundary
}

// sendTransfer sends the UID MOVE, when move, or else the UID COPY, of uids
// into dest, calling sent first. Pipelined ahead of it, it reads dest's
// boundary, and the Message-IDs, by UID, of the messages that exist in the
// two cases where they cannot be read after the change: without UIDPLUS,
// where no COPYUID will come, and before a MOVE, which takes the originals
// away. Elsewhere, or when the server refuses that read, ids is nil.
func (s *Store) sendTransfer(ctx context.Context, c *conn, uids []uint32, dest string, move, uidplus bool, sent func()) (sentTransfer, error) {
	set := uidSet(uids)
	var t sentTransfer
	var err error
	switch {
	case !uidplus:
		if t.ids, t.before, err = c.messageIDsBefore(ctx, set, dest); err != nil || len(t.ids) == 0 {
			return t, err
		}
		set = uidSet(slices.Sorted(maps.Keys(t.ids)))
		sent()
		if move {
			t.cu, err = c.uidMove(ctx, set, dest)
		} else {
			t.cu, err = c.uidCopy(ctx, set, dest)
		}
		return t, err
	case move:
		sent()
		return c.moveWithIDs(ctx, set, dest)
	default:
		sent()
		return c.copyWithBoundary(ctx, set, dest)
	}
}

// destError reports a write the server refused because dest is missing as
// ErrNotFound, and drops the cached folder list.
func (s *Store) destError(err error, dest string) error {
	if missingFolder(err) {
		s.forgetFolders()
		return fmt.Errorf("%w: no folder %q", mailstore.ErrNotFound, dest)
	}
	return err
}

// roleFolder returns the folder with role, from the cached folder list.
func (s *Store) roleFolder(ctx context.Context, role mailstore.Role) (string, error) {
	folders, err := s.folderList(ctx, false)
	if err != nil {
		return "", err
	}
	if name := byRole(folders, role); name != "" {
		return name, nil
	}
	if role == mailstore.RoleArchive {
		return "", fmt.Errorf("%w: no folder has the archive role; set archive_folder for the account", mailstore.ErrNotFound)
	}
	return "", fmt.Errorf("%w: no folder has the %s role", mailstore.ErrNotFound, role)
}

// writeConn runs f on a pooled connection, preferring one with folder open,
// read-write when writable. A failure before the change went out, such as a
// dial or a SELECT on a connection that had dropped, is retried as reads
// are. f calls sent just before its first command that changes anything;
// from then on a lost connection is final, since the change may or may not
// have happened, and is reported as ErrUnavailable.
func (s *Store) writeConn(ctx context.Context, folder string, writable bool, f func(c *conn, sent func()) error) error {
	return s.retry(ctx, func() error {
		sent := false
		err := s.tryConnMode(ctx, folder, writable, func(c *conn) error { return f(c, func() { sent = true }) })
		if sent && errors.Is(err, errConnLost) {
			s.opts.Logger.Debug("connection lost during a write", "account", s.acct.Name, "err", err)
			return fmt.Errorf("%w: %s: the connection broke during the change, which may or may not have happened; search again before retrying", mailstore.ErrUnavailable, s.acct.Name)
		}
		return err
	})
}

// writeOp changes the messages of one folder group on c, which has the
// group's folder open read-write. It calls sent just before its first
// command that changes anything, and records each UID's outcome in g. A UID
// left without an outcome is reported gone.
type writeOp func(ctx context.Context, c *conn, g *groupWrite, sent func()) error

// writeBatch groups refs by folder and runs op once per group, the groups
// concurrently within the pool's size. Each group opens its folder
// read-write and checks every ref's UIDVALIDITY first: a mismatch fails that
// ref alone with ErrStaleRef.
func (s *Store) writeBatch(ctx context.Context, refs []ref.Ref, op writeOp) []mailstore.RefResult {
	out := make([]mailstore.RefResult, len(refs))
	groups := s.groupRefs(refs, out)
	s.fanOut(len(groups), func(k int) {
		grp := groups[k]
		g := newGroupWrite(refs, grp.idx)
		err := s.writeConn(ctx, grp.folder, true, func(c *conn, sent func()) error {
			g.reset()
			if err := s.openMode(ctx, c, grp.folder, true); err != nil {
				return err
			}
			if err := s.resolveGroup(ctx, c, g); err != nil {
				return err
			}
			if len(g.uids) == 0 {
				return nil
			}
			return op(ctx, c, g, sent)
		})
		g.finish(err)
		for j, i := range grp.idx {
			out[i] = g.res[j]
		}
	})
	return out
}

// folderGroup is the refs of a batch that name one folder.
type folderGroup struct {
	folder string
	idx    []int // positions in the batch
}

// groupRefs groups refs by folder, in order of first appearance. A ref of
// another account fails in out at once.
func (s *Store) groupRefs(refs []ref.Ref, out []mailstore.RefResult) []folderGroup {
	pos := map[string]int{}
	var groups []folderGroup
	for i, r := range refs {
		if r.Account != s.acct.Name {
			out[i].Err = fmt.Errorf("%w: the ref is for account %s", mailstore.ErrNotFound, r.Account)
			continue
		}
		k, ok := pos[r.Folder]
		if !ok {
			k = len(groups)
			pos[r.Folder] = k
			groups = append(groups, folderGroup{folder: r.Folder})
		}
		groups[k].idx = append(groups[k].idx, i)
	}
	return groups
}

// fanOut runs f(0) to f(n-1), at most PoolSize at once, and returns once
// every one has.
func (s *Store) fanOut(n int, f func(int)) {
	if n == 1 {
		f(0)
		return
	}
	sem := make(chan struct{}, max(1, s.PoolSize()))
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			f(i)
		})
	}
	wg.Wait()
}

// resolveGroup checks each ref of g against the folder open on c and
// collects the UIDs to change. A stale ref fails on its own.
func (s *Store) resolveGroup(ctx context.Context, c *conn, g *groupWrite) error {
	g.validity = c.uidValidity
	for j, r := range g.refs {
		uid, err := s.resolveUID(ctx, c, r)
		switch {
		case errors.Is(err, mailstore.ErrStaleRef):
			g.setAt(j, mailstore.RefResult{Err: err})
			continue
		case err != nil:
			return err
		}
		if g.byUID[uid] == nil {
			g.uids = append(g.uids, uid)
		}
		g.byUID[uid] = append(g.byUID[uid], j)
	}
	slices.Sort(g.uids)
	return nil
}

// groupWrite is one folder group's write in progress: the UIDs it changes
// and the outcome of each of its refs.
type groupWrite struct {
	refs     []ref.Ref
	res      []mailstore.RefResult // parallel to refs
	done     []bool                // which of res are set
	byUID    map[uint32][]int      // positions in refs naming each UID; a batch may name one message twice
	uids     []uint32              // distinct, ascending
	validity uint32                // the folder's UIDVALIDITY
}

func newGroupWrite(batch []ref.Ref, idx []int) *groupWrite {
	g := &groupWrite{refs: make([]ref.Ref, len(idx)), res: make([]mailstore.RefResult, len(idx)), done: make([]bool, len(idx))}
	for j, i := range idx {
		g.refs[j] = batch[i]
	}
	g.reset()
	return g
}

// reset forgets the outcome of an attempt that is about to be retried.
func (g *groupWrite) reset() {
	clear(g.res)
	clear(g.done)
	g.byUID, g.uids, g.validity = map[uint32][]int{}, nil, 0
}

func (g *groupWrite) setAt(j int, r mailstore.RefResult) {
	g.res[j], g.done[j] = r, true
}

// set records r for every ref naming uid. A UID the group did not ask for,
// which a server might report, is ignored.
func (g *groupWrite) set(uid uint32, r mailstore.RefResult) {
	for _, j := range g.byUID[uid] {
		g.setAt(j, r)
	}
}

// keep records success with the message where it was: the ref as it stands
// now, which on Google accounts may carry a UIDVALIDITY recovered by
// X-GM-MSGID.
func (g *groupWrite) keep(uid uint32) {
	for _, j := range g.byUID[uid] {
		r := g.refs[j]
		r.UIDValidity, r.UID = g.validity, uid
		g.setAt(j, mailstore.RefResult{Ref: r})
	}
}

// setAll records r for every UID of the group.
func (g *groupWrite) setAll(r mailstore.RefResult) {
	for _, uid := range g.uids {
		g.set(uid, r)
	}
}

// place records that the message uid names is now at r, keeping each ref's
// X-GM-MSGID, which stays with a Gmail message wherever it goes.
func (g *groupWrite) place(uid uint32, r ref.Ref) {
	for _, j := range g.byUID[uid] {
		out := r
		out.GmailMsgID = g.refs[j].GmailMsgID
		g.setAt(j, mailstore.RefResult{Ref: out})
	}
}

// failAll fails every ref that has no outcome yet.
func (g *groupWrite) failAll(err error) {
	for j := range g.res {
		if !g.done[j] {
			g.setAt(j, mailstore.RefResult{Err: err})
		}
	}
}

// finish gives every ref still without an outcome err, or, when the group
// went through, reports it gone.
func (g *groupWrite) finish(err error) {
	if err == nil {
		err = fmt.Errorf("%w: the message is gone; search again", mailstore.ErrStaleRef)
	}
	g.failAll(err)
}

// failAll fails every ref with err.
func failAll(refs []ref.Ref, err error) []mailstore.RefResult {
	out := make([]mailstore.RefResult, len(refs))
	for i := range out {
		out[i].Err = err
	}
	return out
}

// copyUID is a COPYUID response code: the destination's UIDVALIDITY and the
// source UIDs with their new UIDs, in matching order.
type copyUID struct {
	validity uint32
	src, dst imap.UIDSet
}

// pairs maps each source UID of a COPYUID to its new UID. ok is false when
// the reply cannot be read: sets of different sizes, or larger than the
// want UIDs asked for.
func (cu copyUID) pairs(want int) (map[uint32]uint32, bool) {
	src, okSrc := expandUIDs(cu.src, want)
	dst, okDst := expandUIDs(cu.dst, want)
	if !okSrc || !okDst || len(src) != len(dst) {
		return nil, false
	}
	out := make(map[uint32]uint32, len(src))
	for i, u := range src {
		out[u] = dst[i]
	}
	return out, true
}

// expandUIDs lists the UIDs of set in order, refusing one with more than
// limit UIDs or an open end. It counts in 64 bits, so a range ending at the
// largest UID ends.
func expandUIDs(set imap.UIDSet, limit int) ([]uint32, bool) {
	var out []uint32
	for _, r := range set {
		lo, hi := uint64(min(r.Start, r.Stop)), uint64(max(r.Start, r.Stop))
		if lo == 0 || hi-lo+1 > uint64(limit-len(out)) {
			return nil, false
		}
		for u := lo; u <= hi; u++ {
			out = append(out, uint32(u))
		}
	}
	return out, true
}

// uidMove sends UID MOVE. The server must have MOVE: imapclient falls back
// to COPY, STORE and EXPUNGE without it, which this package does itself.
func (c *conn) uidMove(ctx context.Context, set imap.UIDSet, dest string) (copyUID, error) {
	var cu copyUID
	err := c.do(ctx, func() error {
		data, err := c.client().Move(set, dest).Wait()
		if err != nil {
			return err
		}
		cu.validity = data.UIDValidity
		cu.src, _ = data.SourceUIDs.(imap.UIDSet) // nil without COPYUID
		cu.dst, _ = data.DestUIDs.(imap.UIDSet)
		return nil
	})
	return cu, err
}

// uidCopy sends UID COPY.
func (c *conn) uidCopy(ctx context.Context, set imap.UIDSet, dest string) (copyUID, error) {
	var cu copyUID
	err := c.do(ctx, func() error {
		data, err := c.client().Copy(set, dest).Wait()
		if err != nil {
			return err
		}
		cu = copyUID{validity: data.UIDValidity, src: data.SourceUIDs, dst: data.DestUIDs}
		return nil
	})
	return cu, err
}

// expungeUIDs marks set \Deleted and expunges exactly those UIDs, pipelined.
func (c *conn) expungeUIDs(ctx context.Context, set imap.UIDSet) error {
	return c.pipelined(ctx, func(cl *imapclient.Client) []func() error {
		store := cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil)
		expunge := cl.UIDExpunge(set)
		return []func() error{store.Close, expunge.Close}
	})
}

// append sends APPEND and returns the APPENDUID, which is zero without UIDPLUS.
func (c *conn) append(ctx context.Context, folder string, flags []imap.Flag, date time.Time, raw []byte) (*imap.AppendData, error) {
	var data *imap.AppendData
	err := c.do(ctx, func() error {
		cmd := c.client().Append(folder, int64(len(raw)), &imap.AppendOptions{Flags: flags, Time: date})
		// Close the command even after a failed write: until it ends, the
		// client holds its encoder. The server's reply, when there is one,
		// says more than the write error.
		_, werr := cmd.Write(raw)
		cerr := cmd.Close()
		d, err := cmd.Wait()
		if err != nil {
			return err
		}
		data = d
		return errors.Join(werr, cerr)
	})
	return data, err
}

// messageIDs returns the Message-ID, without brackets, of each message in
// set that exists, by UID. A message without one maps to "".
func (c *conn) messageIDs(ctx context.Context, set imap.UIDSet) (map[uint32]string, error) {
	bufs, err := c.fetch(ctx, set, &imap.FetchOptions{UID: true, Envelope: true})
	if err != nil {
		return nil, err
	}
	return idsOf(bufs), nil
}

// boundary reads folder's boundary with STATUS. A refused STATUS gives the
// zero boundary and no error: the write that follows decides the outcome.
func (c *conn) boundary(ctx context.Context, folder string) (boundary, error) {
	var b boundary
	err := c.do(ctx, statusBoundary(c.client(), folder, &b))
	return b, err
}

// statusBoundary sends a STATUS of folder's UIDVALIDITY and UIDNEXT, for
// pipelined, and stores the reply in b. A refused STATUS leaves b zero and
// is no error.
func statusBoundary(cl *imapclient.Client, folder string, b *boundary) func() error {
	cmd := cl.Status(folder, &imap.StatusOptions{UIDNext: true, UIDValidity: true})
	return func() error {
		data, err := cmd.Wait()
		var reply *imap.Error
		switch {
		case errors.As(err, &reply):
			return nil
		case err != nil:
			return err
		}
		*b = boundary{validity: data.UIDValidity, next: uint32(data.UIDNext)}
		return nil
	}
}

// fetchIDs sends a UID FETCH of the Message-IDs of set, for pipelined, and
// stores what it finds in ids. A refused FETCH leaves ids nil and is no
// error, so it never hides the outcome of a write pipelined with it.
func fetchIDs(cl *imapclient.Client, set imap.UIDSet, ids *map[uint32]string) func() error {
	cmd := cl.Fetch(set, &imap.FetchOptions{UID: true, Envelope: true})
	return func() error {
		bufs, err := cmd.Collect()
		var reply *imap.Error
		switch {
		case errors.As(err, &reply):
			return nil
		case err != nil:
			return err
		}
		*ids = idsOf(bufs)
		return nil
	}
}

// messageIDsBefore reads dest's boundary and the Message-IDs of set,
// pipelined, before a transfer that will send no COPYUID. Nothing has
// changed yet, so a refused FETCH is an error here.
func (c *conn) messageIDsBefore(ctx context.Context, set imap.UIDSet, dest string) (map[uint32]string, boundary, error) {
	var b boundary
	var bufs []*imapclient.FetchMessageBuffer
	err := c.pipelined(ctx, func(cl *imapclient.Client) []func() error {
		status := statusBoundary(cl, dest, &b)
		fetch := cl.Fetch(set, &imap.FetchOptions{UID: true, Envelope: true})
		return []func() error{status, func() error {
			var err error
			bufs, err = fetch.Collect()
			return err
		}}
	})
	if err != nil {
		return nil, boundary{}, err
	}
	return idsOf(bufs), b, nil
}

// moveWithIDs pipelines a STATUS of dest's boundary and a UID FETCH of the
// Message-IDs of set with the UID MOVE of set into dest, so both are there
// if no COPYUID comes back, at no extra round trip. A refused STATUS or
// FETCH leaves its part unknown and does not fail the MOVE, which may have
// gone through.
func (c *conn) moveWithIDs(ctx context.Context, set imap.UIDSet, dest string) (sentTransfer, error) {
	var t sentTransfer
	err := c.pipelined(ctx, func(cl *imapclient.Client) []func() error {
		status := statusBoundary(cl, dest, &t.before)
		fetch := fetchIDs(cl, set, &t.ids)
		mv := cl.Move(set, dest)
		return []func() error{status, fetch, func() error {
			data, err := mv.Wait()
			if err != nil {
				return err
			}
			t.cu.validity = data.UIDValidity
			t.cu.src, _ = data.SourceUIDs.(imap.UIDSet) // nil without COPYUID
			t.cu.dst, _ = data.DestUIDs.(imap.UIDSet)
			return nil
		}}
	})
	if err != nil {
		return sentTransfer{cu: t.cu}, err
	}
	return t, nil
}

// copyWithBoundary pipelines a STATUS of dest's boundary with the UID COPY
// of set into dest, so the boundary is there if no COPYUID comes back. A
// refused STATUS leaves it unknown and does not fail the COPY.
func (c *conn) copyWithBoundary(ctx context.Context, set imap.UIDSet, dest string) (sentTransfer, error) {
	var t sentTransfer
	err := c.pipelined(ctx, func(cl *imapclient.Client) []func() error {
		status := statusBoundary(cl, dest, &t.before)
		cp := cl.Copy(set, dest)
		return []func() error{status, func() error {
			data, err := cp.Wait()
			if err != nil {
				return err
			}
			t.cu = copyUID{validity: data.UIDValidity, src: data.SourceUIDs, dst: data.DestUIDs}
			return nil
		}}
	})
	return t, err
}

// idsOf maps the UID of each fetched message to its Message-ID, without
// brackets, or "" for a message without one.
func idsOf(bufs []*imapclient.FetchMessageBuffer) map[uint32]string {
	out := make(map[uint32]string, len(bufs))
	for _, b := range bufs {
		id := ""
		if b.Envelope != nil {
			id = b.Envelope.MessageID
		}
		out[uint32(b.UID)] = id
	}
	return out
}

// pipelined sends the commands send starts, all before any reply is read,
// then reads each one's reply on its own goroutine, as collector does for
// FETCH. Each completed command gives the rest a fresh command timeout. It
// returns the first error that broke the connection, else the first refusal.
func (c *conn) pipelined(ctx context.Context, send func(cl *imapclient.Client) []func() error) error {
	return c.pipeline(ctx, func(progress func()) error {
		waits := send(c.client())
		errs := make([]error, len(waits))
		var wg sync.WaitGroup
		for i, wait := range waits {
			wg.Go(func() {
				errs[i] = wait()
				progress()
			})
		}
		wg.Wait()
		return worstError(errs)
	})
}

// worstError returns the first error that is not the server's reply, since
// that one means the connection broke, else the first reply, else nil.
func worstError(errs []error) error {
	var first error
	for _, err := range errs {
		if err == nil {
			continue
		}
		var reply *imap.Error
		if !errors.As(err, &reply) {
			return err
		}
		if first == nil {
			first = err
		}
	}
	return first
}

// collectUIDs reads a UID FETCH into found, for pipelined.
func collectUIDs(cmd *imapclient.FetchCommand, found *[]uint32) func() error {
	return func() error {
		bufs, err := cmd.Collect()
		for _, b := range bufs {
			*found = append(*found, uint32(b.UID))
		}
		return err
	}
}

// imapFlags checks that each flag is one IMAP can store, a keyword or a
// system flag other than \Recent, so none can break the command it goes in.
func imapFlags(flags []string) ([]imap.Flag, error) {
	out := make([]imap.Flag, 0, len(flags))
	for _, f := range flags {
		if !validFlag(f) {
			return nil, fmt.Errorf("%w: a flag is not one IMAP can store", mailstore.ErrUnsupported)
		}
		out = append(out, imap.Flag(f))
	}
	return out, nil
}

// validFlag reports whether f is an IMAP keyword or a system flag that can
// be stored: printable ASCII atom characters, after one leading backslash
// for a system flag.
func validFlag(f string) bool {
	name, system := strings.CutPrefix(f, `\`)
	if name == "" || (system && strings.EqualFold(name, "Recent")) {
		return false
	}
	for i := range len(name) {
		if ch := name[i]; ch <= ' ' || ch >= 0x7f || strings.IndexByte(`(){%*"\]`, ch) >= 0 {
			return false
		}
	}
	return true
}

// bareMessageID trims spaces and angle brackets from a Message-ID. One with
// a line break is no Message-ID at all and gives "".
func bareMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimSuffix(strings.TrimPrefix(id, "<"), ">")
	if strings.ContainsAny(id, "\r\n") {
		return ""
	}
	return strings.TrimSpace(id)
}

// messageIDOf returns the Message-ID of a raw message, without brackets, or "".
func messageIDOf(raw []byte) string {
	h := readHeader(raw)
	return bareMessageID(h.Get("Message-Id"))
}
