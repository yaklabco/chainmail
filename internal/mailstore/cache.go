package mailstore

import (
	"context"

	"github.com/jamesainslie/chainmail/internal/ref"
)

// Header is what the local cache keeps of a message without its body: the
// summary a search row shows and the fields a summary leaves out.
type Header struct {
	// Summary has Ref, the dates, From, To, Subject, Flags, Size,
	// HasAttachment, ThreadID, MessageID and Labels, as FetchSummaries gives
	// them. Preview and Sample are nil.
	Summary Summary
	Cc      []Address
	// Bcc is what a message in Sent or Drafts carries. A received message
	// has none.
	Bcc []Address
	// MessageID is the Message-ID without angle brackets, as
	// Message.MessageID.
	MessageID string
	// InReplyTo and References are as Message has them.
	InReplyTo  []string
	References []string
	// Attachments are the attachment parts, as Message has them. They are
	// nil when Summary.AttachmentsUnknown is set.
	Attachments []Attachment
}

// HeaderFetcher is implemented by stores that can fetch headers for the
// local cache.
type HeaderFetcher interface {
	// FetchHeaders returns the headers of uids in src.Folder, in the order
	// of uids, leaving out UIDs the folder no longer holds. A folder whose
	// UIDVALIDITY is not src.UIDValidity is ErrStaleRef. It never sets
	// \Seen. A message whose body structure the server sent in a form
	// chainmail cannot read comes back with Summary.AttachmentsUnknown.
	// Messages it cannot read at all are left out, and the error is an
	// *UnreadableError naming them: the headers that come with that error
	// are valid. A plain ErrMalformed means the store could not isolate
	// the messages at fault. It never does so for IsolatedBatch UIDs or
	// fewer, so from such a fetch it is a fault in the folder, such as its
	// SELECT answer, and not in a message.
	FetchHeaders(ctx context.Context, src Source, uids []uint32) ([]Header, error)
}

// IsolatedBatch is the largest fetch in which FetchHeaders and
// FetchSummaries always isolate the messages they cannot read, however
// many of them are at fault: each comes back in an *UnreadableError, never
// as a plain ErrMalformed.
const IsolatedBatch = 5

// Scoper is implemented by stores that can say which folders a search
// covers.
type Scoper interface {
	// SearchFolders returns the folders Search uses for the in: value in,
	// with Bounds nil, in the order Search reports them, and the same
	// errors.
	SearchFolders(ctx context.Context, in string) ([]string, error)
}

// ThreadFinder is implemented by stores that can find a thread's messages
// by its key, with no seed message.
type ThreadFinder interface {
	// FindThread returns the refs of the account's messages in the thread
	// with key threadKey, in the default search scope, sorted by folder then
	// UID. On Google it searches All Mail by X-GM-THRID (threadKey is the
	// lowercase hex thread ID, as Summary.ThreadID gives it) and ignores
	// rootMessageID. Elsewhere it runs the same search the server thread
	// call runs for a root Message-ID, using rootMessageID, with or without
	// angle brackets, and ignores threadKey. No match is an empty slice and
	// a nil error, and so are a Google key Summary.ThreadID could not have
	// given and an empty rootMessageID elsewhere. It never sets \Seen.
	FindThread(ctx context.Context, threadKey, rootMessageID string) ([]ref.Ref, error)
}
