package live

// How cleanup removes the messages a write test knows by ref. Gmail's
// search trails real changes by minutes, so a sweep that only searches for
// the token can find nothing and leave the test's mail behind. Like the
// folder cleanup, this needs a daemon but no real account, so this file has
// no build tag, and its tests run with the unit suite against a daemon over
// fake stores.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/client"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

// expungeTracked expunges the messages refs name whose subject holds token,
// and returns how many it removed. It reads each ref first, so it removes
// no message without the token in its subject, and a ref that no longer
// names a message is done already. A ref whose message lacks the token is
// left, and is an error, since the test tracked a message it did not make.
func expungeTracked(ctx context.Context, c *client.Client, token string, refs []string) (int, error) {
	items, err := readable(ctx, c, refs)
	var doomed []string
	for _, it := range items {
		if !strings.Contains(it.GetMessage().GetSubject().GetUntrusted(), token) {
			err = errors.Join(err, fmt.Errorf("%s has no token in its subject, so cleanup left it", it.GetRef()))
			continue
		}
		doomed = append(doomed, it.GetRef())
	}
	if len(doomed) == 0 {
		return 0, err
	}
	resp, xerr := c.Organize.Expunge(ctx, connect.NewRequest(&chainmailv1.ExpungeRequest{Refs: doomed}))
	if xerr != nil {
		return 0, errors.Join(err, fmt.Errorf("expunge: %w (%s)", xerr, hint(xerr)))
	}
	removed := 0
	for _, o := range resp.Msg.GetOutcomes() {
		switch e := o.GetError(); {
		case e == nil:
			removed++
		case !gone(e):
			err = errors.Join(err, fmt.Errorf("expunge %s: %s: %s", o.GetRef(), e.GetReason(), e.GetMessage()))
		}
	}
	return removed, err
}

// stillThere returns the refs that still name a message.
func stillThere(ctx context.Context, c *client.Client, refs []string) ([]string, error) {
	items, err := readable(ctx, c, refs)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.GetRef())
	}
	return out, err
}

// readable reads refs and returns the items that read. A ref that no longer
// names a message is left out. Any other failure is an error, since the
// message may still be there, and the items that did read are returned too.
func readable(ctx context.Context, c *client.Client, refs []string) ([]*chainmailv1.GetItem, error) {
	var out []*chainmailv1.GetItem
	var errs []error
	for chunk := range slices.Chunk(refs, 50) {
		resp, err := c.Message.Get(ctx, connect.NewRequest(&chainmailv1.GetRequest{Refs: chunk}))
		if err != nil {
			return out, errors.Join(append(errs, fmt.Errorf("get: %w (%s)", err, hint(err)))...)
		}
		for _, it := range resp.Msg.GetItems() {
			switch e := it.GetError(); {
			case e == nil && it.GetMessage() != nil:
				out = append(out, it)
			case e == nil:
				errs = append(errs, fmt.Errorf("get %s: the item has neither a message nor an error", it.GetRef()))
			case !gone(e):
				errs = append(errs, fmt.Errorf("get %s: %s: %s", it.GetRef(), e.GetReason(), e.GetMessage()))
			}
		}
	}
	return out, errors.Join(errs...)
}

// TestExpungeTracked tracks refs as a test learns them: one of a message
// still in the inbox, one already expunged, one a move made stale with the
// new ref the move gave, and, by mistake, one of a message without the
// token. Cleanup removes the token's messages by ref, takes the stale and
// expunged refs as done, and leaves the other message alone.
func TestExpungeTracked(t *testing.T) {
	t.Parallel()
	const token = "cmlivetracked1"
	st := fake.New("host", false)
	h := daemontest.Start(t, cleanupConfig, daemontest.Options{
		NewStore: func(config.ResolvedAccount) mailstore.Store { return st },
	})
	c := h.Client
	n := 0
	add := func(folder, subject string) ref.Ref {
		n++
		return st.Add(fake.Message{Folder: folder, Subject: subject, MessageID: fmt.Sprintf("<m%d@example.com>", n), Date: time.Now()})
	}
	inbox := add("INBOX", "chainmail live test "+token)
	sent := add("Sent", "chainmail live test "+token)
	expunged := add("INBOX", "chainmail live reply "+token)
	ex, err := c.Organize.Expunge(t.Context(), connect.NewRequest(&chainmailv1.ExpungeRequest{Refs: []string{expunged.String()}}))
	if err != nil || ex.Msg.GetOutcomes()[0].GetError() != nil {
		t.Fatalf("expunging ahead of cleanup: %v", err)
	}
	moved := add("INBOX", "chainmail live organize "+token)
	mv, err := c.Organize.Move(t.Context(), connect.NewRequest(&chainmailv1.MoveRequest{Refs: []string{moved.String()}, Folder: "Archive"}))
	if err != nil {
		t.Fatal(err)
	}
	archived := mv.Msg.GetOutcomes()[0].GetNewRef()
	if archived == "" {
		t.Fatal("the fake's move gave no new ref; the test proves nothing")
	}
	other := add("INBOX", "a message the test did not make")

	var k tracked
	k.track(inbox.String(), sent.String(), expunged.String(), moved.String(), archived, other.String())
	removed, err := expungeTracked(t.Context(), c, token, k.refs)
	if err == nil || !strings.Contains(err.Error(), other.String()) {
		t.Errorf("expungeTracked: %v, want an error naming %s, whose subject lacks the token", err, other)
	}
	if removed != 3 {
		t.Errorf("expungeTracked removed %d messages, want 3", removed)
	}
	for _, r := range []ref.Ref{inbox, sent} {
		if len(st.RawMessage(r)) != 0 {
			t.Errorf("%s is still there after cleanup", r)
		}
	}
	if left, err := stillThere(t.Context(), c, []string{archived}); err != nil || len(left) != 0 {
		t.Errorf("the moved message: still there %q, %v, want removed", left, err)
	}
	if len(st.RawMessage(other)) == 0 {
		t.Error("cleanup removed a message whose subject lacks the token")
	}

	// A second sweep, as the registered cleanup runs after the test's own,
	// finds every ref gone but the other message's.
	left, err := stillThere(t.Context(), c, k.refs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(left, []string{other.String()}) {
		t.Errorf("stillThere = %q, want only %s", left, other)
	}
}

// On Gmail, Trash moves the message out of All Mail, so its All Mail ref
// goes stale and the outcome's new ref names it in Trash. Cleanup removes
// it by the Trash ref and takes the All Mail ref as done.
func TestExpungeTrackedAfterTrashOnGmail(t *testing.T) {
	t.Parallel()
	const token = "cmlivetracked2"
	st := fake.New("gm", true)
	h := daemontest.Start(t, gmailCleanupConfig, daemontest.Options{
		NewStore: func(config.ResolvedAccount) mailstore.Store { return st },
	})
	c := h.Client
	all := st.Add(fake.Message{
		Folder: "[Gmail]/All Mail", Subject: "chainmail live organize " + token,
		MessageID: "<trashed@example.com>", Labels: []string{`\Sent`}, Date: time.Now(),
	})
	tr, err := c.Organize.Trash(t.Context(), connect.NewRequest(&chainmailv1.TrashRequest{Refs: []string{all.String()}}))
	if err != nil {
		t.Fatal(err)
	}
	trashed := tr.Msg.GetOutcomes()[0].GetNewRef()
	if trashed == "" {
		t.Fatal("the fake's trash gave no new ref; the test proves nothing")
	}

	var k tracked
	k.track(all.String(), trashed)
	n, err := expungeTracked(t.Context(), c, token, k.refs)
	if err != nil {
		t.Fatalf("expungeTracked: %v, want nil when a tracked ref is already gone", err)
	}
	if n != 1 {
		t.Errorf("expungeTracked removed %d messages, want 1", n)
	}
	if left, err := stillThere(t.Context(), c, k.refs); err != nil || len(left) != 0 {
		t.Errorf("stillThere = %q, %v, want nothing left", left, err)
	}
}

const gmailCleanupConfig = `
[accounts.gm]
provider = "gmail"
address = "me@gmail.example"
auth = "password"
`
