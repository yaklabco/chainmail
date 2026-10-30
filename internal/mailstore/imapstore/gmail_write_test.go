package imapstore

import (
	"errors"
	"testing"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/ref"
)

func TestGmailWritesWaitForTheirOwnImplementation(t *testing.T) {
	t.Parallel()
	s := New(testAccount("g", provider.Endpoint{Host: "127.0.0.1", Port: 1, Security: provider.SecurityTLS}, true), passwordCreds("g", "pw"), Options{})
	t.Cleanup(func() { _ = s.Close() })
	refs := []ref.Ref{{Account: "g", Folder: "[Gmail]/All Mail", UIDValidity: 1, UID: 1}}
	ctx := t.Context()
	for name, res := range map[string][]mailstore.RefResult{
		"Move": s.Move(ctx, refs, "Work"), "Copy": s.Copy(ctx, refs, "Work"), "Archive": s.Archive(ctx, refs),
		"Trash": s.Trash(ctx, refs), "Expunge": s.Expunge(ctx, refs), "StoreLabels": s.StoreLabels(ctx, refs, []string{"x"}, nil),
	} {
		if len(res) != 1 || !errors.Is(res[0].Err, mailstore.ErrUnsupported) {
			t.Errorf("%s = %+v, want ErrUnsupported", name, res)
		}
	}
}
