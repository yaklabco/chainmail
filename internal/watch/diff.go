package watch

import (
	"cmp"
	"slices"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// diffInput is what diff needs besides the stored checkpoint and the delta.
type diffInput struct {
	Account string
	Folder  string
	// Sweep says the pass asked for the folder's full UID set.
	Sweep bool
	// At stamps every event and the checkpoint's LastSyncAt.
	At time.Time
	// Snippet renders a new message's snippet. Nil, or an empty result,
	// leaves Message.Snippet nil.
	Snippet func(mailstore.Summary) string
}

// diffResult is what one batch writes, all in one transaction.
type diffResult struct {
	// Events are ordered gap, status, expunged, flags, then new, and by
	// ascending UID within each kind.
	Events  []Event
	Next    FolderRow           // the checkpoint to store
	Replace map[uint32]knownUID // non-nil: replace the folder's UID set with it
	Put     map[uint32]knownUID // upserts
	Delete  []uint32
}

// diff is the pure core of Pass, which the table tests call directly. prev
// is the folder's stored row, or the zero row when it has none. known is
// the folder's stored UID set. diff reads it only for a sweep or full flags, so
// it may be nil otherwise.
//
// A rebased delta (a first pass, or a UIDVALIDITY change) yields a gap when
// the folder had a checkpoint, a baselined status and a new UID set, and
// never new, flags or expunged events, since mail already there is not news.
// Otherwise, on a sweep or with full flags (when d.UIDs is the folder's
// whole UID set, possibly empty), every UID known below the old UIDNEXT and
// missing from d.UIDs is expunged. Changed flags become flags events
// (CONDSTORE's CHANGEDSINCE result below the old UIDNEXT, or every known
// UID whose hash differs), and each new summary becomes a new event.
func diff(prev FolderRow, known map[uint32]knownUID, d mailstore.Delta, in diffInput) diffResult {
	res := diffResult{Next: nextCheckpoint(prev, d, in)}
	if d.Rebased {
		res.Events = rebaseEvents(prev, in)
		res.Replace = rebaseUIDs(d)
		return res
	}
	news := slices.SortedFunc(slices.Values(d.New), bySummaryUID)
	put := make(map[uint32]knownUID)
	if in.Sweep || d.FullFlags {
		var expunged []Event
		expunged, res.Delete = expungedEvents(prev, known, d, in)
		res.Events = append(res.Events, expunged...)
	}
	if d.FullFlags {
		res.Events = append(res.Events, fullFlagEvents(known, d, in, put)...)
	} else {
		res.Events = append(res.Events, condStoreFlagEvents(prev, news, d, in)...)
	}
	for _, s := range news {
		res.Events = append(res.Events, newMailEvent(s, in))
		k := knownUID{GmailMsgID: s.Ref.GmailMsgID}
		if !d.State.CondStore {
			k.FlagsHash = flagsHash(s.Flags, s.Labels)
		}
		put[s.Ref.UID] = k
	}
	if len(put) > 0 {
		res.Put = put
	}
	return res
}

// nextCheckpoint is the row to store after d. The checkpoint fields come
// from the folder as selected, except that a batch with more new mail to
// come stops just after its last new UID, so the next batch starts there.
// Mode, State and LastError carry over from prev, except that a baseline
// records the baselined state its status event reports.
func nextCheckpoint(prev FolderRow, d mailstore.Delta, in diffInput) FolderRow {
	next := FolderRow{
		Account:       in.Account,
		Folder:        in.Folder,
		UIDValidity:   d.State.UIDValidity,
		UIDNext:       d.State.UIDNext,
		HighestModSeq: d.State.HighestModSeq,
		CondStore:     d.State.CondStore,
		Mode:          prev.Mode,
		State:         prev.State,
		LastSyncAt:    in.At,
		LastSweepAt:   prev.LastSweepAt,
		LastError:     prev.LastError,
	}
	if d.More && len(d.New) > 0 {
		next.UIDNext = slices.MaxFunc(d.New, bySummaryUID).Ref.UID + 1
	}
	if in.Sweep || d.Rebased {
		next.LastSweepAt = in.At
	}
	if d.Rebased {
		next.State = StateBaselined
	}
	return next
}

// rebaseEvents reports a baseline: a uidvalidity gap when the folder had a
// checkpoint, so watchers know to query again, then a baselined status.
func rebaseEvents(prev FolderRow, in diffInput) []Event {
	var events []Event
	if prev.UIDValidity != 0 {
		gap := syncEvent(in, KindGap)
		gap.Reason = GapUIDValidity
		events = append(events, gap)
	}
	status := syncEvent(in, KindStatus)
	status.State = StateBaselined
	return append(events, status)
}

// rebaseUIDs is the folder's whole UID set after a baseline. Gmail message
// IDs come from d.Changed whenever it carries them, since on Gmail every
// message is there with or without CONDSTORE. Flag hashes come from it only
// with FullFlags. It is never nil, so an empty folder still clears the old set.
func rebaseUIDs(d mailstore.Delta) map[uint32]knownUID {
	changed := make(map[uint32]mailstore.FlagState, len(d.Changed))
	for _, f := range d.Changed {
		changed[f.UID] = f
	}
	uids := make(map[uint32]knownUID, len(d.UIDs))
	for _, uid := range d.UIDs {
		var k knownUID
		if f, ok := changed[uid]; ok {
			k.GmailMsgID = f.GmailMsgID
			if d.FullFlags {
				k.FlagsHash = flagsHash(f.Flags, f.Labels)
			}
		}
		uids[uid] = k
	}
	return uids
}

// expungedEvents compares the known UIDs below the old UIDNEXT with the
// folder's full UID set. Each one missing is expunged, with the Gmail
// message ID stored for it, and returned for deletion, ascending. A known
// UID at or above the old UIDNEXT is left alone: d.UIDs cannot prove it gone.
func expungedEvents(prev FolderRow, known map[uint32]knownUID, d mailstore.Delta, in diffInput) ([]Event, []uint32) {
	present := make(map[uint32]struct{}, len(d.UIDs))
	for _, uid := range d.UIDs {
		present[uid] = struct{}{}
	}
	var gone []uint32
	for uid := range known {
		if uid >= prev.UIDNext {
			continue
		}
		if _, ok := present[uid]; !ok {
			gone = append(gone, uid)
		}
	}
	slices.Sort(gone)
	var events []Event
	for _, uid := range gone {
		e := syncEvent(in, KindExpunged)
		e.Ref = syncRef(in, d.State.UIDValidity, uid, known[uid].GmailMsgID)
		events = append(events, e)
	}
	return events, gone
}

// condStoreFlagEvents turns a CHANGEDSINCE result into flags events. Only
// UIDs below the old UIDNEXT count: anything at or above it, or in this
// pass's new mail, is reported as new with its current flags instead.
func condStoreFlagEvents(prev FolderRow, news []mailstore.Summary, d mailstore.Delta, in diffInput) []Event {
	isNew := make(map[uint32]bool, len(news))
	for _, s := range news {
		isNew[s.Ref.UID] = true
	}
	var events []Event
	for _, f := range slices.SortedFunc(slices.Values(d.Changed), byFlagUID) {
		if f.UID >= prev.UIDNext || isNew[f.UID] {
			continue
		}
		events = append(events, flagsChangeEvent(in, d.State.UIDValidity, f, f.GmailMsgID))
	}
	return events
}

// fullFlagEvents compares every message's flags, without CONDSTORE, with the
// stored hashes. Each known UID whose hash differs gets a flags event and
// its new hash in put. UIDs not known yet (new in this or a later batch) are
// skipped: they arrive as new events.
func fullFlagEvents(known map[uint32]knownUID, d mailstore.Delta, in diffInput, put map[uint32]knownUID) []Event {
	var events []Event
	for _, f := range slices.SortedFunc(slices.Values(d.Changed), byFlagUID) {
		k, ok := known[f.UID]
		if !ok {
			continue
		}
		hash := flagsHash(f.Flags, f.Labels)
		if hash == k.FlagsHash {
			continue
		}
		msgID := cmp.Or(f.GmailMsgID, k.GmailMsgID)
		events = append(events, flagsChangeEvent(in, d.State.UIDValidity, f, msgID))
		put[f.UID] = knownUID{GmailMsgID: msgID, FlagsHash: hash}
	}
	return events
}

// newMailEvent is the new event for one summary.
func newMailEvent(s mailstore.Summary, in diffInput) Event {
	e := syncEvent(in, KindNew)
	e.Ref = s.Ref.String()
	e.Message = messageFromSummary(s, in.Snippet)
	return e
}

// messageFromSummary copies a summary into an event's message, so the event
// shares no slice with the store's result.
func messageFromSummary(s mailstore.Summary, snippet func(mailstore.Summary) string) *Message {
	m := &Message{
		Date:          s.Date,
		InternalDate:  s.InternalDate,
		From:          slices.Clone(s.From),
		To:            slices.Clone(s.To),
		Subject:       s.Subject,
		Flags:         slices.Clone(s.Flags),
		Labels:        slices.Clone(s.Labels),
		Size:          s.Size,
		HasAttachment: s.HasAttachment,
		ThreadID:      s.ThreadID,
	}
	if snippet != nil {
		if text := snippet(s); text != "" {
			m.Snippet = &text
		}
	}
	return m
}

// flagsChangeEvent is a flags event with the message's current flags and labels.
func flagsChangeEvent(in diffInput, uidValidity uint32, f mailstore.FlagState, msgID uint64) Event {
	e := syncEvent(in, KindFlags)
	e.Ref = syncRef(in, uidValidity, f.UID, msgID)
	e.Flags = slices.Clone(f.Flags)
	e.Labels = slices.Clone(f.Labels)
	return e
}

// syncEvent is an event of kind on the input's folder, stamped with its time.
func syncEvent(in diffInput, kind Kind) Event {
	return Event{At: in.At, Kind: kind, Account: in.Account, Folder: in.Folder}
}

// syncRef is the ref of a message in the input's folder.
func syncRef(in diffInput, uidValidity, uid uint32, msgID uint64) string {
	return ref.Ref{Account: in.Account, Folder: in.Folder, UIDValidity: uidValidity, UID: uid, GmailMsgID: msgID}.String()
}

func bySummaryUID(a, b mailstore.Summary) int { return cmp.Compare(a.Ref.UID, b.Ref.UID) }

func byFlagUID(a, b mailstore.FlagState) int { return cmp.Compare(a.UID, b.UID) }
