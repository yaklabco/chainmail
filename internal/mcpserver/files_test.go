package mcpserver

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
)

func TestSanitizeName(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 300) + ".pdf"
	tests := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\x\evil.exe`, "evil.exe"},
		{".bashrc", "bashrc"},
		{"...hidden.txt", "hidden.txt"},
		{"bad\x00na\x1fme\x7f.txt", "badname.txt"},
		{"invoice\u202Efdp.exe", "invoicefdp.exe"},
		{"\xff\xfe.txt", "txt"},
		{"  spaced.txt  ", "spaced.txt"},
		{"", "attachment"},
		{"..", "attachment"},
		{"dir/", "attachment"},
		{long, strings.Repeat("a", 196) + ".pdf"},
	}
	for _, tt := range tests {
		if got := sanitizeName(tt.in, "attachment"); got != tt.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSaveFileNeverReplaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(s string) func(io.Writer) (int64, error) {
		return func(w io.Writer) (int64, error) { n, err := io.WriteString(w, s); return int64(n), err }
	}
	tests := []struct{ name, content, want string }{
		{name: "a.pdf", content: "one", want: "a.pdf"},
		{name: "a.pdf", content: "two", want: "a (2).pdf"},
		{name: "a.pdf", content: "three", want: "a (3).pdf"},
		{name: "notes", content: "x", want: "notes"},
		{name: "notes", content: "y", want: "notes (2)"},
	}
	for _, tt := range tests {
		path, n, err := saveFile(dir, tt.name, write(tt.content))
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(path) != tt.want || n != int64(len(tt.content)) {
			t.Errorf("saved %s (%d bytes), want %s", path, n, tt.want)
		}
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, %v; want 0600", path, st.Mode(), err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.pdf")); string(b) != "one" {
		t.Errorf("a.pdf was replaced: %q", b)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestOutputDir(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	file := filepath.Join(base, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(base, "cache", "chainmail", "attachments")
	s := &Server{opts: Options{AttachmentsDir: def}}
	tests := []struct {
		name, dir, want string
		wantErr         bool
	}{
		{name: "default is created", dir: "", want: def},
		{name: "existing absolute dir", dir: base, want: base},
		{name: "relative", dir: "x", wantErr: true},
		{name: "missing", dir: filepath.Join(base, "nope"), wantErr: true},
		{name: "a file", dir: file, wantErr: true},
	}
	for _, tt := range tests {
		got, err := s.outputDir(tt.dir)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("%s: outputDir(%q) = %q, %v", tt.name, tt.dir, got, err)
		}
	}
	if st, err := os.Stat(def); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("default dir mode %v, %v; want 0700", st.Mode(), err)
	}
}

func TestUploadAttachments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	report := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(report, []byte("%PDF-1.7 body"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "latest.pdf")
	if err := os.Symlink(report, link); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(dir, "dirlink")
	if err := os.Symlink(dir, dirLink); err != nil {
		t.Fatal(err)
	}
	type got struct{ account, filename, data string }
	tests := []struct {
		name      string
		paths     []string
		wantErr   bool
		wantFiles []got
	}{
		{name: "none", paths: nil},
		{name: "one file", paths: []string{report}, wantFiles: []got{{"work", "report.pdf", "%PDF-1.7 body"}}},
		{name: "a symlink to a file keeps the link's name", paths: []string{link}, wantFiles: []got{{"work", "latest.pdf", "%PDF-1.7 body"}}},
		{name: "relative", paths: []string{"report.pdf"}, wantErr: true},
		{name: "missing", paths: []string{filepath.Join(dir, "nope.pdf")}, wantErr: true},
		{name: "a directory", paths: []string{dir}, wantErr: true},
		{name: "a symlink to a directory", paths: []string{dirLink}, wantErr: true},
		{name: "one bad path uploads nothing", paths: []string{report, "rel.txt"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var (
				mu      sync.Mutex
				uploads []got
			)
			f := &fakeDaemon{UploadAttachment: func(_ context.Context, st *connect.ClientStream[chainmailv1.UploadAttachmentRequest]) (*chainmailv1.UploadAttachmentResponse, error) {
				var g got
				for st.Receive() {
					if h := st.Msg().GetHeader(); h != nil {
						g.account, g.filename = h.GetAccount(), h.GetFilename()
					}
					g.data += string(st.Msg().GetData())
				}
				mu.Lock()
				uploads = append(uploads, g)
				mu.Unlock()
				return &chainmailv1.UploadAttachmentResponse{Handle: "h-" + g.filename, Sha256: "ab"}, st.Err()
			}}
			e := newTestEnv(t, f, latest)
			ups, err := e.srv.uploadAttachments(t.Context(), "work", tt.paths)
			mu.Lock()
			defer mu.Unlock()
			if tt.wantErr {
				var te *toolError
				if !errors.As(err, &te) || te.body.Reason != api.ReasonInvalidArgument {
					t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
				}
				for _, c := range e.fake.Calls() {
					if c.Procedure == chainmailv1connect.ComposeServiceUploadAttachmentProcedure {
						t.Error("a bad path still uploaded")
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(ups) != len(tt.wantFiles) || len(uploads) != len(tt.wantFiles) {
				t.Fatalf("uploads %+v, handles %+v, want %+v", uploads, ups, tt.wantFiles)
			}
			for i, w := range tt.wantFiles {
				if uploads[i] != w || ups[i].Handle != "h-"+w.filename || ups[i].SHA256 != "ab" {
					t.Errorf("upload %d = %+v / %+v, want %+v", i, uploads[i], ups[i], w)
				}
			}
		})
	}
}
