package mcpserver

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
)

var (
	keyRE      = regexp.MustCompile(`^mcp-[a-z2-7]{52}$`)
	errOutcome = errors.New("the server stopped answering after DATA")
)

func TestDeriveKey(t *testing.T) {
	t.Parallel()
	base := sendEmailIn{Account: "work", To: []string{"a@x.test", "b@x.test"}, Subject: "Q3", contentIn: contentIn{Body: "hi"}}
	key := func(tool string, in sendEmailIn, ups []uploadedFile) string {
		in.To, in.Cc, in.Bcc = sorted(in.To), sorted(in.Cc), sorted(in.Bcc)
		k, err := deriveKey(tool, in, ups)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	k0 := key("send_email", base, nil)
	if !keyRE.MatchString(k0) || len(k0) > maxKeyLen {
		t.Fatalf("key %q does not match %s", k0, keyRE)
	}
	reordered := base
	reordered.To = []string{"b@x.test", "a@x.test"}
	otherBody := base
	otherBody.Body = "hello"
	tests := []struct {
		name string
		got  string
		same bool
	}{
		{name: "same input", got: key("send_email", base, nil), same: true},
		{name: "recipient order does not matter", got: key("send_email", reordered, nil), same: true},
		{name: "a different body", got: key("send_email", otherBody, nil)},
		{name: "a different tool", got: key("reply", base, nil)},
		{name: "different attachment bytes", got: key("send_email", base, []uploadedFile{{Handle: "h", SHA256: "ff"}})},
	}
	for _, tt := range tests {
		if (tt.got == k0) != tt.same {
			t.Errorf("%s: key %q vs %q, want same=%t", tt.name, tt.got, k0, tt.same)
		}
	}
}

func sentResult(r *chainmailv1.ComposeResult) unaryFn[chainmailv1.SendRequest, chainmailv1.SendResponse] {
	return func(context.Context, *chainmailv1.SendRequest) (*chainmailv1.SendResponse, error) {
		return &chainmailv1.SendResponse{Result: r}, nil
	}
}

func TestSendEmailResults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		result   *chainmailv1.ComposeResult
		want     composeOut // IdempotencyKey is checked separately
		wantText []string
	}{
		{
			name:   "sent",
			result: &chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Sent{Sent: &chainmailv1.Sent{MessageId: "m1@x", SentRef: testRef2}}},
			want:   composeOut{Status: statusSent, MessageID: "m1@x", SentRef: testRef2}, wantText: []string{"Sent m1@x"},
		},
		{
			name:   "duplicate",
			result: &chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Sent{Sent: &chainmailv1.Sent{MessageId: "m1@x", Duplicate: true}}},
			want:   composeOut{Status: statusDuplicate, OriginalStatus: statusSent, MessageID: "m1@x"}, wantText: []string{"nothing was sent again"},
		},
		{
			name: "held",
			result: &chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Held{Held: &chainmailv1.Held{
				Draft: &chainmailv1.Draft{Ref: testRef2, MessageId: "m2@x"}, Blocked: []string{"stranger@evil.test"}, TooManyRecipients: true,
				Hint: "held by policy: saved to Drafts for James to review and send",
			}}},
			want: composeOut{
				Status: statusHeld, DraftRef: testRef2, MessageID: "m2@x", Blocked: []string{"stranger@evil.test"},
				TooManyRecipients: boolPtr(true), Hint: "held by policy: saved to Drafts for James to review and send",
			},
			wantText: []string{"NOT sent", "Drafts", "Do not retry", "stranger@evil.test", "More recipients"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{Send: sentResult(tt.result)}, latest)
			res := e.call(t, "send_email", map[string]any{"account": "work", "to": []string{"stranger@evil.test"}, "subject": "Q3", "body": "See *this*."})
			wantOK(t, res) // a held send is a successful result, never an error
			var out composeOut
			decode(t, res, &out)
			if !keyRE.MatchString(out.IdempotencyKey) {
				t.Errorf("idempotency key = %q, want a derived key", out.IdempotencyKey)
			}
			out.IdempotencyKey = ""
			if !composeEqual(out, tt.want) {
				t.Errorf("out = %+v, want %+v", out, tt.want)
			}
			for _, w := range tt.wantText {
				if !strings.Contains(textOf(res), w) {
					t.Errorf("text %q lacks %q", textOf(res), w)
				}
			}
		})
	}
}

// composeEqual compares two outputs field by field.
func composeEqual(a, b composeOut) bool {
	tooMany := func(p *bool) bool { return p != nil && *p }
	return a.Status == b.Status && a.OriginalStatus == b.OriginalStatus && a.MessageID == b.MessageID &&
		a.SentRef == b.SentRef && a.DraftRef == b.DraftRef && strings.Join(a.Blocked, ",") == strings.Join(b.Blocked, ",") &&
		tooMany(a.TooManyRecipients) == tooMany(b.TooManyRecipients) && a.Hint == b.Hint
}

func TestComposeRequests(t *testing.T) {
	t.Parallel()
	md := func(s string) *chainmailv1.ComposeBody {
		return &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Markdown{Markdown: s}}
	}
	tests := []struct {
		name string
		tool string
		args map[string]any
		proc string
		want proto.Message
	}{
		{
			name: "send_email with an explicit key", tool: "send_email",
			args: map[string]any{"account": "work", "to": []string{"a@x.test"}, "cc": []string{"c@x.test"}, "subject": "Hi", "body": "yo", "idempotency_key": "k-1", "headers": map[string]string{"Reply-To": "r@x.test"}},
			proc: chainmailv1connect.ComposeServiceSendProcedure,
			want: &chainmailv1.SendRequest{IdempotencyKey: "k-1", Spec: &chainmailv1.ComposeSpec{
				Account: "work", To: []string{"a@x.test"}, Cc: []string{"c@x.test"}, Subject: "Hi",
				Content: &chainmailv1.Content{Body: md("yo"), Headers: map[string]string{"Reply-To": "r@x.test"}},
			}},
		},
		{
			name: "reply all as plain text", tool: "reply",
			args: map[string]any{"ref": testRef, "all": true, "body": "ok", "body_format": "text", "idempotency_key": "k-2"},
			proc: chainmailv1connect.ComposeServiceReplyProcedure,
			want: &chainmailv1.ReplyRequest{
				Ref: testRef, All: true, IdempotencyKey: "k-2",
				Content: &chainmailv1.Content{Body: &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Text{Text: "ok"}}},
			},
		},
		{
			name: "reply as a draft sends no key", tool: "reply",
			args: map[string]any{"ref": testRef, "body": "later", "draft": true},
			proc: chainmailv1connect.ComposeServiceReplyProcedure,
			want: &chainmailv1.ReplyRequest{Ref: testRef, Draft: true, Content: &chainmailv1.Content{Body: md("later")}},
		},
		{
			name: "forward without attachments, html", tool: "forward",
			args: map[string]any{"ref": testRef, "to": []string{"z@x.test"}, "body": "<p>FYI</p>", "body_format": "html", "html_text": "FYI", "no_attachments": true, "idempotency_key": "k-3"},
			proc: chainmailv1connect.ComposeServiceForwardProcedure,
			want: &chainmailv1.ForwardRequest{
				Ref: testRef, To: []string{"z@x.test"}, NoAttachments: true, IdempotencyKey: "k-3",
				Content: &chainmailv1.Content{Body: &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Html{Html: "<p>FYI</p>"}, HtmlText: "FYI"}},
			},
		},
		{
			name: "create_draft", tool: "create_draft",
			args: map[string]any{"account": "work", "to": []string{"a@x.test"}, "subject": "Draft", "body": "wip", "no_signature": true},
			proc: chainmailv1connect.ComposeServiceCreateDraftProcedure,
			want: &chainmailv1.CreateDraftRequest{Spec: &chainmailv1.ComposeSpec{
				Account: "work", To: []string{"a@x.test"}, Subject: "Draft",
				Content: &chainmailv1.Content{Body: md("wip"), NoSignature: true},
			}},
		},
		{
			name: "update_draft takes the account from the ref", tool: "update_draft",
			args: map[string]any{"ref": testRef, "subject": "v2", "body": "new"},
			proc: chainmailv1connect.ComposeServiceUpdateDraftProcedure,
			want: &chainmailv1.UpdateDraftRequest{Ref: testRef, Spec: &chainmailv1.ComposeSpec{Account: "work", Subject: "v2", Content: &chainmailv1.Content{Body: md("new")}}},
		},
		{
			name: "send_draft with an explicit key", tool: "send_draft", args: map[string]any{"ref": testRef, "idempotency_key": "k-4"},
			proc: chainmailv1connect.ComposeServiceSendDraftProcedure, want: &chainmailv1.SendDraftRequest{Ref: testRef, IdempotencyKey: "k-4"},
		},
		{
			name: "delete_draft", tool: "delete_draft", args: map[string]any{"ref": testRef},
			proc: chainmailv1connect.ComposeServiceDeleteDraftProcedure, want: &chainmailv1.DeleteDraftRequest{Ref: testRef},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, latest)
			e.call(t, tt.tool, tt.args)
			if got := e.fake.lastCall(t, tt.proc).Msg; !proto.Equal(got, tt.want) {
				t.Errorf("request = %v\nwant %v", got, tt.want)
			}
		})
	}
}

func TestDerivedKeysReachTheDaemon(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, &fakeDaemon{}, latest)
	args := map[string]any{"account": "work", "to": []string{"b@x.test", "a@x.test"}, "subject": "Hi", "body": "yo"}
	e.call(t, "send_email", args)
	k1 := e.fake.lastCall(t, chainmailv1connect.ComposeServiceSendProcedure).Msg.(*chainmailv1.SendRequest).GetIdempotencyKey()
	args["to"] = []string{"a@x.test", "b@x.test"}
	e.call(t, "send_email", args)
	k2 := e.fake.lastCall(t, chainmailv1connect.ComposeServiceSendProcedure).Msg.(*chainmailv1.SendRequest).GetIdempotencyKey()
	if !keyRE.MatchString(k1) || k1 != k2 {
		t.Errorf("keys %q and %q, want one derived key for both orders", k1, k2)
	}
	e.call(t, "send_draft", map[string]any{"ref": testRef})
	if k := e.fake.lastCall(t, chainmailv1connect.ComposeServiceSendDraftProcedure).Msg.(*chainmailv1.SendDraftRequest).GetIdempotencyKey(); !keyRE.MatchString(k) {
		t.Errorf("send_draft key = %q", k)
	}
}

func TestComposeInputErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{name: "an unknown body format", tool: "send_email", args: map[string]any{"account": "work", "to": []string{"a@x.test"}, "subject": "s", "body": "b", "body_format": "rtf"}},
		{name: "html_text without html", tool: "send_email", args: map[string]any{"account": "work", "to": []string{"a@x.test"}, "subject": "s", "body": "b", "html_text": "t"}},
		{name: "a ref that is not a ref", tool: "reply", args: map[string]any{"ref": "not-a-ref", "body": "b"}},
		{name: "a relative attachment", tool: "send_email", args: map[string]any{"account": "work", "to": []string{"a@x.test"}, "subject": "s", "attachments": []string{"q3.pdf"}}},
		{name: "a missing required field", tool: "forward", args: map[string]any{"ref": testRef}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, latest)
			wantError(t, e.call(t, tt.tool, tt.args), api.ReasonInvalidArgument)
			for _, c := range e.fake.Calls() {
				if strings.HasPrefix(c.Procedure, "/chainmail.v1.ComposeService/") {
					t.Errorf("bad input reached the daemon: %s", c.Procedure)
				}
			}
		})
	}
}

func TestComposeDaemonErrors(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{SendDraft: func(context.Context, *chainmailv1.SendDraftRequest) (*chainmailv1.SendDraftResponse, error) {
		return nil, api.New(connect.CodeUnknown, api.ReasonSendOutcomeUnknown, "search Sent before sending again", errOutcome)
	}}
	e := newTestEnv(t, f, latest)
	if b := wantError(t, e.call(t, "send_draft", map[string]any{"ref": testRef}), api.ReasonSendOutcomeUnknown); !strings.Contains(b.Hint, "Sent") {
		t.Errorf("hint = %q", b.Hint)
	}
}

func TestComposeToolAnnotations(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, &fakeDaemon{}, latest)
	res, err := e.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		title      string
		idempotent bool
	}{
		"send_email":   {title: "Send an email"},
		"reply":        {title: "Reply to a message"},
		"forward":      {title: "Forward a message"},
		"create_draft": {title: "Create a draft"},
		"update_draft": {title: "Update a draft"},
		"send_draft":   {title: "Send a draft"},
		"delete_draft": {title: "Delete a draft", idempotent: true},
	}
	seen := 0
	for _, tool := range res.Tools {
		want, ok := tests[tool.Name]
		if !ok {
			continue
		}
		seen++
		a := tool.Annotations
		if tool.Title != want.title || a == nil || a.Title != want.title || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint ||
			a.IdempotentHint != want.idempotent || a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("%s: title %q, annotations %+v; want %q, not read only, not destructive, idempotent=%t, open world",
				tool.Name, tool.Title, a, want.title, want.idempotent)
		}
	}
	if seen != len(tests) {
		t.Errorf("listed %d of the %d compose tools", seen, len(tests))
	}
}
