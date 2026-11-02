package mailstore

import (
	"context"
	"time"
)

// FolderState is what SELECT or STATUS reports about a folder.
type FolderState struct {
	UIDValidity uint32
	UIDNext     uint32
	// HighestModSeq is 0 when the server has no CONDSTORE.
	HighestModSeq uint64
	// CondStore is true only when the server advertises CONDSTORE and the
	// folder reports a non-zero HIGHESTMODSEQ (a NOMODSEQ folder syncs the
	// full-flags way).
	CondStore bool
}

// Checkpoint is where a folder's last sync pass left off.
type Checkpoint struct {
	UIDValidity   uint32
	UIDNext       uint32
	HighestModSeq uint64
}

// FlagState is one message's current flags and, on Gmail, labels.
type FlagState struct {
	UID        uint32
	GmailMsgID uint64
	Flags      []string
	Labels     []string
}

// SyncRequest asks for one sync pass on a folder.
type SyncRequest struct {
	// From is the folder's checkpoint. The zero value asks for a baseline.
	From Checkpoint
	// Sweep asks for the folder's full UID set in Delta.UIDs.
	Sweep bool
	// Batch caps len(Delta.New). 0 means 500.
	Batch int
	// Snippets fetches PREVIEW or a text sample for each new message.
	Snippets bool
}

// Delta is what one sync pass found.
type Delta struct {
	// State is the folder as selected for this pass.
	State FolderState
	// Rebased is set when the pass was a baseline: From was zero, or its
	// UIDVALIDITY no longer matched. New and Changed are then empty, and
	// UIDs (with Changed, without CONDSTORE) describe the whole folder.
	Rebased bool
	// New holds summaries of messages with UID >= From.UIDNext, ascending
	// by UID, at most Batch of them.
	New []Summary
	// More is set when more new messages exist beyond New.
	More bool
	// Changed holds flag states. With CONDSTORE it is the CHANGEDSINCE
	// result for UIDs below From.UIDNext. Without CONDSTORE it is every
	// message in the folder, and FullFlags is set.
	Changed   []FlagState
	FullFlags bool
	// UIDs is the folder's full UID set, ascending, when Rebased, Sweep
	// or FullFlags: non-nil (possibly empty) in those cases, nil otherwise.
	// Consumers decide by those flags, never by a nil check.
	UIDs []uint32
}

// Wake is why Idle returned.
type Wake struct {
	// Changed is set when the server reported EXISTS, FETCH or EXPUNGE.
	Changed bool
	// Expunged is set when the server reported EXPUNGE.
	Expunged bool
}

// WatchConn is a dedicated connection that stays selected on one folder.
// It is not part of the store's pool. It is not safe for concurrent use.
type WatchConn interface {
	// Sync runs one pass on the folder.
	Sync(ctx context.Context, req SyncRequest) (Delta, error)
	// IdleSupported reports whether the server advertises IDLE.
	IdleSupported() bool
	// Idle waits in IDLE until the server reports a change, limit passes,
	// or ctx ends. It returns Wake{} with a nil error when limit passed.
	// When ctx ends it sends DONE, returns ctx.Err(), and leaves the
	// connection selected and usable; if DONE fails the connection is dead.
	Idle(ctx context.Context, limit time.Duration) (Wake, error)
	// Close logs out, bounded at 5 seconds.
	Close() error
}

// Watcher is a store that can watch and sync folders. imapstore.Store and
// fake.Store implement it.
type Watcher interface {
	// OpenWatch dials a dedicated connection and selects folder on it.
	OpenWatch(ctx context.Context, folder string) (WatchConn, error)
	// FolderState runs STATUS on a pooled connection.
	FolderState(ctx context.Context, folder string) (FolderState, error)
	// SyncFolder runs one pass on a pooled connection.
	SyncFolder(ctx context.Context, folder string, req SyncRequest) (Delta, error)
}

// DefaultSyncBatch is the Batch used when SyncRequest.Batch is 0.
const DefaultSyncBatch = 500
