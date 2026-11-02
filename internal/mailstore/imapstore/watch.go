package imapstore

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/emersion/go-imap/v2"

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
// nothing.
func (s *Store) SyncFolder(ctx context.Context, folder string, req mailstore.SyncRequest) (mailstore.Delta, error) {
	var d mailstore.Delta
	err := s.withConn(ctx, folder, func(c *conn) error {
		var err error
		d, err = s.syncPass(ctx, c, folder, req)
		return err
	})
	return d, err
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

// syncPass runs one sync pass on c with the semantics mailstore.SyncRequest
// documents. WatchConn.Sync and SyncFolder share it.
func (s *Store) syncPass(ctx context.Context, c *conn, folder string, req mailstore.SyncRequest) (mailstore.Delta, error) {
	data, err := s.selectForSync(ctx, c, folder)
	if err != nil {
		return mailstore.Delta{}, err
	}
	d := mailstore.Delta{State: folderState(c.uidValidity, c.uidNext, c.highestModSeq, c.client().Caps().Has(imap.CapCondStore))}
	exists := data.NumMessages
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
			return mailstore.Delta{}, err
		}
		return d, nil
	}
	if d.State.UIDNext > req.From.UIDNext {
		if err := s.syncNew(ctx, c, folder, req, &d); err != nil {
			return mailstore.Delta{}, err
		}
	}
	if err := s.syncChanged(ctx, c, exists, req.From, &d); err != nil {
		return mailstore.Delta{}, err
	}
	if req.Sweep && d.State.CondStore {
		if d.UIDs, err = c.allUIDs(ctx, exists); err != nil {
			return mailstore.Delta{}, err
		}
	}
	return d, nil
}

// syncNew fills d.New with summaries of the messages from req.From.UIDNext
// up to the UIDNEXT the pass selected, ascending, at most req.Batch of them.
// The server answers <n>:* with its highest UID when nothing is at n or
// above, so UIDs below req.From.UIDNext are dropped. UIDs at or above the
// selected UIDNEXT arrived after the SELECT and are left for the next pass,
// which the checkpoint from d.State sends there.
func (s *Store) syncNew(ctx context.Context, c *conn, folder string, req mailstore.SyncRequest, d *mailstore.Delta) error {
	from := max(req.From.UIDNext, 1)
	criteria := &imap.SearchCriteria{UID: []imap.UIDSet{{imap.UIDRange{Start: imap.UID(from), Stop: 0}}}}
	found, _, err := c.search(ctx, criteria, false)
	if err != nil {
		return err
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
		return nil
	}
	sums, err := s.fetchSummaries(ctx, c, folder, uids, req.Snippets)
	if err != nil {
		return err
	}
	d.New = sums
	return nil
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
	d.Changed, err = s.fetchStates(ctx, c, set, s.stateItems(true, from.HighestModSeq))
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
	states, err := s.fetchStates(ctx, c, imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, s.stateItems(flags, 0))
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
func (s *Store) fetchStates(ctx context.Context, c *conn, set imap.UIDSet, items *imap.FetchOptions) ([]mailstore.FlagState, error) {
	bufs, err := c.fetch(ctx, set, items)
	if err != nil {
		return nil, err
	}
	out := make([]mailstore.FlagState, 0, len(bufs))
	for _, b := range bufs {
		if b.UID == 0 {
			continue // not an answer to this FETCH
		}
		state := mailstore.FlagState{UID: uint32(b.UID), GmailMsgID: b.GmailMsgID, Labels: b.GmailLabels}
		if items.Flags {
			state.Flags = flagStrings(b.Flags)
		}
		out = append(out, state)
	}
	slices.SortFunc(out, func(a, b mailstore.FlagState) int { return cmp.Compare(a.UID, b.UID) })
	return out, nil
}
