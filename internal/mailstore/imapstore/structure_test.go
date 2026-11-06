package imapstore

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/render"
)

func sampleStructure() imap.BodyStructure {
	return &imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
		&imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
			&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Params: map[string]string{"charset": "utf-8"}, Size: 10},
			&imap.BodyStructureSinglePart{Type: "text", Subtype: "html", Size: 20},
		}},
		&imap.BodyStructureSinglePart{
			Type: "application", Subtype: "pdf", Encoding: "base64", Size: 4000,
			Extended: &imap.BodyStructureSinglePartExt{Disposition: &imap.BodyStructureDisposition{
				Value: "attachment", Params: map[string]string{"filename": "=?utf-8?q?R=C3=A9sum=C3=A9.pdf?="},
			}},
		},
		&imap.BodyStructureSinglePart{Type: "message", Subtype: "rfc822", Size: 500},
	}}
}

func TestAttachmentsAndTextLeaves(t *testing.T) {
	t.Parallel()
	bs := sampleStructure()
	want := []mailstore.Attachment{
		{Part: "2", Filename: "Résumé.pdf", ContentType: "application/pdf", Size: 4000},
		{Part: "3", Filename: "message.eml", ContentType: "message/rfc822", Size: 500},
	}
	if got := attachmentsOf(bs); !reflect.DeepEqual(got, want) {
		t.Errorf("attachmentsOf = %+v, want %+v", got, want)
	}
	path, leaf := textLeaf(bs)
	if !reflect.DeepEqual(path, []int{1, 1}) || leaf.MediaType() != "text/plain" {
		t.Errorf("textLeaf = %v %v", path, leaf)
	}
	var got []string
	for _, sec := range textSections(bs) {
		got = append(got, fmt.Sprintf("%v %s %t", sec.Part, sec.Specifier, sec.Partial != nil))
	}
	wantSecs := []string{"[1 1] MIME false", "[1 1]  true", "[1 2] MIME false", "[1 2]  true"}
	if !reflect.DeepEqual(got, wantSecs) {
		t.Errorf("textSections = %q, want %q", got, wantSecs)
	}
	if sp := findPart(bs, []int{2}); sp == nil || sp.Subtype != "pdf" {
		t.Errorf("findPart(2) = %v", sp)
	}
	if sp := findPart(bs, []int{9}); sp != nil {
		t.Errorf("findPart(9) = %v, want nil", sp)
	}
}

func sectionKey(sec *imap.FetchItemBodySection) string {
	return fmt.Sprintf("%v/%s/%t", sec.Part, sec.Specifier, sec.Partial != nil)
}

func TestRebuildMultipart(t *testing.T) {
	t.Parallel()
	header := []byte("From: a@example.com\r\nSubject: s\r\nContent-Type: multipart/mixed; boundary=\"orig\"\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	parts := map[string]string{
		"[1 1]/MIME/false": "Content-Type: text/plain; charset=utf-8\r\n\r\n",
		"[1 1]//true":      "Hello",
		"[1 2]/MIME/false": "Content-Type: text/html\r\n\r\n",
		"[1 2]//true":      "<p>Hello</p>",
	}
	raw, clipped, err := rebuild(header, sampleStructure(), func(sec *imap.FetchItemBodySection) []byte { return []byte(parts[sectionKey(sec)]) })
	if err != nil || clipped {
		t.Fatalf("rebuild = clipped %t, %v", clipped, err)
	}
	if strings.Contains(string(raw), "boundary=\"orig\"") || strings.Contains(string(raw), "base64") {
		t.Errorf("rebuilt header kept the original boundary or encoding:\n%s", raw)
	}
	body, err := render.Render(raw, render.Options{})
	if err != nil || body.Text != "Hello" {
		t.Errorf("render of the rebuilt message = %q, %v\n%s", body.Text, err, raw)
	}
}

func TestRebuildSinglePart(t *testing.T) {
	t.Parallel()
	bs := &imap.BodyStructureSinglePart{Type: "text", Subtype: "plain"}
	header := []byte("Subject: x\r\nContent-Type: text/plain\r\n\r\n")
	raw, clipped, err := rebuild(header, bs, func(sec *imap.FetchItemBodySection) []byte {
		if sec.Specifier != imap.PartSpecifierText {
			t.Errorf("single part fetched %v", sec)
		}
		return []byte("Hi")
	})
	if err != nil || clipped || string(raw) != string(header)+"Hi" {
		t.Errorf("rebuild = %q, %t, %v", raw, clipped, err)
	}
	_, clipped, _ = rebuild(header, bs, func(*imap.FetchItemBodySection) []byte { return make([]byte, mailstore.TextPartCap) })
	if !clipped {
		t.Error("a text part at the cap should be reported as clipped")
	}
	raw, _, _ = rebuild(header, &imap.BodyStructureSinglePart{Type: "image", Subtype: "png"}, func(*imap.FetchItemBodySection) []byte {
		t.Error("fetched a part of an image-only message")
		return nil
	})
	if string(raw) != string(header) {
		t.Errorf("image-only rebuild = %q", raw)
	}
}

func TestParsePart(t *testing.T) {
	t.Parallel()
	good := map[string][]int{"1": {1}, "2": {2}, "1.2": {1, 2}, "10.3.1": {10, 3, 1}}
	for in, want := range good {
		if got, ok := parsePart(in); !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("parsePart(%q) = %v, %t", in, got, ok)
		}
	}
	for _, bad := range []string{"", "0", "1.", ".1", "a", "1.0", "1..2", "-1", "1.2.3.4.5.6.7.8.9.10.11.12.13.14.15.16.17"} {
		if _, ok := parsePart(bad); ok {
			t.Errorf("parsePart(%q) accepted", bad)
		}
	}
}

func TestRebuildSkipsMalformedHeaderLines(t *testing.T) {
	t.Parallel()
	header := []byte(" stray continuation\r\nFrom: a@example.com\r\nthis line has no colon\r\nSubject: s\r\nReferences: <r1@x>\r\n" +
		"Bad Key: spaces are not allowed in a field name\r\nContent-Type: multipart/mixed; boundary=\"orig\"\r\n\r\n")
	parts := map[string]string{
		"[1 1]/MIME/false": "Content-Type: text/plain; charset=utf-8\r\n\r\n",
		"[1 1]//true":      "Hello",
	}
	raw, _, err := rebuild(header, sampleStructure(), func(sec *imap.FetchItemBodySection) []byte { return []byte(parts[sectionKey(sec)]) })
	if err != nil {
		t.Fatalf("rebuild with a malformed header line: %v", err)
	}
	if !strings.Contains(string(raw), "Subject: s") || strings.Contains(string(raw), "no colon") {
		t.Errorf("rebuilt header should keep the good fields and drop the bad lines:\n%s", raw)
	}
	body, err := render.Render(raw, render.Options{})
	if err != nil || body.Text != "Hello" {
		t.Errorf("render of the rebuilt message = %q, %v\n%s", body.Text, err, raw)
	}
	if ids := msgIDList(header, "References"); !reflect.DeepEqual(ids, []string{"r1@x"}) {
		t.Errorf("References = %q, want [r1@x] despite the malformed lines", ids)
	}
}
