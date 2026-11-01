package mcpserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
)

var errServerGone = errors.New("server went away")

func fakeAttachment(name string, chunks []string, failAfter bool) func(context.Context, *chainmailv1.DownloadAttachmentRequest, *connect.ServerStream[chainmailv1.DownloadAttachmentResponse]) error {
	return func(_ context.Context, _ *chainmailv1.DownloadAttachmentRequest, st *connect.ServerStream[chainmailv1.DownloadAttachmentResponse]) error {
		hdr := &chainmailv1.AttachmentHeader{Filename: &chainmailv1.Untrusted{Untrusted: name}, ContentType: "application/pdf", Size: 99}
		if err := st.Send(&chainmailv1.DownloadAttachmentResponse{Chunk: &chainmailv1.DownloadAttachmentResponse_Header{Header: hdr}}); err != nil {
			return err
		}
		for _, c := range chunks {
			if err := st.Send(&chainmailv1.DownloadAttachmentResponse{Chunk: &chainmailv1.DownloadAttachmentResponse_Data{Data: []byte(c)}}); err != nil {
				return err
			}
		}
		if failAfter {
			return api.New(connect.CodeUnavailable, api.ReasonServerUnavailable, "try again", errServerGone)
		}
		return nil
	}
}

func TestDownloadAttachment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		filename  string
		chunks    []string
		failAfter bool
		calls     int
		wantNames []string
		wantErr   string
	}{
		{name: "saves under the sanitized name", filename: "../q3<report>.pdf", chunks: []string{"%PDF", "-1.7"}, calls: 1, wantNames: []string{"q3<report>.pdf"}},
		{name: "a second download gets a suffix", filename: "a.pdf", chunks: []string{"x"}, calls: 2, wantNames: []string{"a.pdf", "a (2).pdf"}},
		{name: "an empty name becomes attachment", filename: "", chunks: []string{"x"}, calls: 1, wantNames: []string{"attachment"}},
		{name: "a failed stream leaves nothing", filename: "a.pdf", chunks: []string{"x"}, failAfter: true, calls: 1, wantErr: api.ReasonServerUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{DownloadAttachment: fakeAttachment(tt.filename, tt.chunks, tt.failAfter)}, latest)
			var got []string
			for range tt.calls {
				res := e.call(t, "download_attachment", map[string]any{"ref": testRef, "part": "2"})
				if tt.wantErr != "" {
					wantError(t, res, tt.wantErr)
					continue
				}
				wantOK(t, res)
				var out attachmentFileOut
				decode(t, res, &out)
				path := out.Path.Untrusted
				b, err := os.ReadFile(path)
				if err != nil || string(b) != strings.Join(tt.chunks, "") || out.Size != int64(len(b)) {
					t.Errorf("file %s = %q, %v (size %d)", path, b, err, out.Size)
				}
				if filepath.Dir(path) != e.dir || out.ContentType.Untrusted != "application/pdf" || out.Filename == nil || out.Filename.Untrusted != tt.filename {
					t.Errorf("out = %+v", out)
				}
				if !strings.Contains(textOf(res), untrustedPreamble) {
					t.Error("the text does not wrap the message-derived name")
				}
				got = append(got, filepath.Base(path))
			}
			if strings.Join(got, "|") != strings.Join(tt.wantNames, "|") {
				t.Errorf("names = %v, want %v", got, tt.wantNames)
			}
			if tt.wantErr != "" {
				if entries, _ := os.ReadDir(e.dir); len(entries) != 0 {
					t.Errorf("files left after a failed download: %v", entries)
				}
			}
		})
	}
}

func TestDownloadAttachmentDirRules(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, &fakeDaemon{DownloadAttachment: fakeAttachment("a.pdf", []string{"x"}, false)}, latest)
	for _, dir := range []string{"relative/dir", filepath.Join(e.dir, "missing")} {
		wantError(t, e.call(t, "download_attachment", map[string]any{"ref": testRef, "part": "2", "dir": dir}), api.ReasonInvalidArgument)
	}
	other := t.TempDir()
	res := e.call(t, "download_attachment", map[string]any{"ref": testRef, "part": "2", "dir": other})
	wantOK(t, res)
	var out attachmentFileOut
	decode(t, res, &out)
	if filepath.Dir(out.Path.Untrusted) != other {
		t.Errorf("saved to %s, want %s", out.Path.Untrusted, other)
	}
}

func TestSaveRawMessage(t *testing.T) {
	t.Parallel()
	raw := "From: a@x.test\r\nSubject: hi\r\n\r\nbody\r\n"
	f := &fakeDaemon{GetRaw: func(_ context.Context, _ *chainmailv1.GetRawRequest, st *connect.ServerStream[chainmailv1.GetRawResponse]) error {
		for _, c := range []string{raw[:10], raw[10:]} {
			if err := st.Send(&chainmailv1.GetRawResponse{Data: []byte(c)}); err != nil {
				return err
			}
		}
		return nil
	}}
	e := newTestEnv(t, f, latest)
	for i, want := range []string{"message.eml", "message (2).eml"} {
		res := e.call(t, "save_raw_message", map[string]any{"ref": testRef})
		wantOK(t, res)
		var out savedFileOut
		decode(t, res, &out)
		b, _ := os.ReadFile(out.Path)
		if filepath.Base(out.Path) != want || string(b) != raw || out.ContentType != "message/rfc822" {
			t.Errorf("call %d: %+v holding %q", i, out, b)
		}
	}
}

func TestFileToolAnnotations(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, &fakeDaemon{}, latest)
	res, err := e.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]string{"download_attachment": "Download an attachment", "save_raw_message": "Save the raw message"}
	seen := 0
	for _, tool := range res.Tools {
		title, ok := titles[tool.Name]
		if !ok {
			continue
		}
		seen++
		a := tool.Annotations
		if tool.Title != title || a == nil || a.Title != title || !a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint ||
			!a.IdempotentHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("%s: title %q, annotations %+v; want %q, read only, not destructive, idempotent, open world", tool.Name, tool.Title, a, title)
		}
	}
	if seen != len(titles) {
		t.Errorf("listed %d of the %d file tools", seen, len(titles))
	}
}

// TestDownloadAttachmentWrapsMessageStrings: the content type and the file
// name come from the message, and the saved path holds the sanitized name,
// which can still read as an instruction. All three reach the agent only as
// untrusted data, and the summary is fixed text.
func TestDownloadAttachmentWrapsMessageStrings(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{DownloadAttachment: func(_ context.Context, _ *chainmailv1.DownloadAttachmentRequest, st *connect.ServerStream[chainmailv1.DownloadAttachmentResponse]) error {
		hdr := &chainmailv1.AttachmentHeader{Filename: readU(readHostile + ".pdf"), ContentType: "application/pdf; x=" + readHostile, Size: 1}
		if err := st.Send(&chainmailv1.DownloadAttachmentResponse{Chunk: &chainmailv1.DownloadAttachmentResponse_Header{Header: hdr}}); err != nil {
			return err
		}
		return st.Send(&chainmailv1.DownloadAttachmentResponse{Chunk: &chainmailv1.DownloadAttachmentResponse_Data{Data: []byte("x")}})
	}}
	e := newTestEnv(t, f, latest)
	res := e.call(t, "download_attachment", map[string]any{"ref": testRef, "part": "2"})
	wantOK(t, res)
	if n := readWrappedOnly(t, res.StructuredContent, readHostile); n != 3 {
		t.Errorf("%d enveloped hostile strings, want 3 (path, content_type, filename)", n)
	}
	readInsideMarkers(t, textOf(res), readHostile)
}
