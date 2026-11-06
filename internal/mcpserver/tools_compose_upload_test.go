package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
)

func TestComposeUploadsAttachmentsFirst(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "q3.pdf")
	if err := os.WriteFile(file, []byte("%PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "chart.png")
	if err := os.WriteFile(img, []byte("\x89PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	var (
		mu sync.Mutex
		n  int
	)
	f := &fakeDaemon{UploadAttachment: func(_ context.Context, st *connect.ClientStream[chainmailv1.UploadAttachmentRequest]) (*chainmailv1.UploadAttachmentResponse, error) {
		size := 0
		for st.Receive() {
			size += len(st.Msg().GetData())
		}
		mu.Lock()
		n++
		handle := fmt.Sprintf("h%d", n)
		mu.Unlock()
		return &chainmailv1.UploadAttachmentResponse{Handle: handle, Sha256: fmt.Sprintf("%064x", size)}, st.Err()
	}}
	e := newTestEnv(t, f, latest)
	e.call(t, "send_email", map[string]any{
		"account": "work", "to": []string{"a@x.test"}, "subject": "Q3", "body": "![c](chart.png)",
		"attachments": []string{file}, "inline": []map[string]string{{"name": "chart.png", "path": img}},
	})
	got := e.fake.lastCall(t, chainmailv1connect.ComposeServiceSendProcedure).Msg.(*chainmailv1.SendRequest).GetSpec().GetContent()
	if strings.Join(got.GetAttachments(), ",") != "h1" || len(got.GetInline()) != 1 || got.GetInline()[0].GetUpload() != "h2" || got.GetInline()[0].GetName() != "chart.png" {
		t.Errorf("content = %v", got)
	}
	calls := e.fake.Calls()
	if calls[len(calls)-1].Procedure != chainmailv1connect.ComposeServiceSendProcedure {
		t.Errorf("Send was not the last call: %+v", calls)
	}
}
