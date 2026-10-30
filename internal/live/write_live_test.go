//go:build live

package live

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/client"
)

// These tests change real mailboxes, so they touch only what they create:
// messages whose subject carries a random token, sent by each account to
// its own address and to no other, and a chainmail-test-<random> folder or
// label. Every test removes what it made with t.Cleanup, even when it
// fails. Like the read tests, they log counts, refs and folder names, never
// subjects, addresses or bodies.
//
// They send mail and probe connection limits, so they run only when
// CHAINMAIL_LIVE_WRITES=1 is set as well as the live tag.

const (
	// writesEnv opts in to the tests that send, organize and probe.
	writesEnv = "CHAINMAIL_LIVE_WRITES"
	// deliveryTimeout bounds the wait for a message sent to self to arrive.
	deliveryTimeout = 3 * time.Minute
	// settleTimeout is how long cleanup keeps looking for a message whose
	// delivery the test never saw, after the last send.
	settleTimeout = 90 * time.Second
	// cleanupTimeout bounds each cleanup.
	cleanupTimeout = 5 * time.Minute
	// pollInterval is the pause between searches while waiting.
	pollInterval = 5 * time.Second
	// testFolderPrefix starts the name of the folder or label the organize
	// test creates.
	testFolderPrefix = "chainmail-test-"
	// maxSweepPages bounds how far a cleanup search pages.
	maxSweepPages = 10
)

// requireWrites skips a test that changes mail unless writesEnv is 1.
func requireWrites(t *testing.T) {
	t.Helper()
	if os.Getenv(writesEnv) != "1" {
		t.Skipf("set %s=1 to send mail to each account's own address, organize it and probe connection limits", writesEnv)
	}
}

// isGoogle says the account is on Gmail, where refs live in All Mail and
// folders are labels.
func isGoogle(a *chainmailv1.Account) bool {
	return a.GetProvider() == "gmail" || a.GetProvider() == "google-workspace"
}

// randomToken returns n lowercase base32 characters from crypto/rand.
func randomToken(n int) string {
	return strings.ToLower(rand.Text()[:n])
}

// liveKey derives the idempotency key of one send step from the run's token.
func liveKey(token, step string) string { return "live-" + token + "-" + step }

func reasonOf(err error) string { return api.Detail(err).GetReason() }

// trimID drops the angle brackets around a Message-ID, if any.
func trimID(id string) string { return strings.TrimSuffix(strings.TrimPrefix(id, "<"), ">") }

func term(field, value string) *chainmailv1.Query {
	return &chainmailv1.Query{Node: &chainmailv1.Query_Term{Term: &chainmailv1.QueryTerm{Field: field, Value: value}}}
}

// tokenQuery matches messages whose subject holds token, in folder in when
// it is set ("anywhere" includes Trash and Spam).
func tokenQuery(token, in string) *chainmailv1.Query {
	nodes := []*chainmailv1.Query{term("subject", token)}
	if in != "" {
		nodes = append(nodes, term("in", in))
	}
	return &chainmailv1.Query{Node: &chainmailv1.Query_And{And: &chainmailv1.QueryList{Nodes: nodes}}}
}

// tokenRows returns every row of the account's search whose subject holds
// token. The subject check is redundant with the query, and is what keeps
// a cleanup from touching any message the test did not create.
func tokenRows(ctx context.Context, c *client.Client, account, token, in string) ([]*chainmailv1.MessageRow, error) {
	var out []*chainmailv1.MessageRow
	cursor := ""
	for range maxSweepPages {
		resp, err := c.Message.Search(ctx, connect.NewRequest(&chainmailv1.SearchRequest{
			Query: tokenQuery(token, in), Accounts: []string{account}, Limit: 100, Cursor: cursor,
		}))
		if err != nil {
			return nil, fmt.Errorf("search: %w (%s)", err, hint(err))
		}
		if p := resp.Msg.GetPartial(); len(p) > 0 {
			return nil, fmt.Errorf("search could not read folder %q: %s", p[0].GetFolder(), p[0].GetReason())
		}
		for _, r := range resp.Msg.GetRows() {
			if strings.Contains(r.GetSubject().GetUntrusted(), token) {
				out = append(out, r)
			}
		}
		if cursor = resp.Msg.GetNextCursor(); cursor == "" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("search: still more rows after %d pages", maxSweepPages)
}

// getMessages reads refs and returns the messages that could be read.
func getMessages(ctx context.Context, c *client.Client, refs []string) ([]*chainmailv1.Message, error) {
	var out []*chainmailv1.Message
	for chunk := range slices.Chunk(refs, 50) {
		resp, err := c.Message.Get(ctx, connect.NewRequest(&chainmailv1.GetRequest{Refs: chunk}))
		if err != nil {
			return nil, fmt.Errorf("get: %w (%s)", err, hint(err))
		}
		for _, it := range resp.Msg.GetItems() {
			if m := it.GetMessage(); m != nil {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// findMessage waits up to deliveryTimeout for a message with Message-ID id
// and token in its subject, in folder in, that accept takes. It returns the
// message's ref.
func findMessage(t *testing.T, c *client.Client, account, token, in, id string, accept func(*chainmailv1.Message) bool) string {
	t.Helper()
	deadline := time.Now().Add(deliveryTimeout)
	start := time.Now()
	for {
		rows, err := tokenRows(t.Context(), c, account, token, in)
		if err != nil {
			t.Fatalf("waiting for %s in %q: %v", id, in, err)
		}
		refs := make([]string, 0, len(rows))
		for _, r := range rows {
			refs = append(refs, r.GetRef())
		}
		msgs, err := getMessages(t.Context(), c, refs)
		if err != nil {
			t.Fatalf("waiting for %s in %q: %v", id, in, err)
		}
		for _, m := range msgs {
			if trimID(m.GetMessageId().GetUntrusted()) == trimID(id) && (accept == nil || accept(m)) {
				t.Logf("found %s as %s in %q after %v", id, m.GetRef(), m.GetFolder(), time.Since(start).Round(time.Second))
				return m.GetRef()
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear in %q within %v (%d candidate rows)", id, in, deliveryTimeout, len(rows))
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("waiting for %s: %v", id, t.Context().Err())
		case <-time.After(pollInterval):
		}
	}
}

// folderRoles maps each of the account's folders to its role.
func folderRoles(t *testing.T, c *client.Client, account string) map[string]string {
	t.Helper()
	resp, err := c.Message.ListFolders(t.Context(), connect.NewRequest(&chainmailv1.ListFoldersRequest{Accounts: []string{account}}))
	if err != nil {
		t.Fatalf("ListFolders: %v (%s)", err, hint(err))
	}
	reportPartial(t, "ListFolders", resp.Msg.GetPartial())
	roles := map[string]string{}
	for _, af := range resp.Msg.GetAccounts() {
		for _, f := range af.GetFolders() {
			roles[f.GetName()] = f.GetRole()
		}
	}
	return roles
}

// received accepts the delivered copy of a message sent to self: on Google
// the one message in All Mail, elsewhere a copy outside Sent and Drafts.
// A draft that shares the Message-ID, as one being sent does, is not it.
func received(a *chainmailv1.Account, roles map[string]string) func(*chainmailv1.Message) bool {
	return func(m *chainmailv1.Message) bool {
		if slices.Contains(m.GetFlags(), `\Draft`) {
			return false
		}
		if isGoogle(a) {
			return true
		}
		role := roles[m.GetFolder()]
		return role != "sent" && role != "drafts"
	}
}

// sweeper removes every message carrying a test's token. pending counts
// sends whose delivery the test has not seen; while any are pending,
// cleanup keeps looking until settleTimeout after the last send, so a
// message that arrives late is still removed.
type sweeper struct {
	c       *client.Client
	account string
	token   string
	pending int
	last    time.Time
}

// newSweeper registers a cleanup that expunges every message of the
// account whose subject holds token.
func newSweeper(t *testing.T, c *client.Client, account, token string) *sweeper {
	t.Helper()
	s := &sweeper{c: c, account: account, token: token}
	// t.Context is cancelled before cleanups run, so the cleanup keeps its
	// values but drops the cancellation.
	base := context.WithoutCancel(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(base, cleanupTimeout)
		defer cancel()
		if err := s.sweep(ctx, t); err != nil {
			t.Errorf("cleanup on %s: %v; search subject:%s in:anywhere and remove what is left", account, err, token)
		}
	})
	return s
}

// sending is called before each send attempt.
func (s *sweeper) sending() {
	s.pending++
	s.last = time.Now()
}

// delivered is called once the test has seen a send arrive.
func (s *sweeper) delivered() {
	if s.pending > 0 {
		s.pending--
	}
}

// gone says a ref no longer names a message, which cleanup takes as done.
func gone(e *chainmailv1.ItemError) bool {
	return e.GetReason() == api.ReasonNotFound || e.GetReason() == api.ReasonStaleRef
}

// sweep expunges the token's messages until none are left and no
// delivery is still pending.
func (s *sweeper) sweep(ctx context.Context, t *testing.T) error {
	removed := 0
	for {
		rows, err := tokenRows(ctx, s.c, s.account, s.token, "anywhere")
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			refs := make([]string, 0, len(rows))
			for _, r := range rows {
				refs = append(refs, r.GetRef())
			}
			resp, err := s.c.Organize.Expunge(ctx, connect.NewRequest(&chainmailv1.ExpungeRequest{Refs: refs}))
			if err != nil {
				return fmt.Errorf("expunge: %w (%s)", err, hint(err))
			}
			for _, o := range resp.Msg.GetOutcomes() {
				if e := o.GetError(); e != nil && !gone(e) {
					return fmt.Errorf("expunge %s: %s: %s", o.GetRef(), e.GetReason(), e.GetMessage())
				}
			}
			removed += len(refs)
			continue
		}
		if s.pending == 0 || time.Since(s.last) >= settleTimeout {
			t.Logf("cleanup on %s: expunged %d messages", s.account, removed)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %d late deliveries: %w", s.pending, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// selfSpec is a Markdown message from the account to its own address.
func selfSpec(a *chainmailv1.Account, subject, body string) *chainmailv1.ComposeSpec {
	return &chainmailv1.ComposeSpec{
		Account: a.GetName(), To: []string{a.GetAddress()}, Subject: subject,
		Content: liveContent(body),
	}
}

func liveContent(body string) *chainmailv1.Content {
	return &chainmailv1.Content{
		Body:        &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Markdown{Markdown: body}},
		NoSignature: true,
	}
}

// sentOf checks a send-type result and returns what was sent.
func sentOf(t *testing.T, call string, res *chainmailv1.ComposeResult) *chainmailv1.Sent {
	t.Helper()
	s := res.GetSent()
	if s == nil {
		t.Fatalf("%s: the result is not a sent message", call)
	}
	if s.GetMessageId() == "" {
		t.Fatalf("%s: no Message-ID", call)
	}
	if s.GetSentCopyError() != "" {
		t.Errorf("%s: sent, but the Sent copy failed: %s", call, s.GetSentCopyError())
	}
	t.Logf("%s: sent %s, Sent copy %q, duplicate %v", call, s.GetMessageId(), s.GetSentRef(), s.GetDuplicate())
	return s
}

// hasFlag reads ref and says whether it has flag.
func hasFlag(t *testing.T, c *client.Client, ref, flag string) bool {
	t.Helper()
	return slices.Contains(getOne(t, c, ref, false).GetFlags(), flag)
}

// TestEveryAccountSends sends a message from each account to its own
// address, checks a repeat with the same key sends nothing, then replies to
// it, forwards it, and creates, updates, sends and deletes drafts.
func TestEveryAccountSends(t *testing.T) {
	requireWrites(t)
	c := liveClient(t)
	for _, a := range liveAccounts(t, c) {
		t.Run(a.GetName(), func(t *testing.T) {
			token := "cmlive" + randomToken(16)
			t.Logf("token %s", token)
			sw := newSweeper(t, c, a.GetName(), token)
			roles := folderRoles(t, c, a.GetName())
			subject := "chainmail live test " + token
			spec := selfSpec(a, subject, "A **chainmail** live test. It deletes itself.")

			dry, err := c.Compose.Send(t.Context(), connect.NewRequest(&chainmailv1.SendRequest{Spec: spec, DryRun: true}))
			if err != nil {
				t.Fatalf("Send dry run: %v (%s)", err, hint(err))
			}
			d := dry.Msg.GetResult().GetDryRun()
			if d == nil || len(d.GetRaw()) == 0 {
				t.Fatal("Send dry run: no message built")
			}
			if !slices.Equal(d.GetEnvelopeTo(), []string{a.GetAddress()}) {
				t.Fatalf("Send dry run: %d envelope recipients, want only the account's own address", len(d.GetEnvelopeTo()))
			}
			t.Logf("dry run: %d bytes", len(d.GetRaw()))

			req := &chainmailv1.SendRequest{Spec: spec, IdempotencyKey: liveKey(token, "send")}
			sw.sending()
			resp, err := c.Compose.Send(t.Context(), connect.NewRequest(req))
			if err != nil {
				t.Fatalf("Send: %v (%s)", err, hint(err))
			}
			sent := sentOf(t, "Send", resp.Msg.GetResult())
			if sent.GetDuplicate() {
				t.Fatal("Send: the first send is marked duplicate")
			}

			again, err := c.Compose.Send(t.Context(), connect.NewRequest(req))
			if err != nil {
				t.Fatalf("Send again with the same key: %v (%s)", err, hint(err))
			}
			dup := again.Msg.GetResult().GetSent()
			if !dup.GetDuplicate() || dup.GetMessageId() != sent.GetMessageId() {
				t.Errorf("Send again with the same key: duplicate %v, Message-ID %s, want duplicate of %s",
					dup.GetDuplicate(), dup.GetMessageId(), sent.GetMessageId())
			}

			orig := findMessage(t, c, a.GetName(), token, "", sent.GetMessageId(), received(a, roles))
			sw.delivered()

			t.Run("reply", func(t *testing.T) {
				// The original is the account's own message to itself, so
				// the reply names the account's address as its recipient.
				sw.sending()
				resp, err := c.Compose.Reply(t.Context(), connect.NewRequest(&chainmailv1.ReplyRequest{
					Ref: orig, Content: liveContent("A live test reply."), To: []string{a.GetAddress()},
					IdempotencyKey: liveKey(token, "reply"),
				}))
				if err != nil {
					t.Fatalf("Reply: %v (%s)", err, hint(err))
				}
				s := sentOf(t, "Reply", resp.Msg.GetResult())
				got := findMessage(t, c, a.GetName(), token, "", s.GetMessageId(), received(a, roles))
				sw.delivered()
				m := getOne(t, c, got, false)
				if irt := m.GetInReplyTo(); len(irt) != 1 || trimID(irt[0].GetUntrusted()) != trimID(sent.GetMessageId()) {
					t.Errorf("the reply's In-Reply-To does not name the original %s", sent.GetMessageId())
				}
				if !hasFlag(t, c, orig, `\Answered`) {
					t.Errorf(`the original %s is not \Answered after the reply`, orig)
				}
			})

			t.Run("forward", func(t *testing.T) {
				sw.sending()
				resp, err := c.Compose.Forward(t.Context(), connect.NewRequest(&chainmailv1.ForwardRequest{
					Ref: orig, To: []string{a.GetAddress()}, Content: liveContent("A live test forward."),
					IdempotencyKey: liveKey(token, "forward"),
				}))
				if err != nil {
					t.Fatalf("Forward: %v (%s)", err, hint(err))
				}
				s := sentOf(t, "Forward", resp.Msg.GetResult())
				findMessage(t, c, a.GetName(), token, "", s.GetMessageId(), received(a, roles))
				sw.delivered()
				if !hasFlag(t, c, orig, "$Forwarded") {
					t.Errorf("the original %s is not $Forwarded after the forward", orig)
				}
			})

			t.Run("draft", func(t *testing.T) {
				created, err := c.Compose.CreateDraft(t.Context(), connect.NewRequest(&chainmailv1.CreateDraftRequest{
					Spec: selfSpec(a, "chainmail live draft "+token, "First version."),
				}))
				if err != nil {
					t.Fatalf("CreateDraft: %v (%s)", err, hint(err))
				}
				first := created.Msg.GetDraft()
				t.Logf("draft created: %s", first.GetRef())

				updated, err := c.Compose.UpdateDraft(t.Context(), connect.NewRequest(&chainmailv1.UpdateDraftRequest{
					Ref: first.GetRef(), Spec: selfSpec(a, "chainmail live draft "+token, "Second version."),
				}))
				if err != nil {
					t.Fatalf("UpdateDraft %s: %v (%s)", first.GetRef(), err, hint(err))
				}
				second := updated.Msg.GetDraft()
				if second.GetRef() == "" || second.GetRef() == first.GetRef() {
					t.Fatalf("UpdateDraft: new ref %q, want a new ref", second.GetRef())
				}
				t.Logf("draft updated: %s", second.GetRef())

				sw.sending()
				sendResp, err := c.Compose.SendDraft(t.Context(), connect.NewRequest(&chainmailv1.SendDraftRequest{
					Ref: second.GetRef(), IdempotencyKey: liveKey(token, "draft-send"),
				}))
				if err != nil {
					t.Fatalf("SendDraft %s: %v (%s)", second.GetRef(), err, hint(err))
				}
				s := sentOf(t, "SendDraft", sendResp.Msg.GetResult())
				findMessage(t, c, a.GetName(), token, "", s.GetMessageId(), received(a, roles))
				sw.delivered()

				doomed, err := c.Compose.CreateDraft(t.Context(), connect.NewRequest(&chainmailv1.CreateDraftRequest{
					Spec: selfSpec(a, "chainmail live draft to delete "+token, "Deleted unsent."),
				}))
				if err != nil {
					t.Fatalf("CreateDraft: %v (%s)", err, hint(err))
				}
				dref := doomed.Msg.GetDraft().GetRef()
				if _, err := c.Compose.DeleteDraft(t.Context(), connect.NewRequest(&chainmailv1.DeleteDraftRequest{Ref: dref})); err != nil {
					t.Fatalf("DeleteDraft %s: %v (%s)", dref, err, hint(err))
				}
				left, err := tokenRows(t.Context(), c, a.GetName(), token, "drafts")
				if err != nil {
					t.Fatalf("searching Drafts: %v", err)
				}
				if len(left) != 0 {
					t.Errorf("%d drafts left after sending one and deleting the other", len(left))
				}
			})
		})
	}
}

// outcomesOf returns a batch organize call's outcomes, or its error.
func outcomesOf[T any, PT interface {
	*T
	GetOutcomes() []*chainmailv1.RefOutcome
}](resp *connect.Response[T], err error) ([]*chainmailv1.RefOutcome, error) {
	if err != nil {
		return nil, err
	}
	return PT(resp.Msg).GetOutcomes(), nil
}

// outcome checks the result of an organize call on one ref and fails the
// test if the call or its outcome failed.
func outcome(t *testing.T, call string, outs []*chainmailv1.RefOutcome, err error) *chainmailv1.RefOutcome {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v (%s)", call, err, hint(err))
	}
	if len(outs) != 1 {
		t.Fatalf("%s: %d outcomes, want 1", call, len(outs))
	}
	if e := outs[0].GetError(); e != nil {
		t.Fatalf("%s %s: %s: %s (%s)", call, outs[0].GetRef(), e.GetReason(), e.GetMessage(), e.GetHint())
	}
	t.Logf("%s %s: new ref %q", call, outs[0].GetRef(), outs[0].GetNewRef())
	return outs[0]
}

// TestEveryAccountOrganizes sends one message from each account to itself
// and takes it through a chainmail-test-<random> folder (a label on
// Google): copy, flag, unflag, labels, move, archive, trash and expunge,
// then deletes the folder.
func TestEveryAccountOrganizes(t *testing.T) {
	requireWrites(t)
	c := liveClient(t)
	for _, a := range liveAccounts(t, c) {
		t.Run(a.GetName(), func(t *testing.T) {
			google := isGoogle(a)
			name := a.GetName()
			folder := testFolderPrefix + randomToken(12)
			token := "cmlive" + randomToken(16)
			t.Logf("folder %s, token %s", folder, token)

			if _, err := c.Organize.CreateFolder(t.Context(), connect.NewRequest(&chainmailv1.CreateFolderRequest{Account: name, Name: folder})); err != nil {
				t.Fatalf("CreateFolder %s: %v (%s)", folder, err, hint(err))
			}
			base := context.WithoutCancel(t.Context())
			// Registered before the sweeper, so it runs after it. The
			// folder is the test's own, with a random name, so whatever is
			// still in it was put there by the test: force is safe.
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(base, cleanupTimeout)
				defer cancel()
				_, err := c.Organize.DeleteFolder(ctx, connect.NewRequest(&chainmailv1.DeleteFolderRequest{Account: name, Name: folder, Force: true}))
				if err != nil && reasonOf(err) != api.ReasonNotFound {
					t.Errorf("cleanup on %s: DeleteFolder %s: %v (%s); delete it by hand", name, folder, err, hint(err))
				}
			})
			sw := newSweeper(t, c, name, token)
			roles := folderRoles(t, c, name)
			if _, ok := roles[folder]; !ok {
				t.Fatalf("%s is not listed after CreateFolder", folder)
			}

			sw.sending()
			resp, err := c.Compose.Send(t.Context(), connect.NewRequest(&chainmailv1.SendRequest{
				Spec:           selfSpec(a, "chainmail live organize "+token, "A chainmail live organize test. It deletes itself."),
				IdempotencyKey: liveKey(token, "organize"),
			}))
			if err != nil {
				t.Fatalf("Send: %v (%s)", err, hint(err))
			}
			id := sentOf(t, "Send", resp.Msg.GetResult()).GetMessageId()
			m := findMessage(t, c, name, token, "", id, received(a, roles))
			sw.delivered()

			// next is the message's ref after a call: new_ref when set, the
			// same ref on Google when the call only edits labels, or else
			// the copy found by searching in.
			next := func(o *chainmailv1.RefOutcome, in string, exclude ...string) string {
				t.Helper()
				if o.GetNewRef() != "" {
					return o.GetNewRef()
				}
				if google && in != "trash" {
					return o.GetRef()
				}
				return findMessage(t, c, name, token, in, id, func(m *chainmailv1.Message) bool {
					return !slices.Contains(exclude, m.GetRef())
				})
			}

			cpOuts, err := outcomesOf(c.Organize.Copy(t.Context(), connect.NewRequest(&chainmailv1.CopyRequest{Refs: []string{m}, Folder: folder})))
			cp := next(outcome(t, "Copy", cpOuts, err), folder)
			findMessage(t, c, name, token, folder, id, nil)

			for _, step := range []struct {
				name        string
				add, remove []string
				want        bool
			}{
				{"flag", []string{`\Flagged`}, nil, true},
				{"unflag", nil, []string{`\Flagged`}, false},
			} {
				outs, err := outcomesOf(c.Organize.SetFlags(t.Context(), connect.NewRequest(&chainmailv1.SetFlagsRequest{
					Refs: []string{cp}, Add: step.add, Remove: step.remove,
				})))
				outcome(t, "SetFlags "+step.name, outs, err)
				if got := hasFlag(t, c, cp, `\Flagged`); got != step.want {
					t.Errorf(`after %s, %s has \Flagged %v, want %v`, step.name, cp, got, step.want)
				}
			}

			if google {
				for _, step := range []struct {
					name        string
					add, remove []string
					want        bool
				}{
					{"remove", nil, []string{folder}, false},
					{"add", []string{folder}, nil, true},
				} {
					outs, err := outcomesOf(c.Organize.SetLabels(t.Context(), connect.NewRequest(&chainmailv1.SetLabelsRequest{
						Refs: []string{m}, Add: step.add, Remove: step.remove,
					})))
					outcome(t, "SetLabels "+step.name, outs, err)
					if got := slices.Contains(getOne(t, c, m, false).GetLabels(), folder); got != step.want {
						t.Errorf("after SetLabels %s, %s has label %s %v, want %v", step.name, m, folder, got, step.want)
					}
				}
			} else {
				// Labels are Gmail's: a standard IMAP account refuses them.
				outs, err := outcomesOf(c.Organize.SetLabels(t.Context(), connect.NewRequest(&chainmailv1.SetLabelsRequest{
					Refs: []string{m}, Add: []string{folder},
				})))
				reason := reasonOf(err)
				if err == nil && len(outs) == 1 {
					reason = outs[0].GetError().GetReason()
				}
				if reason != api.ReasonInvalidArgument {
					t.Errorf("SetLabels on a standard IMAP account: reason %q, want %s", reason, api.ReasonInvalidArgument)
				}
			}

			mvOuts, err := outcomesOf(c.Organize.Move(t.Context(), connect.NewRequest(&chainmailv1.MoveRequest{Refs: []string{m}, Folder: folder})))
			moved := next(outcome(t, "Move", mvOuts, err), folder, cp)
			if !google {
				msgs, err := getMessages(t.Context(), c, []string{m})
				if err != nil {
					t.Fatal(err)
				}
				if len(msgs) != 0 {
					t.Errorf("%s can still be read after moving it", m)
				}
			}

			hasArchive := slices.Contains(slices.Collect(maps.Values(roles)), "archive")
			current := moved
			arOuts, err := outcomesOf(c.Organize.Archive(t.Context(), connect.NewRequest(&chainmailv1.ArchiveRequest{Refs: []string{moved}})))
			if err == nil && !google && !hasArchive && len(arOuts) == 1 && arOuts[0].GetError().GetReason() == api.ReasonNotFound {
				t.Logf("Archive: the account has no archive folder; set archive_folder to test it")
			} else {
				current = next(outcome(t, "Archive", arOuts, err), "archive", cp)
			}

			trOuts, err := outcomesOf(c.Organize.Trash(t.Context(), connect.NewRequest(&chainmailv1.TrashRequest{Refs: []string{current}})))
			next(outcome(t, "Trash", trOuts, err), "trash")
			findMessage(t, c, name, token, "trash", id, nil)

			// Expunge every message of the test, the copy and the trashed
			// original, then check nothing is left.
			ctx, cancel := context.WithTimeout(t.Context(), cleanupTimeout)
			defer cancel()
			if err := sw.sweep(ctx, t); err != nil {
				t.Fatalf("Expunge: %v", err)
			}
			left, err := tokenRows(t.Context(), c, name, token, "anywhere")
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 0 {
				t.Errorf("%d messages left after expunging", len(left))
			}

			if _, err := c.Organize.DeleteFolder(t.Context(), connect.NewRequest(&chainmailv1.DeleteFolderRequest{Account: name, Name: folder})); err != nil {
				t.Fatalf("DeleteFolder %s: %v (%s)", folder, err, hint(err))
			}
			if _, ok := folderRoles(t, c, name)[folder]; ok {
				t.Errorf("%s is still listed after DeleteFolder", folder)
			}
		})
	}
}

// TestEveryAccountProbes measures each account's IMAP connection limit
// and checks the pool size the daemon took from it.
func TestEveryAccountProbes(t *testing.T) {
	requireWrites(t)
	c := liveClient(t)
	for _, a := range liveAccounts(t, c) {
		t.Run(a.GetName(), func(t *testing.T) {
			start := time.Now()
			resp, err := c.Account.ProbeLimits(t.Context(), connect.NewRequest(&chainmailv1.ProbeLimitsRequest{Accounts: []string{a.GetName()}}))
			if err != nil {
				t.Fatalf("ProbeLimits: %v (%s)", err, hint(err))
			}
			limits := resp.Msg.GetLimits()
			if len(limits) != 1 || limits[0].GetAccount() != a.GetName() {
				t.Fatalf("ProbeLimits: %d results, want one for %s", len(limits), a.GetName())
			}
			l := limits[0]
			if e := l.GetError(); e != nil {
				t.Fatalf("ProbeLimits: %s: %s (%s)", e.GetReason(), e.GetMessage(), e.GetHint())
			}
			if l.GetImapCap() < 1 || l.GetImapCap() > 16 {
				t.Errorf("imap_cap %d, want 1 to 16", l.GetImapCap())
			}
			if p := l.GetPoolSize(); p < 1 || p > 12 {
				t.Errorf("pool_size %d, want 1 to 12", p)
			}
			if at := l.GetProbedAt().AsTime(); at.Before(start.Add(-time.Minute)) {
				t.Errorf("probed_at %v is older than the call", at)
			}
			t.Logf("cap %d, pool %d, in %v", l.GetImapCap(), l.GetPoolSize(), time.Since(start).Round(time.Millisecond))
		})
	}
}
