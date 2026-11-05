package imapstore

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// FolderState implements mailstore.Watcher with STATUS on a pooled
// connection. It asks for HIGHESTMODSEQ only when the server advertises
// CONDSTORE, and CondStore is set only when the folder reports one.
func (s *Store) FolderState(ctx context.Context, folder string) (mailstore.FolderState, error) {
	var st mailstore.FolderState
	err := s.withConn(ctx, "", func(c *conn) error {
		condStore := c.client().Caps().Has(imap.CapCondStore)
		var data *imap.StatusData
		err := c.do(ctx, func() error {
			var err error
			data, err = c.client().Status(folder, &imap.StatusOptions{UIDNext: true, UIDValidity: true, HighestModSeq: condStore}).Wait()
			return err
		})
		if err != nil {
			if missingFolder(err) {
				s.forgetFolders()
				return &mailstore.FolderNotFoundError{Name: folder, Requested: folder}
			}
			return err
		}
		st = folderState(data.UIDValidity, uint32(data.UIDNext), data.HighestModSeq, condStore)
		return nil
	})
	return st, err
}

// SyncFolder implements mailstore.Watcher on a pooled connection. A
// dropped connection is retried on a fresh one, since a pass changes
// nothing. New mail the pass could not read on its connection is read
// again through FetchSummaries once that connection is back, so the read
// never waits for a pool slot while it holds one.
func (s *Store) SyncFolder(ctx context.Context, folder string, req mailstore.SyncRequest) (mailstore.Delta, error) {
	var d mailstore.Delta
	var unread []uint32
	err := s.withConn(ctx, folder, func(c *conn) error {
		var err error
		d, unread, err = s.syncPass(ctx, c, folder, req, nil)
		return err
	})
	if err == nil && len(unread) > 0 {
		err = s.readNew(ctx, folder, req, &d, unread)
	}
	return d, err
}

// readNew fills d.New with summaries of the new mail uids, and d.Unreadable
// with the UIDs of those it cannot read, through FetchSummaries, which
// isolates a message the server sent in a form chainmail cannot read. A
// pass calls it when its own FETCH of that mail failed so.
func (s *Store) readNew(ctx context.Context, folder string, req mailstore.SyncRequest, d *mailstore.Delta, uids []uint32) error {
	src := mailstore.Source{Folder: folder, UIDValidity: d.State.UIDValidity}
	sums, err := s.FetchSummaries(ctx, src, uids, mailstore.SummaryOptions{Snippets: req.Snippets})
	if unreadable, ok := errors.AsType[*mailstore.UnreadableError](err); ok {
		d.Unreadable, err = unreadable.UIDs, nil
	}
	if err != nil {
		return err
	}
	d.New = sums
	return nil
}

// folderState is a mailstore.FolderState from what SELECT or STATUS
// reported. A folder whose server has CONDSTORE but that reports no
// HIGHESTMODSEQ keeps no mod-sequences, so it syncs as if without CONDSTORE.
func folderState(uidValidity, uidNext uint32, highestModSeq uint64, condStore bool) mailstore.FolderState {
	st := mailstore.FolderState{UIDValidity: uidValidity, UIDNext: uidNext}
	if condStore && highestModSeq > 0 {
		st.HighestModSeq, st.CondStore = highestModSeq, true
	}
	return st
}

// selectForSync opens folder on c afresh, read-only, with CONDSTORE when the
// server advertises it, so the answer reports the folder as it is now. A
// missing folder is a FolderNotFoundError.
func (s *Store) selectForSync(ctx context.Context, c *conn, folder string) (*imap.SelectData, error) {
	c.folderGen = s.folderGen.Load()
	writes := s.writeGen.Load()
	c.forget()
	data, err := c.selectFolder(ctx, folder, false, c.client().Caps().Has(imap.CapCondStore))
	switch {
	case err == nil:
		c.writeGen = writes
		return data, nil
	case errors.Is(err, mailstore.ErrNotFound):
		s.forgetFolders()
		return nil, &mailstore.FolderNotFoundError{Name: folder, Requested: folder}
	default:
		return nil, err
	}
}

// passView is what a pass saw of the folder, for a watch connection to
// compare with its last pass. The caller sets since; the pass fills in the
// rest.
type passView struct {
	// since is the UID from which the pass counts new mail: the UIDNEXT of
	// the connection's last pass.
	since uint32
	// uidValidity, uidNext and exists are what the SELECT reported.
	uidValidity, uidNext, exists uint32
	// added counts the messages SEARCH found from since up to uidNext.
	// counted says it searched from since or lower, so added is complete.
	added   uint32
	counted bool
}

// syncPass runs one sync pass on c with the semantics mailstore.SyncRequest
// documents. WatchConn.Sync and SyncFolder share it. When view is not nil
// the pass records in it what it saw.
//
// It fetches the new mail last. When that FETCH fails because the server
// sent a message in a form chainmail cannot read, c is dead, and syncPass
// returns the delta without New, and the UIDs of the new mail for the
// caller to read through readNew, which isolates the message at fault.
func (s *Store) syncPass(ctx context.Context, c *conn, folder string, req mailstore.SyncRequest, view *passView) (mailstore.Delta, []uint32, error) {
	data, err := s.selectForSync(ctx, c, folder)
	if err != nil {
		return mailstore.Delta{}, nil, err
	}
	d := mailstore.Delta{State: folderState(c.uidValidity, c.uidNext, c.highestModSeq, c.client().Caps().Has(imap.CapCondStore))}
	exists := data.NumMessages
	if view != nil {
		view.uidValidity, view.uidNext, view.exists = d.State.UIDValidity, d.State.UIDNext, exists
	}
	if req.From == (mailstore.Checkpoint{}) || req.From.UIDValidity != d.State.UIDValidity {
		d.Rebased = true
		switch {
		case !d.State.CondStore:
			d.Changed, d.UIDs, err = s.allStates(ctx, c, exists, true)
			d.FullFlags = true
		case s.gmail():
			// X-GM-MSGID only, so the engine can store each message's Gmail
			// ID for the ref of a later expunged event.
			d.Changed, d.UIDs, err = s.allStates(ctx, c, exists, false)
		default:
			d.UIDs, err = c.allUIDs(ctx, exists)
		}
		if err != nil {
			return mailstore.Delta{}, nil, err
		}
		return d, nil, nil
	}
	if err := s.syncChanged(ctx, c, exists, req.From, &d); err != nil {
		return mailstore.Delta{}, nil, err
	}
	if req.Sweep && d.State.CondStore {
		if d.UIDs, err = c.allUIDs(ctx, exists); err != nil {
			return mailstore.Delta{}, nil, err
		}
	}
	if d.State.UIDNext <= req.From.UIDNext {
		return d, nil, nil
	}
	unread, err := s.syncNew(ctx, c, folder, req, &d, view)
	if err != nil {
		return mailstore.Delta{}, nil, err
	}
	return d, unread, nil
}

// syncNew fills d.New with summaries of the messages from req.From.UIDNext
// up to the UIDNEXT the pass selected, ascending, at most req.Batch of them.
// The server answers <n>:* with its highest UID when nothing is at n or
// above, so UIDs below req.From.UIDNext are dropped. UIDs at or above the
// selected UIDNEXT arrived after the SELECT and are left for the next pass,
// which the checkpoint from d.State sends there.
// A view that is not nil gets the count of new mail from view.since on.
// When the FETCH fails because the server sent a message in a form
// chainmail cannot read, c is dead, and syncNew returns the UIDs it was
// fetching, for syncPass's caller to read again.
func (s *Store) syncNew(ctx context.Context, c *conn, folder string, req mailstore.SyncRequest, d *mailstore.Delta, view *passView) ([]uint32, error) {
	from := max(req.From.UIDNext, 1)
	criteria := &imap.SearchCriteria{UID: []imap.UIDSet{{imap.UIDRange{Start: imap.UID(from), Stop: 0}}}}
	found, _, err := c.search(ctx, criteria, false)
	if err != nil {
		return nil, err
	}
	if view != nil && from <= view.since {
		view.counted = true
		for _, u := range found {
			if u >= view.since && u < d.State.UIDNext {
				view.added++
			}
		}
	}
	uids := slices.DeleteFunc(found, func(u uint32) bool { return u < from || u >= d.State.UIDNext })
	slices.Sort(uids)
	batch := req.Batch
	if batch <= 0 {
		batch = mailstore.DefaultSyncBatch
	}
	if len(uids) > batch {
		uids, d.More = uids[:batch], true
	}
	if len(uids) == 0 {
		return nil, nil
	}
	sums, err := s.fetchSummaries(ctx, c, folder, uids, req.Snippets)
	switch {
	case splittable(err):
		return uids, nil
	case err != nil:
		return nil, err
	}
	d.New = sums
	return nil, nil
}

// syncChanged fills d.Changed. With CONDSTORE it holds the messages below
// from.UIDNext whose mod-sequence passed from.HighestModSeq. Without it, it
// holds every message, and d.UIDs the full UID set.
func (s *Store) syncChanged(ctx context.Context, c *conn, exists uint32, from mailstore.Checkpoint, d *mailstore.Delta) error {
	var err error
	if !d.State.CondStore {
		d.Changed, d.UIDs, err = s.allStates(ctx, c, exists, true)
		d.FullFlags = true
		return err
	}
	if d.State.HighestModSeq <= from.HighestModSeq || from.UIDNext <= 1 || exists == 0 {
		return nil
	}
	var set imap.UIDSet
	set.AddRange(1, imap.UID(from.UIDNext-1))
	d.Changed, err = s.fetchStates(ctx, c, set, s.stateItems(true, from.HighestModSeq), 0)
	return err
}

// allUIDs returns every UID in the folder open on c, ascending: empty and
// not nil when the folder is.
func (c *conn) allUIDs(ctx context.Context, exists uint32) ([]uint32, error) {
	if exists == 0 {
		return []uint32{}, nil
	}
	uids, _, err := c.search(ctx, &imap.SearchCriteria{}, false)
	if err != nil {
		return nil, err
	}
	slices.Sort(uids)
	return uids, nil
}

// allStates returns the state of every message in the folder open on c,
// with its flags when flags is set, and the folder's UIDs, both ascending.
// The UIDs are empty and not nil when the folder is.
func (s *Store) allStates(ctx context.Context, c *conn, exists uint32, flags bool) ([]mailstore.FlagState, []uint32, error) {
	if exists == 0 {
		return nil, []uint32{}, nil
	}
	states, err := s.fetchStates(ctx, c, imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, s.stateItems(flags, 0), int(exists))
	if err != nil {
		return nil, nil, err
	}
	uids := make([]uint32, len(states))
	for i, f := range states {
		uids[i] = f.UID
	}
	return states, uids, nil
}

// stateItems is what a pass fetches for a message's state: UID, with FLAGS
// when flags is set, and on Gmail X-GM-MSGID, with X-GM-LABELS when flags is
// set. A changedSince other than 0 adds CHANGEDSINCE.
func (s *Store) stateItems(flags bool, changedSince uint64) *imap.FetchOptions {
	return &imap.FetchOptions{
		UID: true, Flags: flags, ChangedSince: changedSince,
		GmailMsgID: s.gmail(), GmailLabels: s.gmail() && flags,
	}
}

// fetchStates fetches items for set and returns the states, ascending by UID.
// It reads the answer a message at a time (conn.fetchEach), so a folder of
// hundreds of thousands of messages, whose answer takes longer than the
// command timeout, syncs while the server keeps answering. expect is how
// many messages the answer likely holds, 0 when unknown, to size the result.
func (s *Store) fetchStates(ctx context.Context, c *conn, set imap.UIDSet, items *imap.FetchOptions, expect int) ([]mailstore.FlagState, error) {
	out := make([]mailstore.FlagState, 0, expect)
	err := c.fetchEach(ctx, set, items, func(b *imapclient.FetchMessageBuffer) {
		if b.UID == 0 {
			return // not an answer to this FETCH
		}
		state := mailstore.FlagState{UID: uint32(b.UID), GmailMsgID: b.GmailMsgID, Labels: b.GmailLabels}
		if items.Flags {
			state.Flags = flagStrings(b.Flags)
		}
		out = append(out, state)
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b mailstore.FlagState) int { return cmp.Compare(a.UID, b.UID) })
	return out, nil
}
