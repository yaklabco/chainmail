package daemon_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/emersion/go-message"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// Parts of inlineOriginal.
var (
	chartPNG = []byte("\x89PNG\r\n\x1a\nchart")
	logoGIF  = []byte("GIF89alogo")
	notesTxt = []byte("caf\xe9")
)

// inlineOriginal is an HTML message showing a named inline image and an
// unnamed one by Content-ID, with a windows-1252 text attachment.
var inlineOriginal = "From: alice@example.net\r\n" +
	"To: me@example.test\r\n" +
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
	base64.StdEncoding.EncodeToString(notesTxt) + "\r\n" +
	"--mix--\r\n"

// sentPart is one leaf of a sent message, with the media type of the
// multipart that holds it.
type sentPart struct {
	parent string
	header message.Header
	raw    []byte
}

// sentLeaves lists the leaves of raw, with their bytes decoded.
func sentLeaves(t *testing.T, raw []byte) []sentPart {
	t.Helper()
	var out []sentPart
	var walk func(e *message.Entity, parent string)
	walk = func(e *message.Entity, parent string) {
		mt, _, _ := e.Header.ContentType()
		if mr := e.MultipartReader(); mr != nil {
			for {
				p, err := mr.NextPart()
				if errors.Is(err, io.EOF) {
					return
				}
				if err != nil && !message.IsUnknownCharset(err) {
					t.Fatalf("reading part: %v", err)
				}
				walk(p, mt)
			}
		}
		b, err := io.ReadAll(e.Body)
		if err != nil {
			t.Fatalf("reading %s: %v", mt, err)
		}
		out = append(out, sentPart{parent: parent, header: e.Header, raw: b})
	}
	e, err := message.Read(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) {
		t.Fatalf("parsing message: %v", err)
	}
	walk(e, "")
	return out
}

// TestForwardKeepsInlinePartsThroughIMAP forwards inlineOriginal read from
// an IMAP server through the production store. The sent message keeps the
// named and the unnamed inline image under their Content-IDs, inline in the
// multipart/related around the HTML that refers to them, and the text
// attachment keeps its charset and bytes.
func TestForwardKeepsInlinePartsThroughIMAP(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	imapEP := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
		Username: "me@example.test", Password: "pw", Folders: []string{"Sent"}, Mail: map[string][]string{"INBOX": {inlineOriginal}},
	})
	srv := smtptest.StartWith(t, ca, provider.SecurityStartTLS, smtptest.Options{}, smtptest.User{Username: "me@example.test", Password: "pw"})
	h := daemontest.Start(t, fmt.Sprintf(`
[accounts.local]
provider = "custom"
address = "me@example.test"
pool = 2
imap = { host = "127.0.0.1", port = %d, security = "tls" }
smtp = { host = "127.0.0.1", port = %d, security = "starttls" }
`, imapEP.Port, srv.Endpoint().Port), daemontest.Options{RootCAs: ca.Pool})
	setSecret(t, h, "accounts/local", "pw")
	found, err := h.Client.Message.Search(t.Context(), connect.NewRequest(&chainmailv1.SearchRequest{QueryString: "subject:Chart"}))
	if err != nil || len(found.Msg.GetRows()) != 1 {
		t.Fatalf("Search = %v, %v", found, err)
	}
	orig := found.Msg.GetRows()[0].GetRef()
	if _, err := h.Client.Compose.Forward(t.Context(), connect.NewRequest(&chainmailv1.ForwardRequest{
		Ref: orig, To: []string{"zed@example.org"}, Content: replyContent("FYI"), IdempotencyKey: "fwd-inline",
	})); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	msgs := srv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	byCID := map[string]sentPart{}
	var htm, notes *sentPart
	for _, p := range sentLeaves(t, msgs[0].Data) {
		mt, _, _ := p.header.ContentType()
		switch {
		case p.header.Get("Content-Id") != "":
			byCID[p.header.Get("Content-Id")] = p
		case mt == "text/html":
			htm = &p
		case mt == "text/plain" && p.parent == "multipart/mixed":
			notes = &p
		}
	}
	for cid, want := range map[string]struct {
		filename string
		data     []byte
	}{"<chart>": {"chart.png", chartPNG}, "<logo@example.net>": {"", logoGIF}} {
		p, ok := byCID[cid]
		if !ok {
			t.Errorf("no part with Content-ID %s; parts by Content-ID: %v", cid, byCID)
			continue
		}
		disp, params, _ := p.header.ContentDisposition()
		if p.parent != "multipart/related" || disp != "inline" || params["filename"] != want.filename || !bytes.Equal(p.raw, want.data) {
			t.Errorf("%s: in %s, disposition %q %v, %q; want inline in multipart/related, filename %q, the original's bytes",
				cid, p.parent, disp, params, p.raw, want.filename)
		}
	}
	if htm == nil || !strings.Contains(string(htm.raw), "cid:chart") || !strings.Contains(string(htm.raw), "cid:logo@example.net") {
		t.Errorf("the HTML part lost the original's image references: %v", htm)
	}
	if notes == nil {
		t.Fatal("the text attachment is missing")
	}
	// The daemon registers go-message's charset readers, so reading the part
	// may decode it to UTF-8 by its declared charset: either way the bytes
	// sent are the original's windows-1252.
	if _, params, _ := notes.header.ContentType(); params["charset"] != "windows-1252" || !bytes.Equal(notes.raw, notesTxt) && string(notes.raw) != "caf\u00e9" {
		t.Errorf("text attachment = charset %q, %q; want windows-1252 and the original's bytes", params["charset"], notes.raw)
	}
}
