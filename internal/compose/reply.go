package compose

import (
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/jamesainslie/chainmail/internal/render"
)

// maxReferences is how many Message-IDs a reply's References header keeps.
const maxReferences = 20

// quoteDateLayout is how a quote header writes the original's date.
const quoteDateLayout = "Mon, 2 Jan 2006 at 15:04"

// forwardMarker opens the header block of a forwarded message.
const forwardMarker = "---------- Forwarded message ---------"

// ReplyParts is what a reply adds to a Spec.
type ReplyParts struct {
	To, Cc     []mail.Address
	Subject    string
	InReplyTo  string
	References []string
	Quote      *Quote
}

// Reply builds the parts of a reply to orig from an account whose own
// addresses are self. all adds the original To and Cc. An empty recipient
// list is ErrInvalid.
//
// To is the original's Reply-To, or its From when it has none. With all, Cc
// is the original's To and Cc. Every address in self is dropped from both,
// as are repeats (the first is kept) and Cc addresses already in To.
// Addresses compare without regard to case. When only Cc is left, as in a
// reply to all on the account's own message, it becomes To. loc is the zone the quote's
// attribution line writes the original's date in; nil means time.Local.
func Reply(orig Original, self []string, all bool, loc *time.Location) (ReplyParts, error) {
	seen := make(map[string]bool, len(self))
	for _, a := range self {
		seen[addrKey(a)] = true
	}
	to := orig.ReplyTo
	if len(to) == 0 {
		to = orig.From
	}
	p := ReplyParts{To: keepNew(to, seen)}
	if all {
		p.Cc = keepNew(slices.Concat(orig.To, orig.Cc), seen)
	}
	if len(p.To) == 0 {
		// A reply to the account's own message goes to whoever it was sent to.
		p.To, p.Cc = p.Cc, nil
	}
	if len(p.To) == 0 {
		return ReplyParts{}, fmt.Errorf("%w: the reply has no recipients", ErrInvalid)
	}
	p.Subject = prefixSubject(orig.Subject, "Re: ", "re:")
	p.References = slices.Clone(orig.References)
	if orig.MessageID != "" {
		p.InReplyTo = orig.MessageID
		p.References = append(p.References, orig.MessageID)
	}
	if n := len(p.References); n > maxReferences {
		p.References = p.References[n-maxReferences:]
	}
	if len(p.References) == 0 {
		p.References = nil
	}
	header := formatAddresses(orig.From) + " wrote:"
	if !orig.Date.IsZero() {
		header = "On " + quoteDate(orig.Date, loc) + ", " + header
	}
	p.Quote = &Quote{
		Header: header,
		Text:   orig.Text,
		HTML:   SanitizeHTML(orig.HTML),
	}
	return p, nil
}

// ForwardParts is what a forward adds to a Spec.
type ForwardParts struct {
	Subject     string
	Quote       *Quote
	Attachments []File // orig.Attachments
}

// Forward builds the parts of a forward of orig. The quote header is the
// forwarded-message block with the original's From, Date, Subject and To,
// its date written in loc; nil means time.Local.
func Forward(orig Original, loc *time.Location) ForwardParts {
	date := ""
	if !orig.Date.IsZero() {
		date = quoteDate(orig.Date, loc)
	}
	header := strings.Join([]string{
		forwardMarker,
		"From: " + formatAddresses(orig.From),
		"Date: " + date,
		"Subject: " + render.CleanHeader(orig.Subject),
		"To: " + formatAddresses(orig.To),
	}, "\n")
	return ForwardParts{
		Subject: prefixSubject(orig.Subject, "Fwd: ", "fwd:", "fw:"),
		Quote: &Quote{
			Header:  header,
			Text:    orig.Text,
			HTML:    SanitizeHTML(orig.HTML),
			Forward: true,
		},
		Attachments: slices.Clone(orig.Attachments),
	}
}

// keepNew returns the addresses in as whose keys are not in seen, adding
// each kept key to seen.
func keepNew(as []mail.Address, seen map[string]bool) []mail.Address {
	var out []mail.Address
	for _, a := range as {
		k := addrKey(a.Address)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, a)
	}
	return out
}

// addrKey is how an address compares: trimmed and lower case.
func addrKey(a string) string {
	return strings.ToLower(strings.TrimSpace(a))
}

// prefixSubject returns subject on one line, with prefix in front unless it
// already starts with one of have, which are lower case, in any case.
func prefixSubject(subject, prefix string, have ...string) string {
	s := render.CleanHeader(subject)
	l := strings.ToLower(s)
	for _, h := range have {
		if strings.HasPrefix(l, h) {
			return s
		}
	}
	return prefix + s
}

// quoteDate writes t in loc for a quote header.
func quoteDate(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format(quoteDateLayout)
}

// formatAddresses writes as for a quote header: "Name <addr>", or the bare
// address when there is no name, joined by ", ", with every value on one
// line and free of invisible characters.
func formatAddresses(as []mail.Address) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		addr := render.CleanHeader(a.Address)
		if name := render.CleanHeader(a.Name); name != "" {
			parts = append(parts, name+" <"+addr+">")
		} else {
			parts = append(parts, addr)
		}
	}
	return strings.Join(parts, ", ")
}
