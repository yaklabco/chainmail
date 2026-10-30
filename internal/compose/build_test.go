package compose

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"

	"github.com/jamesainslie/chainmail/internal/render"
)

var update = flag.Bool("update", false, "rewrite golden files")

var testDate = time.Date(2026, time.October, 30, 9, 15, 0, 0, time.FixedZone("", -4*60*60))

// pngData is the 8-byte PNG signature, enough for sniffing to say image/png.
var pngData = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

// seqReader returns 0, 1, 2, ... so boundaries and Content-IDs are fixed.
type seqReader struct{ n byte }

func (r *seqReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.n
		r.n++
	}
	return len(p), nil
}

func baseSpec() Spec {
	return Spec{
		From:      mail.Address{Name: "James Ainslie", Address: "james@example.com"},
		To:        []mail.Address{{Name: "Ann Smith", Address: "ann@example.org"}},
		Subject:   "Hello",
		Body:      Body{Kind: Markdown, Source: "Hello **world**.\n"},
		Date:      testDate,
		MessageID: "01JBX7Y8Z9.ABCDEFGHJK@example.com",
		UserAgent: "chainmail/test",
	}
}

func replyQuote() *Quote {
	return &Quote{
		Header: "On Thu, 29 Oct 2026 at 10:00, Ann Smith <ann@example.org> wrote:",
		Text:   "Are we on for Thursday?\n\n> Earlier note\n",
		HTML:   "<p>Are we on for Thursday?</p>",
	}
}

func forwardQuote() *Quote {
	return &Quote{
		Header:  "---------- Forwarded message ---------\nFrom: Ann Smith <ann@example.org>\nDate: Thu, 29 Oct 2026 10:00:00 -0400\nSubject: Plans\nTo: James Ainslie <james@example.com>",
		Text:    "The plans are attached.\n",
		HTML:    "<p>The plans are attached.</p>",
		Forward: true,
	}
}

func mustBuild(t testing.TB, s Spec) Message {
	t.Helper()
	m, err := build(s, &seqReader{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return m
}

// node is a parsed MIME entity with its body decoded and line endings as LF.
type node struct {
	mediaType string
	params    map[string]string
	header    message.Header
	body      string
	raw       string
	parts     []*node
}

func parse(t testing.TB, raw []byte) *node {
	t.Helper()
	e, err := message.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parsing built message: %v", err)
	}
	n, err := parseEntity(e)
	if err != nil {
		t.Fatalf("parsing built message: %v", err)
	}
	return n
}

func parseEntity(e *message.Entity) (*node, error) {
	mt, params, err := e.Header.ContentType()
	if err != nil {
		return nil, err
	}
	n := &node{mediaType: mt, params: params, header: e.Header}
	if mr := e.MultipartReader(); mr != nil {
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return n, nil
			}
			if err != nil {
				return nil, err
			}
			c, err := parseEntity(p)
			if err != nil {
				return nil, err
			}
			n.parts = append(n.parts, c)
		}
	}
	b, err := io.ReadAll(e.Body)
	if err != nil {
		return nil, err
	}
	n.raw = string(b)
	n.body = strings.ReplaceAll(n.raw, "\r\n", "\n")
	return n, nil
}

// shape is the tree of media types, as "multipart/mixed[text/plain,image/png]".
func (n *node) shape() string {
	if len(n.parts) == 0 {
		return n.mediaType
	}
	kids := make([]string, len(n.parts))
	for i, p := range n.parts {
		kids[i] = p.shape()
	}
	return n.mediaType + "[" + strings.Join(kids, ",") + "]"
}

// find returns the first leaf with media type mt, depth first.
func (n *node) find(mt string) *node {
	if len(n.parts) == 0 && n.mediaType == mt {
		return n
	}
	for _, p := range n.parts {
		if f := p.find(mt); f != nil {
			return f
		}
	}
	return nil
}

func TestBuildGolden(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*Spec)
	}{
		{"markdown", func(*Spec) {}},
		{"text", func(s *Spec) { s.Body = Body{Kind: Text, Source: "Hello world.\nSecond line.\n"} }},
		{"html", func(s *Spec) { s.Body = Body{Kind: HTML, Source: "<p>Hello <b>world</b></p>"} }},
		{"html-text", func(s *Spec) {
			s.Body = Body{Kind: HTML, Source: "<p>Hello <b>world</b></p>", HTMLText: "Hello WORLD"}
		}},
		{"inline-markdown", func(s *Spec) {
			s.Body.Source = "See the chart:\n\n![chart](chart.png)\n"
			s.Inline = map[string]File{"chart.png": {Filename: "chart.png", ContentType: "image/png", Data: pngData}}
		}},
		{"inline-html", func(s *Spec) {
			s.Body = Body{Kind: HTML, Source: `<p>See <img src="chart.png" alt="chart"></p>`}
			s.Inline = map[string]File{"chart.png": {Filename: "chart.png", ContentType: "image/png", Data: pngData}}
		}},
		{"attachments", func(s *Spec) {
			s.Attachments = []File{
				{Filename: "report.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4\n")},
				{Filename: "résumé.txt", ContentType: "text/plain; charset=utf-8", Data: []byte("CV\n")},
			}
		}},
		{"non-ascii", func(s *Spec) {
			s.Subject = "Grüße aus Zürich"
			s.From.Name = "Jörg Müller"
		}},
		{"signature", func(s *Spec) { s.Signature = "James\n*chainmail*" }},
		{"reply", func(s *Spec) {
			s.Subject = "Re: Thursday"
			s.Signature = "James"
			s.InReplyTo = "orig@example.org"
			s.References = []string{"first@example.org", "orig@example.org"}
			s.Quote = replyQuote()
		}},
		{"forward", func(s *Spec) {
			s.Subject = "Fwd: Plans"
			s.Quote = forwardQuote()
		}},
		{"quote-pre", func(s *Spec) {
			q := replyQuote()
			q.HTML = ""
			q.Text = "if a < b && c > d\n"
			s.Quote = q
		}},
		{"headers", func(s *Spec) {
			s.Cc = []mail.Address{{Address: "cc@example.org"}}
			s.Bcc = []mail.Address{{Address: "hidden@example.org"}}
			s.Headers = map[string]string{"X-Foo": "bar", "Reply-To": "Desk <desk@example.com>"}
		}},
		{"text-attachment", func(s *Spec) {
			s.Body = Body{Kind: Text, Source: "See attached.\n"}
			s.Attachments = []File{{Filename: "a.bin", Data: []byte{0, 1, 2, 3}}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := baseSpec()
			tt.edit(&s)
			raw := mustBuild(t, s).Raw
			parse(t, raw)
			assertCRLF(t, raw)
			// Goldens are stored with LF line endings, as git may convert CRLF.
			got := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
			path := filepath.Join("testdata", tt.name+".eml")
			if *update {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("built message differs from %s\n got:\n%s\nwant:\n%s", path, got, want)
			}
		})
	}
}

func TestBuildShape(t *testing.T) {
	t.Parallel()
	img := File{ContentType: "image/png", Data: pngData}
	att := File{Filename: "a.txt", ContentType: "text/plain", Data: []byte("a")}
	tests := []struct {
		name string
		edit func(*Spec)
		want string
	}{
		{"markdown", func(*Spec) {}, "multipart/alternative[text/plain,text/html]"},
		{"text", func(s *Spec) { s.Body = Body{Kind: Text, Source: "hi"} }, "text/plain"},
		{"html", func(s *Spec) { s.Body = Body{Kind: HTML, Source: "<p>hi</p>"} }, "multipart/alternative[text/plain,text/html]"},
		{"text with attachment", func(s *Spec) {
			s.Body = Body{Kind: Text, Source: "hi"}
			s.Attachments = []File{att}
		}, "multipart/mixed[text/plain,text/plain]"},
		{"inline", func(s *Spec) {
			s.Body.Source = "![x](x.png)"
			s.Inline = map[string]File{"x.png": img}
		}, "multipart/related[multipart/alternative[text/plain,text/html],image/png]"},
		{"inline and attachments", func(s *Spec) {
			s.Body.Source = "![x](x.png) ![y](y.png)"
			s.Inline = map[string]File{"y.png": img, "x.png": img}
			s.Attachments = []File{att, att}
		}, "multipart/mixed[multipart/related[multipart/alternative[text/plain,text/html],image/png,image/png],text/plain,text/plain]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := baseSpec()
			tt.edit(&s)
			if got := parse(t, mustBuild(t, s).Raw).shape(); got != tt.want {
				t.Errorf("shape = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestBuildMarkdown(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.Body.Source = "Hello **world**.\n\n> quoted\n\n`code`\n"
	root := parse(t, mustBuild(t, s).Raw)
	text, htm := root.find("text/plain"), root.find("text/html")
	if text == nil || htm == nil {
		t.Fatalf("missing parts in %s", root.shape())
	}
	if text.body != s.Body.Source {
		t.Errorf("text part = %q, want the Markdown source", text.body)
	}
	if text.params["charset"] != "utf-8" || text.params["format"] != "" {
		t.Errorf("text params = %v", text.params)
	}
	for _, want := range []string{"<!DOCTYPE html>", "<style>", "font-family", "blockquote", "background", "<strong>world</strong>", "<code>code</code>", "</html>"} {
		if !strings.Contains(htm.body, want) {
			t.Errorf("HTML part lacks %q:\n%s", want, htm.body)
		}
	}
	if htm.params["charset"] != "utf-8" {
		t.Errorf("html params = %v", htm.params)
	}
}

func TestBuildText(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.Body = Body{Kind: Text, Source: "Hello world.\nSecond line.\n"}
	m := mustBuild(t, s)
	root := parse(t, m.Raw)
	if root.shape() != "text/plain" {
		t.Fatalf("shape = %s", root.shape())
	}
	if !bytes.Contains(m.Raw, []byte("Content-Type: text/plain; charset=utf-8; format=flowed\r\n")) {
		t.Errorf("Content-Type not flowed utf-8:\n%s", m.Raw)
	}
	if !bytes.Contains(m.Raw, []byte("Content-Transfer-Encoding: quoted-printable\r\n")) {
		t.Errorf("not quoted-printable:\n%s", m.Raw)
	}
	if root.body != s.Body.Source {
		t.Errorf("body = %q, want %q", root.body, s.Body.Source)
	}
}

// TestBuildTextFlowed checks format=flowed stays faithful: trailing spaces,
// which would join lines, are dropped, and lines a reader would take as
// quoted or stuffed are space-stuffed.
func TestBuildTextFlowed(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.Body = Body{Kind: Text, Source: "keep  \n>not a quote\n leading\nFrom here\nplain\n"}
	got := parse(t, mustBuild(t, s).Raw).body
	want := "keep\n >not a quote\n  leading\n From here\nplain\n"
	if got != want {
		t.Errorf("flowed body = %q, want %q", got, want)
	}
}

func TestBuildHTML(t *testing.T) {
	t.Parallel()
	const src = "<p>Hello <b>world</b></p><ul><li>one</li></ul>"
	derived, err := render.HTMLToText(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, htmlText, want string
	}{
		{"given text", "Hello WORLD", "Hello WORLD"},
		{"derived text", "", derived},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := baseSpec()
			s.Body = Body{Kind: HTML, Source: src, HTMLText: tt.htmlText}
			root := parse(t, mustBuild(t, s).Raw)
			if got := root.find("text/plain").body; got != tt.want {
				t.Errorf("text part = %q, want %q", got, tt.want)
			}
			if got := root.find("text/html").body; got != src {
				t.Errorf("html part = %q, want it as given", got)
			}
		})
	}
}

func TestBuildHTMLInsertsBeforeBody(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.Body = Body{Kind: HTML, Source: "<html><body><p>hi</p></BODY></html>"}
	s.Signature = "James"
	got := parse(t, mustBuild(t, s).Raw).find("text/html").body
	sig := strings.Index(got, `<div class="signature">`)
	end := strings.Index(got, "</BODY>")
	if sig < 0 || end < 0 || sig > end {
		t.Errorf("signature not inside the body:\n%s", got)
	}
}

func TestBuildInline(t *testing.T) {
	t.Parallel()
	img := File{Filename: "chart.png", ContentType: "image/png", Data: pngData}
	tests := []struct {
		name string
		body Body
	}{
		{"markdown", Body{Kind: Markdown, Source: "![chart](chart.png)\n"}},
		{"html", Body{Kind: HTML, Source: `<p><img alt="chart" src="chart.png"/></p>`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := baseSpec()
			s.Body = tt.body
			s.Inline = map[string]File{"chart.png": img}
			root := parse(t, mustBuild(t, s).Raw)
			if root.mediaType != "multipart/related" || root.params["type"] != "multipart/alternative" {
				t.Fatalf("root = %s %v", root.shape(), root.params)
			}
			part := root.find("image/png")
			cid := part.header.Get("Content-Id")
			if !regexp.MustCompile(`^<[0-9A-Z]+@chainmail>$`).MatchString(cid) {
				t.Fatalf("Content-ID = %q", cid)
			}
			disp, params, err := part.header.ContentDisposition()
			if err != nil || disp != "inline" || params["filename"] != "chart.png" {
				t.Errorf("disposition = %q %v %v", disp, params, err)
			}
			if part.raw != string(pngData) {
				t.Errorf("image data changed")
			}
			htm := root.find("text/html").body
			want := `src="cid:` + strings.Trim(cid, "<>") + `"`
			if !strings.Contains(htm, want) || strings.Contains(htm, `src="chart.png"`) {
				t.Errorf("HTML lacks %s:\n%s", want, htm)
			}
		})
	}
}

func TestBuildInlineSniffed(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.Body.Source = "![x](x)"
	s.Inline = map[string]File{"x": {Data: pngData}}
	if root := parse(t, mustBuild(t, s).Raw); root.find("image/png") == nil {
		t.Errorf("sniffed type missing: %s", root.shape())
	}
}

func TestBuildInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*Spec)
	}{
		{"missing from", func(s *Spec) { s.From = mail.Address{} }},
		{"bad from", func(s *Spec) { s.From.Address = "not an address" }},
		{"no recipients", func(s *Spec) { s.To = nil }},
		{"bad recipient", func(s *Spec) { s.To = []mail.Address{{Address: "Ann <ann@example.org>"}} }},
		{"bad kind", func(s *Spec) { s.Body.Kind = BodyKind(9) }},
		{"unreferenced inline", func(s *Spec) {
			s.Inline = map[string]File{"chart.png": {ContentType: "image/png", Data: pngData}}
		}},
		{"inline in a text body", func(s *Spec) {
			s.Body = Body{Kind: Text, Source: "chart.png"}
			s.Inline = map[string]File{"chart.png": {ContentType: "image/png", Data: pngData}}
		}},
		{"non-image inline", func(s *Spec) {
			s.Body.Source = "![x](notes.txt)"
			s.Inline = map[string]File{"notes.txt": {ContentType: "text/plain", Data: []byte("x")}}
		}},
		{"bad content type", func(s *Spec) {
			s.Attachments = []File{{Filename: "a", ContentType: "not a type;;", Data: []byte("x")}}
		}},
		{"subject header", func(s *Spec) { s.Headers = map[string]string{"Subject": "sneaky"} }},
		{"bcc header", func(s *Spec) { s.Headers = map[string]string{"Bcc": "a@example.org"} }},
		{"bare x- header", func(s *Spec) { s.Headers = map[string]string{"X-": "v"} }},
		{"header name with space", func(s *Spec) { s.Headers = map[string]string{"X-A B": "v"} }},
		{"header value with newline", func(s *Spec) { s.Headers = map[string]string{"X-Foo": "a\r\nBcc: x@example.org"} }},
		{"duplicate header", func(s *Spec) { s.Headers = map[string]string{"X-Foo": "a", "x-foo": "b"} }},
		{"bad reply-to", func(s *Spec) { s.Headers = map[string]string{"Reply-To": "nobody"} }},
		{"bad message id", func(s *Spec) { s.MessageID = "a b@example.com" }},
		{"bad in-reply-to", func(s *Spec) { s.InReplyTo = "<x@example.com>" }},
		{"bad reference", func(s *Spec) { s.References = []string{"x@y\r\nBcc: z"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := baseSpec()
			tt.edit(&s)
			_, err := build(s, &seqReader{})
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestBuildAttachments(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("ñ", 120) + ".txt"
	s := baseSpec()
	s.Attachments = []File{
		{Filename: "report.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4\n")},
		{Filename: "résumé.txt", ContentType: "text/plain; charset=utf-8", Data: []byte("CV\n")},
		{Filename: long, Data: []byte("plain text, sniffed\n")},
	}
	m := mustBuild(t, s)
	if !bytes.Contains(m.Raw, []byte("filename*=utf-8''r%C3%A9sum%C3%A9.txt")) {
		t.Errorf("non-ASCII filename not RFC 2231 encoded:\n%s", m.Raw)
	}
	root := parse(t, m.Raw)
	if root.mediaType != "multipart/mixed" || len(root.parts) != 4 {
		t.Fatalf("shape = %s", root.shape())
	}
	for i, f := range s.Attachments {
		p := root.parts[i+1]
		disp, params, err := p.header.ContentDisposition()
		if err != nil || disp != "attachment" || params["filename"] != f.Filename {
			t.Errorf("attachment %d disposition = %q %q %v", i, disp, params["filename"], err)
		}
		if p.header.Get("Content-Transfer-Encoding") != "base64" {
			t.Errorf("attachment %d not base64", i)
		}
		if p.raw != string(f.Data) {
			t.Errorf("attachment %d data = %q", i, p.body)
		}
	}
	if root.parts[3].mediaType != "text/plain" {
		t.Errorf("sniffed type = %s", root.parts[3].mediaType)
	}
}

func TestBuildNonASCIIHeaders(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.Subject = "Grüße aus Zürich"
	s.From.Name = "Jörg Müller"
	s.To = []mail.Address{{Name: "Zoë, Team", Address: "zoe@example.org"}}
	m := mustBuild(t, s)
	if bytes.Contains(m.Raw, []byte("Grüße")) || bytes.Contains(m.Raw, []byte("Jörg")) {
		t.Errorf("raw header has unencoded text:\n%s", m.Raw)
	}
	h := gomail.Header{Header: parse(t, m.Raw).header}
	if subj, err := h.Subject(); err != nil || subj != s.Subject {
		t.Errorf("subject = %q, %v", subj, err)
	}
	from, err := h.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Name != s.From.Name {
		t.Errorf("from = %v, %v", from, err)
	}
	to, err := h.AddressList("To")
	if err != nil || len(to) != 1 || to[0].Name != "Zoë, Team" {
		t.Errorf("to = %v, %v", to, err)
	}
}

func TestBuildHeaderFields(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.Cc = []mail.Address{{Address: "cc@example.org"}}
	s.InReplyTo = "orig@example.org"
	s.References = []string{"first@example.org", "orig@example.org"}
	s.Headers = map[string]string{"X-Foo": "bar", "reply-to": "Desk <desk@example.com>", "X-Note": "naïve"}
	m := mustBuild(t, s)
	h := gomail.Header{Header: parse(t, m.Raw).header}
	checks := map[string]string{
		"Message-Id":   "<01JBX7Y8Z9.ABCDEFGHJK@example.com>",
		"Date":         "Fri, 30 Oct 2026 09:15:00 -0400",
		"Mime-Version": "1.0",
		"User-Agent":   "chainmail/test",
		"In-Reply-To":  "<orig@example.org>",
		"References":   "<first@example.org> <orig@example.org>",
		"X-Foo":        "bar",
		"Reply-To":     `"Desk" <desk@example.com>`,
		"Cc":           "<cc@example.org>",
	}
	for k, want := range checks {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if note, err := h.Text("X-Note"); err != nil || note != "naïve" {
		t.Errorf("X-Note = %q, %v", note, err)
	}
	if m.MessageID != s.MessageID {
		t.Errorf("MessageID = %q", m.MessageID)
	}
}

func TestBuildDefaults(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.MessageID = ""
	s.Date = time.Time{}
	s.UserAgent = ""
	before := time.Now().Add(-time.Second)
	m, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9A-Z]{10}\.[0-9A-Z]{10}@example\.com$`).MatchString(m.MessageID) {
		t.Errorf("MessageID = %q", m.MessageID)
	}
	h := gomail.Header{Header: parse(t, m.Raw).header}
	if id, err := h.MessageID(); err != nil || id != m.MessageID {
		t.Errorf("Message-ID header = %q, %v", id, err)
	}
	if d, err := h.Date(); err != nil || d.Before(before.Truncate(time.Second)) {
		t.Errorf("Date = %v, %v", d, err)
	}
	if h.Has("User-Agent") {
		t.Errorf("User-Agent written when empty")
	}
}

func TestBuildSignature(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, sig string
		wantText  string
		wantHTML  bool
	}{
		{"present", "James\n*chainmail*", "Hello **world**.\n\n-- \nJames\n*chainmail*\n", true},
		{"empty", "", "Hello **world**.\n", false},
		{"blank", " \n", "Hello **world**.\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := baseSpec()
			s.Signature = tt.sig
			root := parse(t, mustBuild(t, s).Raw)
			if got := root.find("text/plain").body; got != tt.wantText {
				t.Errorf("text = %q, want %q", got, tt.wantText)
			}
			htm := root.find("text/html").body
			has := strings.Contains(htm, `<div class="signature">`)
			if has != tt.wantHTML {
				t.Errorf("signature div present = %t:\n%s", has, htm)
			}
			if tt.wantHTML && !strings.Contains(htm, "<em>chainmail</em>") {
				t.Errorf("signature Markdown not rendered:\n%s", htm)
			}
		})
	}
}

func TestBuildTextSignature(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.Body = Body{Kind: Text, Source: "Hi"}
	s.Signature = "James"
	if got := parse(t, mustBuild(t, s).Raw).body; got != "Hi\n\n-- \nJames\n" {
		t.Errorf("text = %q", got)
	}
}

func TestBuildQuote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		kind       BodyKind
		quote      func() *Quote
		wantText   string
		wantHTML   []string
		unwantHTML []string
	}{
		{
			name:     "reply",
			kind:     Markdown,
			quote:    replyQuote,
			wantText: "Thanks.\n\n-- \nJames\n\nOn Thu, 29 Oct 2026 at 10:00, Ann Smith <ann@example.org> wrote:\n> Are we on for Thursday?\n>\n>> Earlier note\n",
			wantHTML: []string{`<blockquote type="cite">`, "<p>Are we on for Thursday?</p>", "Ann Smith &lt;ann@example.org&gt; wrote:"},
		},
		{
			name:       "forward",
			kind:       Markdown,
			quote:      forwardQuote,
			wantText:   "Thanks.\n\n-- \nJames\n\n---------- Forwarded message ---------\nFrom: Ann Smith <ann@example.org>\nDate: Thu, 29 Oct 2026 10:00:00 -0400\nSubject: Plans\nTo: James Ainslie <james@example.com>\n\nThe plans are attached.\n",
			wantHTML:   []string{"---------- Forwarded message ---------<br>", "<p>The plans are attached.</p>"},
			unwantHTML: []string{"<blockquote type=\"cite\">"},
		},
		{
			name: "reply without html",
			kind: Markdown,
			quote: func() *Quote {
				q := replyQuote()
				q.HTML = ""
				q.Text = "if a < b && c > d\n"
				return q
			},
			wantText: "Thanks.\n\n-- \nJames\n\nOn Thu, 29 Oct 2026 at 10:00, Ann Smith <ann@example.org> wrote:\n> if a < b && c > d\n",
			wantHTML: []string{`<blockquote type="cite">`, "<pre>if a &lt; b &amp;&amp; c &gt; d\n</pre>"},
		},
		{
			name:     "reply in a text body",
			kind:     Text,
			quote:    replyQuote,
			wantText: "Thanks.\n\n-- \nJames\n\nOn Thu, 29 Oct 2026 at 10:00, Ann Smith <ann@example.org> wrote:\n> Are we on for Thursday?\n>\n>> Earlier note\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := baseSpec()
			s.Body = Body{Kind: tt.kind, Source: "Thanks.\n"}
			s.Signature = "James"
			s.Quote = tt.quote()
			root := parse(t, mustBuild(t, s).Raw)
			if got := root.find("text/plain").body; got != tt.wantText {
				t.Errorf("text = %q\nwant %q", got, tt.wantText)
			}
			htm := ""
			if h := root.find("text/html"); h != nil {
				htm = h.body
			}
			for _, w := range tt.wantHTML {
				if !strings.Contains(htm, w) {
					t.Errorf("HTML lacks %q:\n%s", w, htm)
				}
			}
			for _, w := range tt.unwantHTML {
				if strings.Contains(htm, w) {
					t.Errorf("HTML has %q:\n%s", w, htm)
				}
			}
			if htm != "" {
				sig, quote := strings.Index(htm, `class="signature"`), strings.Index(htm, `class="quote"`)
				body := strings.Index(htm, "<p>Thanks.</p>")
				if body < 0 || sig < body || quote < sig {
					t.Errorf("HTML order is not body, signature, quote:\n%s", htm)
				}
			}
		})
	}
}

func TestBuildEnvelope(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.To = []mail.Address{{Address: "ann@example.org"}, {Address: "Bob@Example.org"}}
	s.Cc = []mail.Address{{Address: "ANN@example.org"}, {Address: "cc@example.org"}}
	s.Bcc = []mail.Address{{Name: "Hidden", Address: "hidden@example.org"}, {Address: "bob@example.org"}}
	m := mustBuild(t, s)
	if bytes.Contains(m.Raw, []byte("hidden@example.org")) || bytes.Contains(bytes.ToLower(m.Raw), []byte("\nbcc:")) {
		t.Errorf("Bcc leaked into the message:\n%s", m.Raw)
	}
	want := []string{"ann@example.org", "Bob@Example.org", "cc@example.org", "hidden@example.org"}
	if !slices.Equal(m.EnvelopeTo, want) {
		t.Errorf("EnvelopeTo = %v, want %v", m.EnvelopeTo, want)
	}
	if m.EnvelopeFrom != "james@example.com" {
		t.Errorf("EnvelopeFrom = %q", m.EnvelopeFrom)
	}
}

func TestBuildBccOnly(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.To = nil
	s.Bcc = []mail.Address{{Address: "hidden@example.org"}}
	m := mustBuild(t, s)
	if bytes.Contains(m.Raw, []byte("hidden@example.org")) || !slices.Equal(m.EnvelopeTo, []string{"hidden@example.org"}) {
		t.Errorf("bcc-only message wrong: %v\n%s", m.EnvelopeTo, m.Raw)
	}
}

func TestBuildLongLines(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("abcdefghij", 500)
	for _, kind := range []BodyKind{Markdown, Text, HTML} {
		s := baseSpec()
		s.Body = Body{Kind: kind, Source: line}
		s.Subject = strings.Repeat("word ", 300) + strings.Repeat("x", 1500)
		s.Headers = map[string]string{"X-Long": strings.Repeat("y", 1200)}
		m := mustBuild(t, s)
		assertLineLimit(t, m.Raw)
		root := parse(t, m.Raw)
		if kind != HTML && root.find("text/plain").body != line {
			t.Errorf("kind %d: long line changed", kind)
		}
	}
}

func assertLineLimit(t testing.TB, raw []byte) {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), len(raw)+1)
	for n := 1; sc.Scan(); n++ {
		if l := len(sc.Bytes()); l > 998 {
			t.Fatalf("line %d is %d bytes", n, l)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestNewMessageID(t *testing.T) {
	t.Parallel()
	at := time.UnixMilli(1_790_000_000_000)
	a := NewMessageID("example.com", at, &seqReader{})
	if a != NewMessageID("example.com", at, &seqReader{}) {
		t.Errorf("not deterministic for a fixed reader")
	}
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{10}\.[0-9A-HJKMNP-TV-Z]{10}@example\.com$`).MatchString(a) {
		t.Errorf("id = %q", a)
	}
	if a != "01M3250V00.0123456789@example.com" {
		t.Errorf("id = %q", a)
	}
	later := NewMessageID("example.com", at.Add(time.Millisecond), &seqReader{})
	if later[:10] <= a[:10] {
		t.Errorf("time part does not sort: %q then %q", a, later)
	}
	if b := NewMessageID("example.com", at, failReader{}); len(b) != len(a) {
		t.Errorf("id from a failing reader = %q", b)
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("no randomness") }

func FuzzBuild(f *testing.F) {
	f.Add("Hello", "Hello **world**", uint8(0))
	f.Add("Grüße\r\nBcc: x@example.org", "line\r\n> quote\nFrom me", uint8(1))
	f.Add(strings.Repeat("x", 2000), "<p>hi<img src=\"x.png\"></p>", uint8(2))
	f.Add("\x00\xff", "\x00\xff", uint8(1))
	f.Fuzz(func(t *testing.T, subject, body string, kind uint8) {
		s := baseSpec()
		s.Subject = subject
		s.Body = Body{Kind: BodyKind(kind % 3), Source: body}
		s.Signature = "sig"
		s.Quote = replyQuote()
		s.Attachments = []File{{Filename: subject, Data: []byte(body)}}
		m, err := build(s, &seqReader{})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		assertLineLimit(t, m.Raw)
		assertCRLF(t, m.Raw)
		root := parse(t, m.Raw)
		h := gomail.Header{Header: root.header}
		if _, err := h.Subject(); err != nil {
			t.Errorf("subject does not decode: %v", err)
		}
		if h.Has("Bcc") {
			t.Errorf("subject injected a header")
		}
	})
}

// TestBuildCompositeAttachment checks a file declared as a composite type,
// such as an attached email a forward re-attaches, is sent as opaque data:
// base64 is not allowed for message/rfc822, and a multipart type would be
// read as structure.
func TestBuildCompositeAttachment(t *testing.T) {
	t.Parallel()
	for _, ct := range []string{"message/rfc822", "multipart/mixed; boundary=x"} {
		s := baseSpec()
		s.Attachments = []File{{Filename: "note.eml", ContentType: ct, Data: []byte("Subject: hi\r\n\r\nhi\r\n")}}
		root := parse(t, mustBuild(t, s).Raw)
		if len(root.parts) != 2 {
			t.Fatalf("%s: shape = %s", ct, root.shape())
		}
		p := root.parts[1]
		if p.mediaType != "application/octet-stream" || p.raw != string(s.Attachments[0].Data) {
			t.Errorf("%s: sent as %s %q", ct, p.mediaType, p.raw)
		}
	}
}

// assertCRLF checks every line of raw ends in CRLF, with no bare CR or LF.
func assertCRLF(t testing.TB, raw []byte) {
	t.Helper()
	for i, c := range raw {
		switch {
		case c == '\n' && (i == 0 || raw[i-1] != '\r'):
			t.Fatalf("bare LF at byte %d", i)
		case c == '\r' && (i+1 == len(raw) || raw[i+1] != '\n'):
			t.Fatalf("bare CR at byte %d", i)
		}
	}
}
