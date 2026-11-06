package imapstore

import (
	"encoding/base64"
	"io"
	"maps"
	"testing"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/testutil/imaptest"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

// Parts of inlineOriginal.
var (
	chartPNG = []byte("\x89PNG\r\n\x1a\nchart")
	logoGIF  = []byte("GIF89alogo")
	notes    = []byte("caf\xe9")
)

// inlineOriginal is an HTML message showing a named inline image and an
// unnamed one by Content-ID, with a windows-1252 text attachment.
var inlineOriginal = "From: alice@example.net\r\n" +
	"To: host@example.test\r\n" +
	"Subject: Chart\r\n" +
	"Message-ID: <chart-msg@example.net>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"mix\"\r\n" +
	"\r\n" +
	"--mix\r\n" +
	"Content-Type: multipart/related; boundary=\"rel\"\r\n" +
	"\r\n" +
	"--rel\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>See <img src=\"cid:chart\"> and <img src=\"cid:logo@example.net\"></p>\r\n" +
	"--rel\r\n" +
	"Content-Type: image/png; name=\"chart.png\"\r\n" +
	"Content-Disposition: inline; filename=\"chart.png\"\r\n" +
	"Content-ID: <chart>\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	base64.StdEncoding.EncodeToString(chartPNG) + "\r\n" +
	"--rel\r\n" +
	"Content-Type: image/gif\r\n" +
	"Content-ID: <logo@example.net>\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	base64.StdEncoding.EncodeToString(logoGIF) + "\r\n" +
	"--rel--\r\n" +
	"--mix\r\n" +
	"Content-Type: text/plain; charset=windows-1252\r\n" +
	"Content-Disposition: attachment; filename=\"notes.txt\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	base64.StdEncoding.EncodeToString(notes) + "\r\n" +
	"--mix--\r\n"

// unnamedOnly is a message whose only part besides its text is an unnamed
// inline image.
var unnamedOnly = "From: alice@example.net\r\n" +
	"Subject: Logo\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/related; boundary=\"rel\"\r\n" +
	"\r\n" +
	"--rel\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<img src=\"cid:logo\">\r\n" +
	"--rel\r\n" +
	"Content-Type: image/gif\r\n" +
	"Content-ID: <logo>\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	base64.StdEncoding.EncodeToString(logoGIF) + "\r\n" +
	"--rel--\r\n"

// TestInlinePartsKeepTheirMIMEMetadata reads inlineOriginal from a server.
// Its attachments list both inline images, the unnamed one included, and
// streaming each part gives its media type parameters, Content-ID and
// disposition, which a forward needs to send it again. A message whose
// only extra part is an unnamed inline image still has no attachment in
// its summary, as before.
func TestInlinePartsKeepTheirMIMEMetadata(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
		Username: "host@example.test", Password: "pw", Mail: map[string][]string{"INBOX": {inlineOriginal, unnamedOnly}},
	})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool})
	t.Cleanup(func() { _ = s.Close() })
	res, err := s.Search(t.Context(), mailstore.SearchRequest{})
	if err != nil || len(res) != 1 || len(res[0].UIDs) != 2 {
		t.Fatalf("Search = %+v, %v", res, err)
	}
	src := res[0].Source
	refOf := func(uid uint32) ref.Ref {
		return ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: src.UIDValidity, UID: uid}
	}
	uids := res[0].UIDs
	if uids[0] > uids[1] {
		uids[0], uids[1] = uids[1], uids[0]
	}
	got := s.FetchMessages(t.Context(), []ref.Ref{refOf(uids[0])})
	if len(got) != 1 || got[0].Err != nil {
		t.Fatalf("FetchMessages = %+v", got)
	}
	list := got[0].Message.Attachments
	wantParts := []string{"1.2", "1.3", "2"}
	if len(list) != len(wantParts) {
		t.Fatalf("attachments = %+v, want parts %v", list, wantParts)
	}
	want := []struct {
		filename, contentType string
		params                map[string]string
		contentID             string
		inline                bool
		data                  []byte
	}{
		{"chart.png", "image/png", map[string]string{"name": "chart.png"}, "chart", true, chartPNG},
		{"", "image/gif", nil, "logo@example.net", true, logoGIF},
		{"notes.txt", "text/plain", map[string]string{"charset": "windows-1252"}, "", false, notes},
	}
	for i, w := range want {
		a := list[i]
		if a.Part != wantParts[i] || a.Filename != w.filename || a.ContentType != w.contentType {
			t.Errorf("attachment %d = %+v, want part %s %q %s", i, a, wantParts[i], w.filename, w.contentType)
		}
		info, rc, err := s.StreamPart(t.Context(), refOf(uids[0]), a.Part)
		if err != nil {
			t.Fatalf("StreamPart(%s): %v", a.Part, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || string(data) != string(w.data) {
			t.Errorf("part %s = %q, %v, want %q", a.Part, data, err, w.data)
		}
		if info.ContentType != w.contentType || !maps.Equal(info.Params, w.params) || info.ContentID != w.contentID || info.Inline != w.inline {
			t.Errorf("part %s info = %+v, want %s %v Content-ID %q inline %v", a.Part, info, w.contentType, w.params, w.contentID, w.inline)
		}
	}
	sums, err := s.FetchSummaries(t.Context(), src, []uint32{uids[1]}, mailstore.SummaryOptions{})
	if err != nil || len(sums) != 1 {
		t.Fatalf("FetchSummaries = %+v, %v", sums, err)
	}
	if sums[0].HasAttachment {
		t.Error("a message with only an unnamed inline image has an attachment")
	}
}
