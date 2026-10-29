package render

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestRenderCorpus(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("testdata/corpus/*.eml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 8 {
		t.Fatalf("found %d corpus messages, want 8", len(files))
	}
	for _, path := range files {
		name := strings.TrimSuffix(filepath.Base(path), ".eml")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body, err := Render(raw, Options{Limit: 64 << 10})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			full, err := Render(raw, Options{Full: true})
			if err != nil {
				t.Fatalf("Render full: %v", err)
			}
			if full.QuotedTrimmed || full.Truncated {
				t.Errorf("full render trimmed or truncated: %+v", full)
			}
			if body.QuotedTrimmed && body.TrimmedBytes != len(full.Text)-len(body.Text) {
				t.Errorf("trimmed_bytes = %d, want %d", body.TrimmedBytes, len(full.Text)-len(body.Text))
			}
			if !body.QuotedTrimmed && (body.Text != full.Text || body.TrimmedBytes != 0) {
				t.Errorf("untrimmed body differs from the full render")
			}
			got := fmt.Sprintf("quoted_trimmed: %t\ntruncated: %t\n---\n%s\n", body.QuotedTrimmed, body.Truncated, body.Text)
			goldenPath := filepath.Join("testdata", "corpus", name+".golden")
			if *update {
				if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("render differs from %s\n got:\n%s\nwant:\n%s", goldenPath, got, want)
			}
		})
	}
}

func TestRenderFullKeepsQuote(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/corpus/gmail-reply.eml")
	if err != nil {
		t.Fatal(err)
	}
	body, err := Render(raw, Options{Full: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Text, "> Are we still on for Thursday?") || body.Truncated {
		t.Errorf("full body = %+v", body)
	}
}

func TestRenderTruncatesOnRuneBoundary(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: text/plain; charset=utf-8\n\n" + strings.Repeat("é", 100)
	body, err := Render([]byte(raw), Options{Limit: 101})
	if err != nil {
		t.Fatal(err)
	}
	if !body.Truncated || body.Text != strings.Repeat("é", 50) || !utf8.ValidString(body.Text) {
		t.Errorf("body = truncated %t, %d bytes", body.Truncated, len(body.Text))
	}
	if body.QuotedTrimmed {
		t.Error("truncation is not trimming")
	}
}

func TestRenderNoTextPart(t *testing.T) {
	t.Parallel()
	body, err := Render([]byte("Content-Type: image/png\n\nPNG"), Options{Limit: 100})
	if err != nil || body != (Body{}) {
		t.Errorf("Render = %+v, %v; want an empty body", body, err)
	}
}

func TestSnippet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                         string
		mediaType, charset, encoding string
		data                         string
		want                         string
	}{
		{"plain", "text/plain", "utf-8", "7bit", "Hello\n  there\n\nfriend", "Hello there friend"},
		{"quoted-printable latin1", "text/plain", "iso-8859-1", "quoted-printable", "Gr=FC=DFe", "Grüße"},
		{"base64 cut mid-quad", "text/plain", "utf-8", "base64", "SGVsbG8sIHdvcmxkIQ", "Hello, world"},
		{"html", "text/html", "utf-8", "", "<p>Hi <b>there</b></p><script>x()</script>", "Hi there"},
		{"long", "text/plain", "", "", strings.Repeat("ab ", 300), strings.TrimSpace(strings.Repeat("ab ", 67))[:200]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Snippet(tt.mediaType, tt.charset, tt.encoding, []byte(tt.data))
			if got != tt.want {
				t.Errorf("Snippet = %q, want %q", got, tt.want)
			}
			if utf8.RuneCountInString(got) > SnippetLength {
				t.Errorf("snippet has %d characters", utf8.RuneCountInString(got))
			}
		})
	}
}

func TestClip(t *testing.T) {
	t.Parallel()
	if got := Clip("  a\n\tb  "); got != "a b" {
		t.Errorf("Clip = %q", got)
	}
	long := strings.Repeat("é", 250)
	if got := Clip(long); utf8.RuneCountInString(got) != SnippetLength {
		t.Errorf("Clip kept %d characters", utf8.RuneCountInString(got))
	}
}

func TestCleanText(t *testing.T) {
	t.Parallel()
	in := "a\x00b\x1bc\u202eevil\u202c\u200bd\ufeffe\tf\ng\x7fh\u0085i\xff"
	if got, want := CleanText(in), "abcevilde\tf\ngh"+"i\ufffd"; got != want {
		t.Errorf("CleanText = %q, want %q", got, want)
	}
	if got := CleanText("family \U0001F468\u200d\U0001F469"); got != "family \U0001F468\u200d\U0001F469" {
		t.Errorf("CleanText dropped a zero-width joiner: %q", got)
	}
	// Tag characters spell hidden ASCII that a model reads and a human does
	// not see. Soft hyphens, word joiners and other format characters are
	// invisible too. Line and paragraph separators become newlines.
	hidden := "a\U000E0069\U000E0067\U000E006E\U000E007Fb\u00adc\u2060d\u2062e\U0001D173f\u2028g\u2029h"
	if got, want := CleanText(hidden), "abcdef\ng\nh"; got != want {
		t.Errorf("CleanText(%q) = %q, want %q", hidden, got, want)
	}
	if got := CleanText("\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645"); got != "\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645" {
		t.Errorf("CleanText dropped a zero-width non-joiner: %q", got)
	}
}

// deepHTML nests past html.Parse's limit of 512 open elements, so only the
// tokenizer fallback can convert it.
var deepHTML = strings.Repeat("<div>", 600)

func TestRenderDeepHTMLFallsBack(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: text/html; charset=utf-8\n\n" + deepHTML + "<p>Hi</p><p>--</p><p>Sam</p>"
	body, err := Render([]byte(raw), Options{Limit: 1000})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if body.Text != "Hi" || !body.QuotedTrimmed || body.TrimmedBytes != len("\n\n--\n\nSam") {
		t.Errorf("Render = %+v", body)
	}
	full, err := Render([]byte(raw), Options{Full: true})
	if err != nil || full.Text != "Hi\n\n--\n\nSam" {
		t.Errorf("Render full = %+v, %v", full, err)
	}
}

func TestRenderKeepsForwards(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, raw, want string }{
		{
			"outlook plain-text forward",
			"Subject: FW: Thursday\nContent-Type: text/plain\n\nFYI\n\n________________________________\n" +
				"From: Alex Doe <alex@example.com>\nSent: Tuesday\nSubject: Thursday\n\nAre we still on?",
			"FYI\n\n________________________________\nFrom: Alex Doe <alex@example.com>\nSent: Tuesday\nSubject: Thursday\n\nAre we still on?",
		},
		{
			"outlook html forward",
			"Subject: Fw: Thursday\nContent-Type: text/html\n\n<p>FYI</p><hr><div id=\"divRplyFwdMsg\"><b>From:</b> Alex</div><div>Are we still on?</div>",
			"FYI\n\nFrom: Alex\nAre we still on?",
		},
		{
			"encoded german subject",
			"Subject: =?UTF-8?Q?WG=3A_Donnerstag?=\nContent-Type: text/plain\n\nZur Info\n\nVon: Alex <alex@example.com>\nGesendet: Dienstag\n\nPasst es?",
			"Zur Info\n\nVon: Alex <alex@example.com>\nGesendet: Dienstag\n\nPasst es?",
		},
		{
			"gateway tag before the prefix",
			"Subject: [EXTERNAL] [team] FW: Thursday\nContent-Type: text/plain\n\nFYI\n\nFrom: Alex <alex@example.com>\nSent: Tuesday\n\nAre we still on?",
			"FYI\n\nFrom: Alex <alex@example.com>\nSent: Tuesday\n\nAre we still on?",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body, err := Render([]byte(tt.raw), Options{Limit: 1000})
			if err != nil {
				t.Fatal(err)
			}
			if body.Text != tt.want || body.QuotedTrimmed || body.TrimmedBytes != 0 {
				t.Errorf("Render = %+v, want text %q untrimmed", body, tt.want)
			}
		})
	}
	// A reply to a forward is a reply, and its history is trimmed.
	raw := "Subject: RE: FW: Thursday\nContent-Type: text/plain\n\nThanks\n\nFrom: Sam <sam@example.org>\nSent: Tuesday\n\nFYI"
	if body, err := Render([]byte(raw), Options{}); err != nil || body.Text != "Thanks" {
		t.Errorf("reply to a forward = %+v, %v", body, err)
	}
}

func TestRenderHTMLMarkers(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, html, want string }{
		{
			"gmail reply quoting an outlook reply",
			`<div>My reply</div><div class="gmail_quote"><div class="gmail_attr">On Tue, 3 Sep 2024, Alex &lt;a@example.com&gt; wrote:</div>` +
				`<blockquote class="gmail_quote"><div>Alex reply</div><hr><div id="divRplyFwdMsg"><b>From:</b> Sam</div><div>Older</div></blockquote></div>`,
			"My reply",
		},
		{
			"outlook reply quoting a gmail reply",
			`<p>My reply</p><hr><div id="divRplyFwdMsg"><b>From:</b> Alex</div><div>Alex reply<div class="gmail_quote">Older</div></div>`,
			"My reply",
		},
		{
			"gmail quote with text after it is kept",
			`<div>Top</div><div class="gmail_quote">Quoted</div><div>After</div>`,
			"Top\nQuoted\nAfter",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body, err := Render([]byte("Content-Type: text/html\n\n"+tt.html), Options{})
			if err != nil || body.Text != tt.want {
				t.Errorf("Render = %+v, %v; want text %q", body, err, tt.want)
			}
		})
	}
}

func TestCleanTextVariationSelectors(t *testing.T) {
	t.Parallel()
	// Runs of variation selectors can carry a hidden payload, one byte per
	// selector. The emoji and text presentation selectors stay.
	in := "a\ufe00\ufe01b\U000E0100\U000E01EFc\u2764\ufe0f\u2709\ufe0e"
	if got, want := CleanText(in), "abc\u2764\ufe0f\u2709\ufe0e"; got != want {
		t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
	}
}

func TestRenderClippedPartIsTruncated(t *testing.T) {
	t.Parallel()
	// A base64 decode error stops the part early, so it is incomplete.
	raw := "Content-Type: text/plain\nContent-Transfer-Encoding: base64\n\nSGVsbG8g!!!!d29ybGQ="
	for _, opts := range []Options{{Limit: 1000}, {Full: true}} {
		body, err := Render([]byte(raw), opts)
		if err != nil || !body.Truncated {
			t.Errorf("Render(%+v) = %+v, %v; want truncated", opts, body, err)
		}
	}
}

func TestCleanHeader(t *testing.T) {
	t.Parallel()
	if got := CleanHeader(" Re:\r\n  Q3\tplan \u202e"); got != "Re: Q3 plan" {
		t.Errorf("CleanHeader = %q", got)
	}
}
