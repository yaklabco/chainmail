// Package mailstore is chainmail's provider-neutral view of a mail account:
// the interface the daemon reads and changes mail through and the types it
// returns. internal/mailstore/imapstore implements it over IMAP, and
// internal/mailstore/fake in memory for tests. Nothing here imports go-imap.
package mailstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jamesainslie/chainmail/internal/query"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// Errors a Store returns, wrapped with detail. The daemon maps them to API reasons.
var (
	// ErrNotFound means a folder, message or part does not exist.
	ErrNotFound = errors.New("not found")
	// ErrStaleRef means a ref or cursor names a folder whose UIDVALIDITY
	// changed, or a message that is gone. Searching again gives fresh refs.
	ErrStaleRef = errors.New("stale reference")
	// ErrUnavailable means the server could not be reached or refused, after retries.
	ErrUnavailable = errors.New("mail server unavailable")
	// ErrUnsupportedQuery means the account cannot express an operator in the query.
	ErrUnsupportedQuery = errors.New("query not supported on this account")
	// ErrUnsupported means the account cannot do this action, such as labels off Google.
	ErrUnsupported = errors.New("not supported on this account")
	// ErrRefused means folder rules forbid the change, such as deleting INBOX,
	// a role folder, or a folder with mail without force.
	ErrRefused = errors.New("refused")
	// ErrExists means a folder with that name already exists.
	ErrExists = errors.New("already exists")
	// ErrConnLimit means the server refused a connection for being over its
	// connection limit. It comes wrapped in ErrUnavailable: the refusal is
	// temporary, and a later try can get in once a connection closes.
	ErrConnLimit = errors.New("over the server's connection limit")
	// ErrThrottled means the server refused for a quota or bandwidth
	// limit, such as Gmail's daily download limit. It is ErrUnavailable
	// too, so callers that only know that keep working, while background
	// work can wait for the limit to renew instead of retrying at once.
	// It never carries the server's text.
	ErrThrottled = fmt.Errorf("%w: over a server quota or bandwidth limit", ErrUnavailable)
	// ErrMalformed means the mail server sent a response chainmail cannot
	// read, such as a body structure in a form the parser does not know.
	// Asking again gets the same response, so it is never retried.
	ErrMalformed = errors.New("the mail server sent a response chainmail cannot read")
)

// UnreadableError names the messages of one folder whose responses
// chainmail cannot read, even without their body structure. It is
// ErrMalformed. FetchHeaders and FetchSummaries return it together with
// what they read of the other messages.
type UnreadableError struct {
	Folder string
	UIDs   []uint32
}

func (e *UnreadableError) Error() string {
	return fmt.Sprintf("%v: UIDs %v in %q", ErrMalformed, e.UIDs, e.Folder)
}

// Unwrap makes an UnreadableError match ErrMalformed.
func (e *UnreadableError) Unwrap() error { return ErrMalformed }

// FolderNotFoundError is ErrNotFound for a folder the account does not
// have, such as the destination of a move. Name is the folder as the store
// looked for it: on a server with a personal namespace prefix, the prefixed
// name, so part of it is the server's choice and it is only for logs.
// Requested is the folder as the caller named it, before the store mapped
// it to the server's name, or empty when the caller named no folder, as
// for the Trash a trash finds by its role. Only Requested may reach a client.
type FolderNotFoundError struct {
	Name      string
	Requested string
}

func (e *FolderNotFoundError) Error() string {
	return fmt.Sprintf("%v: no folder %q", ErrNotFound, e.Name)
}

// Is makes a FolderNotFoundError match ErrNotFound.
func (e *FolderNotFoundError) Is(target error) bool { return target == ErrNotFound }

const (
	// SampleBytes is how much of a text part a store reads to build a
	// snippet when the server has no PREVIEW.
	SampleBytes = 2048
	// TextPartCap is the most of one text part FetchMessages reads.
	TextPartCap = 4 << 20
)

// Role is what a folder is for, from its special-use attribute or its name.
type Role string

// Roles.
const (
	RoleInbox   Role = "inbox"
	RoleSent    Role = "sent"
	RoleDrafts  Role = "drafts"
	RoleArchive Role = "archive"
	RoleTrash   Role = "trash"
	RoleSpam    Role = "spam"
	RoleAll     Role = "all"
	RoleNone    Role = "none"
)

// Folder is one mailbox.
type Folder struct {
	Name       string
	Role       Role
	Messages   uint32
	Unseen     uint32
	Selectable bool
}

// Address is a sender or recipient.
type Address struct {
	Name  string
	Email string
}

// TextSample is the start of a message's text part, still transfer-encoded
// and in its own charset, for render.Snippet.
type TextSample struct {
	MediaType string
	Charset   string
	Encoding  string
	Data      []byte
}

// Summary is what a search row shows about a message.
type Summary struct {
	Ref ref.Ref
	// Date is the Date header, or InternalDate when the header is missing.
	Date time.Time
	// InternalDate is when the server received the message. Searches by age
	// and date, and the merge across sources, use it.
	InternalDate time.Time
	From         []Address
	To           []Address
	Subject      string
	Flags        []string
	// Size is the whole raw message in bytes, as RFC822.SIZE reports it.
	// larger: and smaller: compare against it.
	Size int64
	// HasAttachment is true when any part is an attachment.
	HasAttachment bool
	// AttachmentsUnknown is true when the message was read without its body
	// structure, because the server sent that in a form chainmail cannot
	// read. HasAttachment is then false and a Header has no Attachments,
	// whatever the message holds.
	AttachmentsUnknown bool
	// ThreadID groups the messages of one thread. On Google accounts it is
	// Gmail's own thread ID, from X-GM-EXT-1, in lowercase hexadecimal.
	// Elsewhere it is ThreadKey of the root Message-ID, or "" when the
	// message has none.
	ThreadID string
	// Labels are Gmail labels, on Google accounts.
	Labels []string
	// Preview is the server's PREVIEW text, when snippets were asked for and
	// the server offers PREVIEW.
	Preview *string
	// Sample is the start of the text part, when snippets were asked for and
	// the server has no PREVIEW.
	Sample *TextSample
}

// Attachment is one attachment part, or a part the body shows by its
// Content-ID, such as an unnamed inline image. StreamPart gives its media
// type parameters, Content-ID and disposition.
type Attachment struct {
	// Part is the IMAP section number, such as "2" or "1.2".
	Part        string
	Filename    string
	ContentType string
	// Size is the encoded size the server reports.
	Size int64
}

// Message is a message ready to render.
type Message struct {
	Summary    Summary
	Cc         []Address
	MessageID  string
	InReplyTo  []string
	References []string
	// Raw is a MIME message holding the original header and the text parts,
	// without attachment bodies, for render.Render.
	Raw []byte
	// TextClipped says a text part was longer than TextPartCap.
	TextClipped bool
	Attachments []Attachment
}

// MessageResult is one entry of FetchMessages: a Message or an error.
type MessageResult struct {
	Message *Message
	Err     error
}

// Source is one folder a search ran in, and its UIDVALIDITY at the time.
type Source struct {
	Folder      string
	UIDValidity uint32
}

// Bound continues a search in one folder: only UIDs below Below, and only if
// the folder still has UIDValidity. Below 0 means no bound. A folder whose
// UIDVALIDITY changed fails on its own, as an ErrStaleRef in its
// SourceResult.Err, and the other bounded folders are still searched.
type Bound struct {
	UIDValidity uint32
	Below       uint32
}

// SearchRequest asks one account for matching messages.
type SearchRequest struct {
	// Query has had its in: and account: terms split off. Nil matches everything.
	Query *query.Node
	// In is the in: value, or empty. On Google accounts in:trash, in:spam and
	// in:anywhere add those folders as sources, and every other value is a
	// filter within All Mail, as Gmail's own in: is.
	In        string
	CountOnly bool
	// Bounds, when not nil, continues a search: exactly these folders are
	// searched, each within its bound, and In does not choose sources again.
	// On Google accounts In still filters within All Mail. A stale bound is a
	// per-source ErrStaleRef, not an error for the whole search.
	Bounds map[string]Bound
}

// SourceResult is what one folder of a search found.
type SourceResult struct {
	Source Source
	// UIDs are the matches, highest first. Empty when CountOnly.
	UIDs  []uint32
	Count uint32
	// Err is set when this folder failed. The other sources still count.
	Err error
}

// SummaryOptions control FetchSummaries.
type SummaryOptions struct {
	Snippets bool
}

// PartInfo describes a part being streamed.
type PartInfo struct {
	Filename string
	// ContentType is the media type, such as "text/plain", and Params its
	// parameters, such as charset, with lower-case names. Nil means none.
	// StreamPart decodes the transfer encoding only, so the bytes are still
	// in that charset.
	ContentType string
	Params      map[string]string
	// ContentID is the part's Content-ID without angle brackets, or "".
	ContentID string
	// Inline says the body shows the part: its disposition is inline, or it
	// has none and has a Content-ID.
	Inline bool
	// Size is the encoded size the server reports for the part, as in
	// Attachment.Size, so it can differ from the decoded bytes streamed. For
	// the whole message it is the raw size, as in Summary.Size.
	Size int64
}

// RefResult is one entry of a batch write: the message's ref afterwards, or an error.
// Ref is zero when the message no longer exists (expunge) or its new place is unknown.
type RefResult struct {
	Ref ref.Ref
	Err error
}

// Writer changes one account. Batch methods take refs of this account only,
// return one result per ref in order, and fail per ref. Semantics: spec 7.1.
// Writes open folders read-write with SELECT, and a write is never retried
// once the server has answered it.
type Writer interface {
	// Append stores raw in folder with flags and returns its ref, from
	// APPENDUID. The ref is zero, with no error, when the message was stored
	// but its place is unknown: the server gave no APPENDUID, or, on Google,
	// the message's X-GM-MSGID could not be read.
	Append(ctx context.Context, folder string, flags []string, date time.Time, raw []byte) (ref.Ref, error)
	// Move moves refs to folder and returns their new refs.
	Move(ctx context.Context, refs []ref.Ref, folder string) []RefResult
	// Copy copies refs to folder and returns the copies' refs.
	Copy(ctx context.Context, refs []ref.Ref, folder string) []RefResult
	// Archive moves refs to the folder with the archive role, or the
	// configured archive folder, with Gmail semantics on Google accounts.
	Archive(ctx context.Context, refs []ref.Ref) []RefResult
	// Trash moves refs to the folder with the trash role. A ref already in
	// Trash succeeds unchanged.
	Trash(ctx context.Context, refs []ref.Ref) []RefResult
	// Expunge permanently deletes refs. RefResult.Ref is zero on success.
	Expunge(ctx context.Context, refs []ref.Ref) []RefResult
	// StoreFlags adds and removes IMAP flags.
	StoreFlags(ctx context.Context, refs []ref.Ref, add, remove []string) []RefResult
	// StoreLabels adds and removes Gmail labels. Other accounts return
	// ErrUnsupported for every ref.
	StoreLabels(ctx context.Context, refs []ref.Ref, add, remove []string) []RefResult
	// CreateFolder creates name, and its missing parents first, and returns
	// the name the server gave the folder: with "/" written as the server's
	// hierarchy delimiter and, on a server that keeps folders under a
	// personal namespace prefix such as "INBOX.", under that prefix unless
	// name is INBOX or starts with it already. ErrExists when it already
	// exists.
	CreateFolder(ctx context.Context, name string) (string, error)
	// RenameFolder renames the folder from and returns its new name, to
	// written as CreateFolder writes a name.
	RenameFolder(ctx context.Context, from, to string) (string, error)
	// DeleteFolder refuses INBOX and role folders, and a folder with mail
	// unless force, with ErrRefused.
	DeleteFolder(ctx context.Context, name string, force bool) error
	// FindByMessageID returns the refs of messages in folder whose
	// Message-ID is messageID, which may come with or without angle brackets.
	FindByMessageID(ctx context.Context, folder, messageID string) ([]ref.Ref, error)
}

// Store reads and changes one account. Every method is safe for concurrent
// use. The read methods never change anything on the server: they accept a
// folder opened read-only or read-write, and fetch bodies with BODY.PEEK so
// nothing is marked read. The Writer methods do change the server.
type Store interface {
	Writer
	// ListFolders returns every folder, INBOX first, then by name ignoring case.
	ListFolders(ctx context.Context) ([]Folder, error)
	// Search returns the matches in each source folder. An error means the
	// whole account failed, such as an unsupported operator or a login refusal.
	Search(ctx context.Context, req SearchRequest) ([]SourceResult, error)
	// FetchSummaries returns summaries for uids in src, in the order given.
	// UIDs that no longer exist are left out. A message whose body
	// structure the server sent in a form chainmail cannot read comes back
	// with AttachmentsUnknown. Messages it cannot read at all are left out
	// too, and the error is an *UnreadableError naming them: the summaries
	// that come with that error are valid.
	FetchSummaries(ctx context.Context, src Source, uids []uint32, opts SummaryOptions) ([]Summary, error)
	// FetchMessages reads refs, which must belong to this account. The
	// result has one entry per ref, in order. A message whose response
	// chainmail cannot read fails with ErrMalformed.
	FetchMessages(ctx context.Context, refs []ref.Ref) []MessageResult
	// StreamPart opens one part, decoded, or with part "" the whole raw
	// message. The caller must close the reader. Bytes already read are
	// never fetched again, so a failure mid-stream is final.
	StreamPart(ctx context.Context, r ref.Ref, part string) (PartInfo, io.ReadCloser, error)
	// Thread returns the refs of every message in r's thread, r included.
	// On Google accounts the thread is every message in All Mail with r's
	// Gmail thread ID. Elsewhere it is every message outside Trash, Junk and
	// any \All folder whose Message-ID is r's root Message-ID or whose
	// References or In-Reply-To contain it. The order is unspecified:
	// callers sort.
	Thread(ctx context.Context, r ref.Ref) ([]ref.Ref, error)
	// Close logs out of every connection, within 5 seconds.
	Close() error
}
