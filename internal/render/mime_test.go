package render

import (
	"strings"
	"testing"
)

func TestPickText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		html bool
		text string
	}{
		{"plain only", "Content-Type: text/plain; charset=utf-8\n\nHello\n", false, "Hello\n"},
		{"no content type", "Subject: x\n\nHello\n", false, "Hello\n"},
		{
			"alternative prefers plain",
			"Content-Type: multipart/alternative; boundary=b\n\n--b\nContent-Type: text/html\n\n<p>Hi</p>\n--b\nContent-Type: text/plain\n\nHi\n--b--\n",
			false, "Hi",
		},
		{
			"alternative falls back to html",
			"Content-Type: multipart/alternative; boundary=b\n\n--b\nContent-Type: text/html\n\n<p>Hi</p>\n--b\nContent-Type: image/png\n\nPNG\n--b--\n",
			true, "<p>Hi</p>",
		},
		{"html only", "Content-Type: text/html\n\n<p>Hi</p>\n", true, "<p>Hi</p>\n"},
		{
			"mixed skips attachment",
			"Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\nContent-Disposition: attachment; filename=notes.txt\n\nattached\n--b\nContent-Type: text/plain\n\nbody\n--b--\n",
			false, "body",
		},
		{
			"nested related alternative",
			"Content-Type: multipart/mixed; boundary=o\n\n--o\nContent-Type: multipart/related; boundary=r\n\n--r\n" +
				"Content-Type: multipart/alternative; boundary=a\n\n--a\nContent-Type: text/plain\n\ndeep\n--a\nContent-Type: text/html\n\n<b>deep</b>\n--a--\n" +
				"--r\nContent-Type: image/png\n\nPNG\n--r--\n--o--\n",
			false, "deep",
		},
		{"base64", "Content-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: base64\n\nSGVsbG8sIHdvcmxk\n", false, "Hello, world"},
		{"quoted-printable latin1", "Content-Type: text/plain; charset=iso-8859-1\nContent-Transfer-Encoding: quoted-printable\n\nGr=FC=DFe\n", false, "Grüße\n"},
		{"unknown charset", "Content-Type: text/plain; charset=x-made-up\n\n\xe9t\xe9\n", false, "\ufffdt\ufffd\n"},
		{"invalid utf-8", "Content-Type: text/plain; charset=utf-8\n\nok \xff\xfe end\n", false, "ok \ufffd end\n"},
		{"crlf", "Content-Type: text/plain\r\n\r\nline one\r\nline two\r\n", false, "line one\nline two\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tp, err := pickText([]byte(tt.raw))
			if err != nil {
				t.Fatalf("pickText: %v", err)
			}
			if tp == nil {
				t.Fatal("pickText found no text part")
			}
			if tp.html != tt.html || tp.text != tt.text {
				t.Errorf("pickText = html %t %q, want html %t %q", tp.html, tp.text, tt.html, tt.text)
			}
		})
	}
}

func TestPickTextNone(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"Content-Type: image/png\n\nPNG\n",
		"Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: application/pdf\n\n%PDF\n--b--\n",
		"Content-Type: text/plain\nContent-Disposition: attachment; filename=a.txt\n\nx\n",
	} {
		tp, err := pickText([]byte(raw))
		if err != nil || tp != nil {
			t.Errorf("pickText(%q) = %+v, %v; want nil, nil", raw, tp, err)
		}
	}
}

func TestPickTextClipsHugeParts(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: text/plain\n\n" + string(make([]byte, maxPartBytes+10))
	tp, err := pickText([]byte(raw))
	if err != nil || tp == nil {
		t.Fatalf("pickText = %v, %v", tp, err)
	}
	if !tp.clipped || len(tp.text) > maxPartBytes {
		t.Errorf("clipped = %t, len = %d", tp.clipped, len(tp.text))
	}
}

func TestPickTextMalformedHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		html bool
		text string
	}{
		{
			"unquoted filename with a space",
			"Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\nContent-Disposition: attachment; filename=my notes.txt\n\nATTACHED\n--b\nContent-Type: text/plain\n\nbody\n--b--\n",
			false, "body",
		},
		{
			"duplicate filename",
			"Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\nContent-Disposition: attachment; filename=a.txt; filename=b.txt\n\nATTACHED\n--b\nContent-Type: text/plain\n\nbody\n--b--\n",
			false, "body",
		},
		{"unterminated charset quote", "Content-Type: text/html; charset=\"utf-8\n\n<p>Hi</p>\n", true, "<p>Hi</p>\n"},
		{"upper case type with broken parameter", "Content-Type: TEXT/HTML; charset\n\n<p>Hi</p>\n", true, "<p>Hi</p>\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tp, err := pickText([]byte(tt.raw))
			if err != nil {
				t.Fatalf("pickText: %v", err)
			}
			if tp == nil {
				t.Fatal("pickText found no text part")
			}
			if tp.html != tt.html || tp.text != tt.text {
				t.Errorf("pickText = html %t %q, want html %t %q", tp.html, tp.text, tt.html, tt.text)
			}
		})
	}
}

func TestPickTextDecodeErrorMarksClipped(t *testing.T) {
	t.Parallel()
	raw := "Content-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: base64\n\nSGVsbG8s\nIHdvcmxk!!!!\nSGVsbG8s\n"
	tp, err := pickText([]byte(raw))
	if err != nil || tp == nil {
		t.Fatalf("pickText = %v, %v", tp, err)
	}
	if !tp.clipped || tp.text != "Hello, world" {
		t.Errorf("pickText = clipped %t %q, want clipped true %q", tp.clipped, tp.text, "Hello, world")
	}
}

func TestPickTextClipsOnRuneBoundary(t *testing.T) {
	t.Parallel()
	// The two-byte é straddles the clip point.
	raw := "Content-Type: text/plain; charset=utf-8\n\n" + strings.Repeat("a", maxPartBytes-1) + "étail"
	tp, err := pickText([]byte(raw))
	if err != nil || tp == nil {
		t.Fatalf("pickText = %v, %v", tp, err)
	}
	if !tp.clipped {
		t.Error("clipped = false, want true")
	}
	if strings.ContainsRune(tp.text, '�') {
		t.Errorf("text ends %q, want no U+FFFD", tp.text[len(tp.text)-8:])
	}
	if len(tp.text) != maxPartBytes-1 {
		t.Errorf("len = %d, want %d", len(tp.text), maxPartBytes-1)
	}
}
