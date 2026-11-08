// Package compose builds outgoing MIME messages from a typed spec. It does no I/O.
package compose

import (
	"errors"
	"fmt"
	"net/mail"
	"time"
)

// BodyKind says how Body.Source is written.
type BodyKind int

// Body kinds.
const (
	// Markdown is sent as the text part and rendered to the HTML part.
	Markdown BodyKind = iota
	// Text is sent as the only part, format=flowed.
	Text
	// HTML is sent as given, with HTMLText or a derived text part.
	HTML
)

// Body is the text of a new message.
type Body struct {
	Kind   BodyKind
	Source string
	// HTMLText is the text part for an HTML body. Empty means derived from Source.
	HTMLText string
}

// File is an attachment or inline image.
type File struct {
	Filename    string
	ContentType string
	// ContentID, without angle brackets, is written as the part's Content-ID,
	// so HTML can show it as cid:ContentID, as a forwarded original's quoted
	// HTML shows its images. Empty means none. An image of Spec.Inline gets a
	// Content-ID of its own instead.
	ContentID string
	// Inline marks an attachment the body shows. It is written with an
	// inline disposition and, when it has a ContentID and the message an HTML
	// part, inside the multipart/related around the body.
	Inline bool
	Data   []byte
}

// Quote is the quoted original under a reply or forward, built by ReplyParts or ForwardParts.
type Quote struct {
	// Header is the attribution line ("On ..., X wrote:") or the forwarded-message block, as text.
	Header string
	// Text is the original's text.
	Text string
	// HTML is the original's HTML, already sanitized. Empty means use Text.
	HTML string
	// Forward means no "> " prefixes and no blockquote, as a forward shows the original.
	Forward bool
}

// Spec is a message to build.
type Spec struct {
	From        mail.Address
	To, Cc, Bcc []mail.Address
	Subject     string
	Body        Body
	// Signature is Markdown. Empty means none.
	Signature   string
	Attachments []File
	// Inline holds images the body shows. The key is the name the body uses.
	Inline map[string]File
	// Headers holds only Reply-To and X- headers.
	Headers    map[string]string
	InReplyTo  string
	References []string
	Quote      *Quote
	Date       time.Time
	// MessageID is without angle brackets.
	MessageID string
	UserAgent string
	// Calendar, when set, is an iCalendar object sent as an alternative to
	// the body, as RFC 6047 sends an iTIP message such as a REPLY: the body
	// becomes multipart/alternative with the text part, the HTML part when
	// there is one, and then a text/calendar part with its method and no
	// disposition.
	Calendar *CalendarAlternative
}

// CalendarAlternative is the iCalendar part of an iMIP message (RFC 6047).
type CalendarAlternative struct {
	// Method is the iTIP method, such as REPLY, written upper-cased as the
	// part's method parameter. It must match the METHOD property in Data,
	// which Build does not read.
	Method string
	// Data is the VCALENDAR object, in UTF-8.
	Data []byte
}

// Message is a built message and its SMTP envelope.
type Message struct {
	Raw          []byte
	MessageID    string
	EnvelopeFrom string
	// EnvelopeTo is the To, Cc and Bcc addresses, deduplicated, in order.
	EnvelopeTo []string
}

// Original is what replies and forwards need from the message they answer.
type Original struct {
	MessageID             string
	References            []string
	From, ReplyTo, To, Cc []mail.Address
	Subject               string
	Date                  time.Time
	Text                  string
	// HTML is the raw HTML of the original. ReplyParts sanitizes it.
	HTML        string
	Attachments []File
	// Truncated is set when Text or HTML is only part of the original.
	// The quote then ends with TruncatedMarker in both parts.
	Truncated bool
}

// ErrInvalid wraps every spec problem the caller can fix.
var ErrInvalid = errors.New("invalid message")

// ErrNoRecipients is Reply's error when nobody is left to reply to, as in a
// note the account sent itself. It wraps ErrInvalid, and Reply still fills
// in the rest of the parts, so a caller that names recipients can go on.
var ErrNoRecipients = fmt.Errorf("%w: the reply has no recipients", ErrInvalid)
