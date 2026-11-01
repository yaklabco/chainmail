package compose

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	mimetext "github.com/emersion/go-message/textproto"
	nethtml "golang.org/x/net/html"

	"github.com/jamesainslie/chainmail/internal/render"
)

// crockford is Crockford's base32 alphabet, as ULIDs use: no I, L, O or U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// htmlHead opens the document a Markdown body is rendered into. The style
// block is small on purpose: mail clients drop or rewrite most CSS.
const htmlHead = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; font-size: 14px; line-height: 1.5; }
blockquote { margin: 0 0 0 0.8ex; padding-left: 1ex; border-left: 3px solid #ccc; }
code, pre { background: #f4f4f4; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 90%; }
pre { padding: 8px; }
</style>
</head>
<body>
`

const htmlTail = "</body>\n</html>\n"

// Transfer encodings: quoted-printable for text and base64 for binary, so
// no line exceeds 998 bytes, and 7bit for composite attachments that are
// already 7-bit data, as RFC 2046 requires of them.
const (
	quotedPrintable  = "quoted-printable"
	base64Encoding   = "base64"
	sevenBitEncoding = "7bit"
)

// Build builds s into MIME bytes and the SMTP envelope, as spec 5.2 sets out:
// multipart/mixed (when there are attachments) around multipart/related
// (when there are inline images) around multipart/alternative (Markdown and
// HTML bodies) around the text and HTML parts. A Text body is a single
// format=flowed part. Bcc is only in the envelope. An empty MessageID is
// generated from the From domain, and a zero Date is now.
//
// Every problem with s is an error wrapping ErrInvalid.
func Build(s Spec) (Message, error) {
	return build(s, rand.Reader)
}

// build is Build with the randomness behind boundaries, Content-IDs and a
// generated Message-ID taken from rnd, so tests get fixed output.
func build(s Spec, rnd io.Reader) (Message, error) {
	if err := validate(s); err != nil {
		return Message{}, err
	}
	date := s.Date
	if date.IsZero() {
		date = time.Now()
	}
	id := s.MessageID
	if id == "" {
		id = NewMessageID(domain(s.From.Address), date, rnd)
	}
	header, err := topHeader(s, date, id)
	if err != nil {
		return Message{}, err
	}
	related, attached, err := attachmentParts(s)
	if err != nil {
		return Message{}, err
	}
	content, err := contentPart(s, rnd, related)
	if err != nil {
		return Message{}, err
	}
	if len(attached) > 0 {
		content = &part{mediaType: "multipart/mixed", children: append([]*part{content}, attached...)}
	}
	content.setBoundaries(rnd)

	var buf bytes.Buffer
	if err := content.writeRoot(&buf, header); err != nil {
		return Message{}, fmt.Errorf("writing message: %w", err)
	}
	return Message{
		Raw:          buf.Bytes(),
		MessageID:    id,
		EnvelopeFrom: s.From.Address,
		EnvelopeTo:   envelopeTo(s),
	}, nil
}

// NewMessageID returns a Message-ID without angle brackets: ten Crockford
// base32 characters of now in milliseconds, as a ULID starts, so IDs sort by
// time, then a dot, ten random base32 characters from rnd, and @domain. If
// rnd fails, the random characters come from crypto/rand instead.
func NewMessageID(domain string, now time.Time, rnd io.Reader) string {
	var t [10]byte
	ms := uint64(now.UnixMilli())
	for i := len(t) - 1; i >= 0; i-- {
		t[i] = crockford[ms&31]
		ms >>= 5
	}
	return string(t[:]) + "." + randomString(rnd, 10) + "@" + domain
}

// randomString returns n Crockford base32 characters read from rnd, or from
// crypto/rand if rnd fails. 256 is a multiple of 32, so masking a byte to
// five bits keeps every character equally likely.
func randomString(rnd io.Reader, n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rnd, b); err != nil {
		// crypto/rand.Read never fails and always fills b.
		_, _ = rand.Read(b)
	}
	for i := range b {
		b[i] = crockford[b[i]&31]
	}
	return string(b)
}

// validate checks everything about s that does not need rendering.
func validate(s Spec) error {
	if s.From.Address == "" {
		return fmt.Errorf("%w: no From address", ErrInvalid)
	}
	if !validAddress(s.From.Address) {
		return fmt.Errorf("%w: bad From address", ErrInvalid)
	}
	if len(s.To)+len(s.Cc)+len(s.Bcc) == 0 {
		return fmt.Errorf("%w: no recipients", ErrInvalid)
	}
	for _, list := range [][]mail.Address{s.To, s.Cc, s.Bcc} {
		for _, a := range list {
			if !validAddress(a.Address) {
				return fmt.Errorf("%w: bad recipient address", ErrInvalid)
			}
		}
	}
	if s.Body.Kind != Markdown && s.Body.Kind != Text && s.Body.Kind != HTML {
		return fmt.Errorf("%w: unknown body kind %d", ErrInvalid, s.Body.Kind)
	}
	if s.MessageID != "" && !validMsgID(s.MessageID) {
		return fmt.Errorf("%w: bad Message-ID", ErrInvalid)
	}
	if s.InReplyTo != "" && !validMsgID(s.InReplyTo) {
		return fmt.Errorf("%w: bad In-Reply-To", ErrInvalid)
	}
	for _, r := range s.References {
		if !validMsgID(r) {
			return fmt.Errorf("%w: bad References", ErrInvalid)
		}
	}
	for _, f := range s.Attachments {
		if f.ContentID != "" && !ValidContentID(f.ContentID) {
			return fmt.Errorf("%w: bad attachment Content-ID", ErrInvalid)
		}
	}
	return nil
}

// validAddress reports whether a is a bare addr-spec, such as a@example.com,
// with no display name or angle brackets.
func validAddress(a string) bool {
	p, err := mail.ParseAddress(a)
	return err == nil && p.Name == "" && p.Address == a
}

// validMsgID reports whether id can go inside angle brackets in a header: an
// @, and no whitespace, control characters or brackets.
func validMsgID(id string) bool {
	if !strings.Contains(id, "@") {
		return false
	}
	return !strings.ContainsFunc(id, func(r rune) bool {
		return r <= ' ' || r == 0x7f || r == '<' || r == '>'
	})
}

// domain returns the part of addr after the last @.
func domain(addr string) string {
	return addr[strings.LastIndexByte(addr, '@')+1:]
}

// topHeader returns the message header fields, in the order they are
// written, without the Content- fields of the root part.
func topHeader(s Spec, date time.Time, id string) ([][2]string, error) {
	var h gomail.Header
	h.SetAddressList("From", []*mail.Address{&s.From})
	h.SetAddressList("To", pointers(s.To))
	h.SetAddressList("Cc", pointers(s.Cc))
	setText(&h, "Subject", s.Subject)
	h.SetDate(date)
	h.SetMessageID(id)
	if s.InReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{s.InReplyTo})
	}
	h.SetMsgIDList("References", s.References)
	h.Set("MIME-Version", "1.0")
	if s.UserAgent != "" {
		setText(&h, "User-Agent", s.UserAgent)
	}
	extra, err := extraHeaders(&h, s.Headers)
	if err != nil {
		return nil, err
	}
	order := []string{"From", "To", "Cc", "Reply-To", "Subject", "Date", "Message-Id", "In-Reply-To", "References", "Mime-Version", "User-Agent"}
	order = append(order, extra...)
	var fields [][2]string
	for _, k := range order {
		if h.Has(k) {
			fields = append(fields, [2]string{k, h.Get(k)})
		}
	}
	return fields, nil
}

// extraHeaders sets the caller's Reply-To and X- headers on h and returns
// the X- keys in sorted order. Any other name, a bare "X-", a value with a
// line break and two names differing only in case are refused.
func extraHeaders(h *gomail.Header, headers map[string]string) ([]string, error) {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var extra []string
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		v := headers[k]
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if seen[ck] {
			return nil, fmt.Errorf("%w: header %s given twice", ErrInvalid, ck)
		}
		seen[ck] = true
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("%w: header %s has a line break", ErrInvalid, ck)
		}
		if len(ck) > maxFieldName {
			return nil, fmt.Errorf("%w: header name longer than %d bytes", ErrInvalid, maxFieldName)
		}
		switch {
		case ck == "Reply-To":
			addrs, err := mail.ParseAddressList(v)
			if err != nil {
				return nil, fmt.Errorf("%w: bad Reply-To", ErrInvalid)
			}
			h.SetAddressList("Reply-To", addrs)
		case strings.HasPrefix(ck, "X-") && len(ck) > 2 && validFieldName(ck):
			setText(h, ck, v)
			extra = append(extra, ck)
		default:
			return nil, fmt.Errorf("%w: header %q is not allowed", ErrInvalid, k)
		}
	}
	return extra, nil
}

// maxFieldName is the longest header name Spec.Headers may use. go-message
// folds a field's first line at 76 bytes counting the name and ": ", and
// panics when a name leaves no room for any of the value.
const maxFieldName = 64

// validFieldName reports whether k is printable ASCII without a colon, as
// RFC 5322 requires of a field name.
func validFieldName(k string) bool {
	for i := range len(k) {
		if c := k[i]; c < '!' || c > '~' || c == ':' {
			return false
		}
	}
	return true
}

// maxWord is the longest run without whitespace that header folding keeps
// whole. go-message folds at whitespace, and a run longer than a line, about
// 998 bytes less the field name, gets a space forced into it.
const maxWord = 900

// setText sets the unstructured field k to v so it decodes back to v. Most
// values go through go-message, which uses RFC 2047 only for text that is
// not ASCII. A value with a run too long to fold, or with "=?" that a reader
// would decode as an encoded word, is written as encoded words instead.
func setText(h *gomail.Header, k, v string) {
	if !strings.Contains(v, "=?") && !hasLongWord(v) {
		h.SetText(k, v)
		return
	}
	h.Set(k, encodeWords(v))
}

// hasLongWord reports whether s has a run of more than maxWord bytes
// without a space or tab.
func hasLongWord(s string) bool {
	run := 0
	for i := range len(s) {
		if s[i] == ' ' || s[i] == '\t' {
			run = 0
			continue
		}
		if run++; run > maxWord {
			return true
		}
	}
	return false
}

// encodeWords encodes all of s as RFC 2047 Q encoded words of at most 75
// bytes, separated by spaces, which decoders drop between encoded words.
// A word never splits a UTF-8 sequence.
func encodeWords(s string) string {
	const prefix, suffix = "=?utf-8?q?", "?="
	const room = 75 - len(prefix) - len(suffix)
	var b, word strings.Builder
	flush := func() {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(prefix + word.String() + suffix)
		word.Reset()
	}
	for s != "" {
		_, n := utf8.DecodeRuneInString(s)
		enc := qEncode(s[:n])
		if word.Len()+len(enc) > room {
			flush()
		}
		word.WriteString(enc)
		s = s[n:]
	}
	if word.Len() > 0 {
		flush()
	}
	return b.String()
}

// qEncode is RFC 2047 Q encoding of s: a space as "_", printable ASCII but
// "=", "?" and "_" as itself, and every other byte as =XX.
func qEncode(s string) string {
	var b strings.Builder
	for i := range len(s) {
		switch c := s[i]; {
		case c == ' ':
			b.WriteByte('_')
		case c > ' ' && c <= '~' && c != '=' && c != '?' && c != '_':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "=%02X", c)
		}
	}
	return b.String()
}

func pointers(l []mail.Address) []*mail.Address {
	p := make([]*mail.Address, len(l))
	for i := range l {
		p[i] = &l[i]
	}
	return p
}

// envelopeTo returns the To, Cc and Bcc addresses in order, keeping the
// first of any that differ only in case.
func envelopeTo(s Spec) []string {
	var out []string
	seen := make(map[string]bool)
	for _, list := range [][]mail.Address{s.To, s.Cc, s.Bcc} {
		for _, a := range list {
			k := strings.ToLower(a.Address)
			if !seen[k] {
				seen[k] = true
				out = append(out, a.Address)
			}
		}
	}
	return out
}

// attachmentParts are the parts of s.Attachments, in order: related holds
// the inline ones with a Content-ID, which the HTML part can show, and
// attached the rest, which go in multipart/mixed. A Text body has no HTML
// part, so all of its attachments are attached.
func attachmentParts(s Spec) (related, attached []*part, err error) {
	for _, f := range s.Attachments {
		disposition := "attachment"
		if f.Inline {
			disposition = "inline"
		}
		p, err := filePart(f, disposition)
		if err != nil {
			return nil, nil, err
		}
		p.contentID = f.ContentID
		if f.Inline && f.ContentID != "" && s.Body.Kind != Text {
			related = append(related, p)
		} else {
			attached = append(attached, p)
		}
	}
	return related, attached, nil
}

// ValidContentID reports whether id can be written as a Content-ID between
// angle brackets: printable ASCII without spaces or angle brackets.
func ValidContentID(id string) bool {
	return id != "" && len(id) <= maxContentID && !strings.ContainsFunc(id, func(r rune) bool {
		return r <= ' ' || r >= 0x7f || r == '<' || r == '>'
	})
}

// maxContentID caps a Content-ID, so its header line stays well under 998
// bytes.
const maxContentID = 900

// contentPart returns the body: the text part, or multipart/alternative of
// text and HTML, inside multipart/related with the inline images if any.
// related are attachment parts the HTML shows, added after the images.
func contentPart(s Spec, rnd io.Reader, related []*part) (*part, error) {
	if s.Body.Kind == Text {
		if len(s.Inline) > 0 {
			return nil, fmt.Errorf("%w: a text body cannot show inline images", ErrInvalid)
		}
		return textPart(flowed(s.Body.Source, s.Signature, s.Quote), "flowed"), nil
	}
	text, bodyHTML, err := bodies(s)
	if err != nil {
		return nil, err
	}
	sigHTML, err := signatureHTML(s.Signature)
	if err != nil {
		return nil, err
	}
	cids, images, err := inlineImages(s.Inline, rnd)
	if err != nil {
		return nil, err
	}
	images = append(images, related...)
	used := make(map[string]bool, len(cids))
	bodyHTML = rewriteImages(bodyHTML, cids, used)
	sigHTML = rewriteImages(sigHTML, cids, used)
	for _, name := range sortedKeys(s.Inline) {
		if !used[name] {
			return nil, fmt.Errorf("%w: inline image %q is not used in the body", ErrInvalid, name)
		}
	}
	extra := sigHTML + quoteHTML(s.Quote)
	var doc string
	if s.Body.Kind == Markdown {
		doc = htmlHead + bodyHTML + extra + htmlTail
	} else {
		doc = insertBeforeBodyEnd(bodyHTML, extra)
	}
	alt := &part{mediaType: "multipart/alternative", children: []*part{
		textPart(plainText(text, s.Signature, s.Quote), ""),
		{mediaType: "text/html", params: map[string]string{"charset": "utf-8"}, encoding: quotedPrintable, body: []byte(doc)},
	}}
	if len(images) == 0 {
		return alt, nil
	}
	return &part{
		mediaType: "multipart/related",
		params:    map[string]string{"type": alt.mediaType},
		children:  append([]*part{alt}, images...),
	}, nil
}

// bodies returns the text part's body and the HTML of a Markdown or HTML
// body, before the signature and quote.
func bodies(s Spec) (text, htm string, err error) {
	if s.Body.Kind == Markdown {
		htm, err = RenderMarkdown(s.Body.Source)
		return s.Body.Source, htm, err
	}
	text = s.Body.HTMLText
	if text == "" {
		if text, err = render.HTMLToText(strings.NewReader(s.Body.Source)); err != nil {
			return "", "", fmt.Errorf("deriving the text part: %w", err)
		}
	}
	return text, s.Body.Source, nil
}

func textPart(body, format string) *part {
	params := map[string]string{"charset": "utf-8"}
	if format != "" {
		params["format"] = format
	}
	return &part{mediaType: "text/plain", params: params, encoding: quotedPrintable, body: []byte(body)}
}

// plainText is the text part: the body, the signature after "-- ", then the
// quote, each separated by a blank line.
func plainText(body, sig string, q *Quote) string {
	var b strings.Builder
	b.WriteString(lf(body))
	if hasSignature(sig) {
		b.WriteString(paragraphBreak(b.String()))
		b.WriteString("-- \n")
		b.WriteString(withNewline(lf(sig)))
	}
	if q != nil {
		b.WriteString(paragraphBreak(b.String()))
		b.WriteString(withNewline(lf(q.Header)))
		if q.Forward {
			b.WriteString("\n")
			b.WriteString(lf(q.Text))
		} else {
			b.WriteString(quoteLines(q.Text))
		}
	}
	return b.String()
}

// flowed is plainText made safe for format=flowed (RFC 3676). A trailing
// space marks a soft break there, so trailing spaces are dropped, except on
// the "-- " signature separator. Lines starting with a space, ">" or "From "
// are space-stuffed so they are not read as stuffed or quoted. The "> "
// lines of a reply quote are left as quotes.
func flowed(body, sig string, q *Quote) string {
	var b strings.Builder
	b.WriteString(stuff(lf(body)))
	if hasSignature(sig) {
		b.WriteString(paragraphBreak(b.String()))
		b.WriteString("-- \n")
		b.WriteString(withNewline(stuff(lf(sig))))
	}
	if q != nil {
		b.WriteString(paragraphBreak(b.String()))
		b.WriteString(withNewline(stuff(lf(q.Header))))
		if q.Forward {
			b.WriteString("\n")
			b.WriteString(stuff(lf(q.Text)))
		} else {
			b.WriteString(trimLines(quoteLines(q.Text)))
		}
	}
	return b.String()
}

// stuff trims trailing spaces and space-stuffs each line of s, for flowed.
func stuff(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l == "-- " {
			continue
		}
		l = strings.TrimRight(l, " \t")
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, ">") || strings.HasPrefix(l, "From ") {
			l = " " + l
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

// trimLines drops trailing spaces from each line of s.
func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}

// quoteLines prefixes each line of s with "> ", or ">" for a blank line or
// one already quoted, so nested quotes read ">>".
func quoteLines(s string) string {
	s = strings.TrimSuffix(lf(s), "\n")
	var b strings.Builder
	for l := range strings.SplitSeq(s, "\n") {
		switch {
		case l == "", strings.HasPrefix(l, ">"):
			b.WriteString(">")
		default:
			b.WriteString("> ")
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

func hasSignature(sig string) bool { return strings.TrimSpace(sig) != "" }

// lf converts CRLF and lone CR line endings to LF.
func lf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func withNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// paragraphBreak returns what to add after sofar so a blank line follows it.
func paragraphBreak(sofar string) string {
	switch {
	case sofar == "":
		return ""
	case strings.HasSuffix(sofar, "\n\n"):
		return ""
	case strings.HasSuffix(sofar, "\n"):
		return "\n"
	default:
		return "\n\n"
	}
}

// signatureHTML renders the Markdown signature into a div, or returns "".
func signatureHTML(sig string) (string, error) {
	if !hasSignature(sig) {
		return "", nil
	}
	r, err := RenderMarkdown(sig)
	if err != nil {
		return "", err
	}
	return "<div class=\"signature\">-- <br>\n" + r + "</div>\n", nil
}

// quoteHTML is the quote under the signature: the attribution or forward
// header, then the original's HTML, or its text in a pre when it has none.
// A reply's original is in a cite blockquote, a forward's is not.
func quoteHTML(q *Quote) string {
	if q == nil {
		return ""
	}
	header := strings.ReplaceAll(html.EscapeString(strings.TrimSuffix(lf(q.Header), "\n")), "\n", "<br>\n")
	content := q.HTML
	if content == "" {
		content = "<pre>" + html.EscapeString(lf(q.Text)) + "</pre>"
	}
	if !q.Forward {
		content = "<blockquote type=\"cite\">\n" + content + "\n</blockquote>"
	}
	return "<div class=\"quote\">\n<div class=\"quote-header\">" + header + "</div>\n" + content + "\n</div>\n"
}

// insertBeforeBodyEnd puts extra before the last </body> end tag of doc, or
// at the end when there is none. The HTML tokenizer finds the tag, so a
// "</body>" in a comment, script, attribute or other raw text is not one.
// doc is otherwise left byte for byte as given.
func insertBeforeBodyEnd(doc, extra string) string {
	z := nethtml.NewTokenizer(strings.NewReader(doc))
	at, offset := -1, 0
	for {
		tt := z.Next()
		if tt == nethtml.ErrorToken {
			// io.EOF: the tokenizer reads from a string, so nothing else can fail.
			break
		}
		if tt == nethtml.EndTagToken {
			if name, _ := z.TagName(); string(name) == "body" {
				at = offset
			}
		}
		offset += len(z.Raw())
	}
	if at < 0 {
		return doc + extra
	}
	return doc[:at] + extra + doc[at:]
}

// inlineImages gives each inline image a Content-ID, in sorted name order,
// and returns the Content-IDs by name and the image parts.
func inlineImages(inline map[string]File, rnd io.Reader) (map[string]string, []*part, error) {
	cids := make(map[string]string, len(inline))
	var parts []*part
	for _, name := range sortedKeys(inline) {
		p, err := filePart(inline[name], "inline")
		if err != nil {
			return nil, nil, err
		}
		if !strings.HasPrefix(p.mediaType, "image/") {
			return nil, nil, fmt.Errorf("%w: inline %q is %s, not an image", ErrInvalid, name, p.mediaType)
		}
		cid := randomString(rnd, 16) + "@chainmail"
		p.contentID = cid
		cids[name] = cid
		parts = append(parts, p)
	}
	return cids, parts, nil
}

// rewriteImages points every img whose src names an inline image at its
// Content-ID and records the names it used. The src may be the name as
// given or percent-encoded, as goldmark writes it. Everything but the
// rewritten img tags is copied byte for byte.
func rewriteImages(s string, cids map[string]string, used map[string]bool) string {
	if len(cids) == 0 || s == "" {
		return s
	}
	z := nethtml.NewTokenizer(strings.NewReader(s))
	var out bytes.Buffer
	for {
		tt := z.Next()
		if tt == nethtml.ErrorToken {
			// io.EOF: the tokenizer reads from a string, so nothing else can fail.
			return out.String()
		}
		if tt != nethtml.StartTagToken && tt != nethtml.SelfClosingTagToken {
			out.Write(z.Raw())
			continue
		}
		// Token unescapes in place, which changes what Raw returned.
		raw := bytes.Clone(z.Raw())
		tok := z.Token()
		changed := false
		if tok.Data == "img" {
			for i, a := range tok.Attr {
				if a.Key != "src" {
					continue
				}
				if name, ok := inlineName(a.Val, cids); ok {
					tok.Attr[i].Val = "cid:" + cids[name]
					used[name] = true
					changed = true
				}
			}
		}
		if changed {
			out.WriteString(tok.String())
		} else {
			out.Write(raw)
		}
	}
}

// inlineName returns the inline image name src refers to.
func inlineName(src string, cids map[string]string) (string, bool) {
	if _, ok := cids[src]; ok {
		return src, true
	}
	if u, err := url.PathUnescape(src); err == nil {
		if _, ok := cids[u]; ok {
			return u, true
		}
	}
	return "", false
}

func sortedKeys(m map[string]File) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// CheckFile reports whether Build can attach f: it returns the error Build
// gives for f as an attachment, or nil. A forward checks the original's
// parts with it, so it can refuse one with text of its own.
func CheckFile(f File) error {
	_, err := filePart(f, "attachment")
	return err
}

// filePart is an attachment or inline image. Its media type is the declared
// one, or sniffed from the data when none is declared. It is base64 encoded,
// except for the composite types compositePart handles.
func filePart(f File, disposition string) (*part, error) {
	ct := f.ContentType
	if ct == "" {
		ct = http.DetectContentType(f.Data)
	}
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, fmt.Errorf("%w: an attachment has a bad content type", ErrInvalid)
	}
	p := &part{
		mediaType:   mt,
		params:      params,
		encoding:    base64Encoding,
		disposition: disposition,
		filename:    f.Filename,
		body:        f.Data,
	}
	if strings.HasPrefix(mt, "multipart/") || strings.HasPrefix(mt, "message/") && mt != "message/global" {
		return compositePart(p)
	}
	return p, nil
}

// compositePart sets the encoding of a multipart or message attachment.
// RFC 2046 allows only 7bit, 8bit or binary for these, and the sender does
// not ask for 8BITMIME, so data that is 7-bit goes as 7bit, with its line
// endings made CRLF. An attached email that is not 7-bit goes as
// message/global, which RFC 6532 lets be base64 encoded. Any other composite
// that is not 7-bit, and a multipart type without a boundary, is refused.
// The errors do not name the media type: a forward re-attaches the
// original's parts with their declared types, which the original's sender
// chose, and the error's text reaches clients.
func compositePart(p *part) (*part, error) {
	if strings.HasPrefix(p.mediaType, "multipart/") && p.params["boundary"] == "" {
		return nil, fmt.Errorf("%w: a multipart attachment has no boundary", ErrInvalid)
	}
	if data, ok := sevenBit(p.body); ok {
		p.encoding, p.body = sevenBitEncoding, data
		return p, nil
	}
	if p.mediaType == "message/rfc822" {
		p.mediaType = "message/global"
		return p, nil
	}
	return nil, fmt.Errorf("%w: a multipart or message attachment is not 7-bit data", ErrInvalid)
}

// sevenBit returns data with CRLF line endings, and whether it is 7bit data
// as RFC 2045 defines it: no NUL, no byte over 127 and no line over 998
// bytes.
func sevenBit(data []byte) ([]byte, bool) {
	lines := strings.Split(lf(string(data)), "\n")
	for _, l := range lines {
		if len(l) > 998 || strings.ContainsFunc(l, func(r rune) bool { return r == 0 || r > 127 }) {
			return nil, false
		}
	}
	return []byte(strings.Join(lines, "\r\n")), true
}

// part is one MIME entity of the message being built.
type part struct {
	mediaType   string
	params      map[string]string
	encoding    string
	disposition string
	filename    string
	contentID   string
	body        []byte
	children    []*part
}

// setBoundaries gives each multipart entity a random boundary, depth first.
func (p *part) setBoundaries(rnd io.Reader) {
	if len(p.children) == 0 {
		return
	}
	if p.params == nil {
		p.params = map[string]string{}
	}
	p.params["boundary"] = randomString(rnd, 28)
	for _, c := range p.children {
		c.setBoundaries(rnd)
	}
}

// fields are the part's Content- header fields, in the order written.
func (p *part) fields() [][2]string {
	f := [][2]string{{"Content-Type", mime.FormatMediaType(p.mediaType, p.params)}}
	// A multipart entity gets no Content-Transfer-Encoding field, as
	// go-message leaves it out, even an attached multipart sent as 7bit.
	if p.encoding != "" && !strings.HasPrefix(p.mediaType, "multipart/") {
		f = append(f, [2]string{"Content-Transfer-Encoding", p.encoding})
	}
	if p.disposition != "" {
		f = append(f, [2]string{"Content-Disposition", dispositionValue(p.disposition, p.filename)})
	}
	if p.contentID != "" {
		f = append(f, [2]string{"Content-Id", "<" + p.contentID + ">"})
	}
	return f
}

// header returns a go-message header writing fields in order. go-message
// writes the last field added first, so they are added in reverse.
func header(fields [][2]string) message.Header {
	var h message.Header
	for _, f := range slices.Backward(fields) {
		h.Add(f[0], f[1])
	}
	return h
}

// writeRoot writes p as the whole message with top above its own fields.
func (p *part) writeRoot(w io.Writer, top [][2]string) error {
	if err := mimetext.WriteHeader(w, header(append(top, p.fields()...)).Header); err != nil {
		return err
	}
	return p.writeBody(w)
}

// writeBody writes p's body, encoded, or its children. The parts are
// written here rather than by go-message's Writer, which refuses a part
// whose charset is not UTF-8, such as a forwarded windows-1252 text
// attachment. Its bytes go as they are, under its own charset.
func (p *part) writeBody(w io.Writer) error {
	if len(p.children) == 0 {
		return writeEncoded(w, p.encoding, p.body)
	}
	mw := mimetext.NewMultipartWriter(w)
	if err := mw.SetBoundary(p.params["boundary"]); err != nil {
		return err
	}
	for _, c := range p.children {
		cw, err := mw.CreatePart(header(c.fields()).Header)
		if err != nil {
			return err
		}
		if err := c.writeBody(cw); err != nil {
			return err
		}
	}
	return mw.Close()
}

// base64Line is the length of a base64 line, as RFC 2045 allows at most 76.
const base64Line = 76

// writeEncoded writes body in encoding: quoted-printable, base64 in lines of
// base64Line, or as it is for 7bit, whose lines are CRLF and short already.
func writeEncoded(w io.Writer, encoding string, body []byte) error {
	switch encoding {
	case quotedPrintable:
		qw := quotedprintable.NewWriter(w)
		if _, err := qw.Write(body); err != nil {
			return err
		}
		return qw.Close()
	case base64Encoding:
		enc := base64.StdEncoding.EncodeToString(body)
		var b strings.Builder
		for len(enc) > base64Line {
			b.WriteString(enc[:base64Line])
			b.WriteString("\r\n")
			enc = enc[base64Line:]
		}
		b.WriteString(enc)
		_, err := io.WriteString(w, b.String())
		return err
	default:
		_, err := w.Write(body)
		return err
	}
}

// dispositionValue formats a Content-Disposition with filename per RFC 2183
// and RFC 2231. A short name is written as mime.FormatMediaType writes it,
// RFC 2231 encoded when it is not ASCII. A long one is split into RFC 2231
// continuations so header folding can keep every line under 998 bytes.
func dispositionValue(disp, filename string) string {
	if filename == "" {
		return disp
	}
	const maxPlain = 60
	if v := mime.FormatMediaType(disp, map[string]string{"filename": filename}); v != "" && len(v) <= len(disp)+maxPlain {
		return v
	}
	enc := percentEncode(filename)
	var b strings.Builder
	b.WriteString(disp)
	for i := 0; enc != ""; i++ {
		n := min(len(enc), maxPlain)
		// Do not split a %XX escape.
		if j := strings.LastIndexByte(enc[:n], '%'); j >= 0 && j > n-3 && n < len(enc) {
			n = j
		}
		fmt.Fprintf(&b, "; filename*%d*=", i)
		if i == 0 {
			b.WriteString("utf-8''")
		}
		b.WriteString(enc[:n])
		enc = enc[n:]
	}
	return b.String()
}

// percentEncode encodes s as an RFC 2231 extended value: attribute
// characters as they are, every other byte as %XX.
func percentEncode(s string) string {
	const attrChars = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(attrChars, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
