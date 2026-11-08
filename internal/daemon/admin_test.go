package daemon_test

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/daemon"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
	"github.com/yaklabco/chainmail/internal/testutil/oauthtest"
)

const hostingerConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"
`

const gmailConfig = `
[oauth_clients.g]
client_id = "client-id"

[accounts.gm]
provider = "gmail"
address = "me@gmail.com"
auth = "oauth2"
oauth_client = "g"

[accounts.host]
provider = "hostinger"
address = "me@example.com"
`

func setSecret(t *testing.T, h *daemontest.Harness, name, value string) {
	t.Helper()
	_, err := h.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{Name: name, Value: []byte(value)}))
	if err != nil {
		t.Fatalf("SetSecret(%s): %v", name, err)
	}
}

func TestSetSecretShowsInStatus(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	setSecret(t, h, "accounts/host", "hunter2")
	resp, err := h.Client.Admin.GetStatus(t.Context(), connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resp.Msg.GetSecretNames(), []string{"accounts/host"}) {
		t.Errorf("secret names = %v", resp.Msg.GetSecretNames())
	}
	if !resp.Msg.GetDevMode() || resp.Msg.GetVersion() != "dev" || resp.Msg.GetStartedAt() == nil {
		t.Errorf("status = %+v", resp.Msg)
	}
}

func TestSetSecretIsAuditedWithoutValue(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	setSecret(t, h, "accounts/host", "hunter2")
	rows := h.AuditRows(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v", rows)
	}
	r := rows[0]
	if r.Action != "secret.set" || r.Targets != `["accounts/host"]` || r.Result != "ok" || r.Client != "test" {
		t.Errorf("row = %+v", r)
	}
	for _, field := range []string{r.Action, r.Targets, r.Reason, r.Account, r.Client} {
		if strings.Contains(field, "hunter2") {
			t.Error("audit row contains the secret value")
		}
	}
}

func TestSetSecretRejectsInvalidName(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	_, err := h.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{Name: "../x", Value: []byte("v")}))
	if got := api.Detail(err).GetReason(); got != api.ReasonInvalidArgument {
		t.Errorf("reason = %s, want INVALID_ARGUMENT (err %v)", got, err)
	}
	if rows := h.AuditRows(t); len(rows) != 1 || rows[0].Result != "error" || rows[0].Reason != api.ReasonInvalidArgument {
		t.Errorf("audit rows = %+v", rows)
	}
}

// SetSecret takes only the names the config uses. Refresh tokens come from
// auth login alone.
func TestSetSecretOnlyConfiguredNames(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, gmailConfig, daemontest.Options{})
	for _, name := range []string{"accounts/host", "oauth_clients/g"} {
		setSecret(t, h, name, "hunter2")
	}
	tests := []struct {
		name string
		hint string
	}{
		{name: "tokens/gm", hint: "chainmail auth login"},
		{name: "accounts/gm", hint: "accounts/<account>"}, // an OAuth2 account has no password
		{name: "accounts/nope", hint: "accounts/<account>"},
		{name: "oauth_clients/other", hint: "oauth_clients/<client>"},
		{name: "other/x", hint: "accounts/<account>"},
		{name: "accounts", hint: "accounts/<account>"},
	}
	for _, tt := range tests {
		_, err := h.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{Name: tt.name, Value: []byte("hunter2")}))
		d := api.Detail(err)
		if connect.CodeOf(err) != connect.CodeInvalidArgument || d.GetReason() != api.ReasonInvalidArgument {
			t.Errorf("%s: err = %v, want INVALID_ARGUMENT", tt.name, err)
			continue
		}
		if !strings.Contains(d.GetHint(), tt.hint) {
			t.Errorf("%s: hint = %q, want it to mention %q", tt.name, d.GetHint(), tt.hint)
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(d.GetHint(), "hunter2") {
			t.Errorf("%s: error %v contains the value", tt.name, err)
		}
	}
	if names := h.Vault.Names(); !slices.Equal(names, []string{"accounts/host", "oauth_clients/g"}) {
		t.Errorf("vault names = %v, want only the allowed ones", names)
	}
	rows := h.AuditRows(t)
	if len(rows) != 2+len(tests) {
		t.Fatalf("audit rows = %+v", rows)
	}
	for i, r := range rows[2:] {
		if r.Action != "secret.set" || r.Result != "error" || r.Reason != api.ReasonInvalidArgument || r.Targets != `["`+tests[i].name+`"]` {
			t.Errorf("audit row for %s = %+v", tests[i].name, r)
		}
	}
}

// Request fields that carry a secret or an authorization code are marked
// debug_redact, for tools that honour it. protobuf-go v1.36 still prints
// them, so the daemon must not log request messages either way.
func TestSecretFieldsAreDebugRedact(t *testing.T) {
	t.Parallel()
	fields := []struct {
		msg   proto.Message
		field protoreflect.Name
	}{
		{msg: &chainmailv1.SetSecretRequest{}, field: "value"},
		{msg: &chainmailv1.FinishAuthLoginRequest{}, field: "redirect_url"},
	}
	for _, f := range fields {
		md := f.msg.ProtoReflect().Descriptor()
		fd := md.Fields().ByName(f.field)
		if fd == nil {
			t.Fatalf("%s has no field %s", md.FullName(), f.field)
		}
		opts, ok := fd.Options().(*descriptorpb.FieldOptions)
		if !ok || !opts.GetDebugRedact() {
			t.Errorf("%s.%s is not debug_redact", md.FullName(), f.field)
		}
	}
}

// Names that fail validation are client-supplied bytes of any size, so the
// append-only audit log records a placeholder instead.
func TestInvalidNamesAreAuditedAsPlaceholder(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, gmailConfig, daemontest.Options{})
	huge := strings.Repeat("a", 100<<10)
	calls := []struct {
		action string
		call   func(name string) error
	}{
		{action: "account.test", call: func(name string) error {
			_, err := h.Client.Account.TestAccount(t.Context(), connect.NewRequest(&chainmailv1.TestAccountRequest{Account: name}))
			return err
		}},
		{action: "secret.set", call: func(name string) error {
			_, err := h.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{Name: name, Value: []byte("v")}))
			return err
		}},
		{action: "auth.login.begin", call: func(name string) error {
			_, err := h.Client.Admin.BeginAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.BeginAuthLoginRequest{Account: name}))
			return err
		}},
		{action: "auth.login.finish", call: func(name string) error {
			_, err := h.Client.Admin.FinishAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.FinishAuthLoginRequest{Account: name, RedirectUrl: "http://127.0.0.1:53682/"}))
			return err
		}},
	}
	for _, c := range calls {
		for _, name := range []string{huge, "../x"} {
			err := c.call(name)
			if got := api.Detail(err).GetReason(); got != api.ReasonInvalidArgument {
				t.Errorf("%s %.20q: reason = %s, want INVALID_ARGUMENT (err %.200v)", c.action, name, got, err)
			}
			if err != nil && strings.Contains(err.Error(), huge[:1024]) {
				t.Errorf("%s: the error echoes the huge name", c.action)
			}
		}
	}
	rows := h.AuditRows(t)
	if len(rows) != 2*len(calls) {
		t.Fatalf("audit rows = %d, want %d", len(rows), 2*len(calls))
	}
	for i, r := range rows {
		want := daemontest.AuditRow{Client: "test", Account: "(invalid)", Action: calls[i/2].action, Targets: "[]", Result: "error", Reason: api.ReasonInvalidArgument}
		if want.Action == "secret.set" {
			want.Account, want.Targets = "", `["(invalid)"]`
		}
		if r != want {
			t.Errorf("audit row %d = %.300v, want %+v", i, r, want)
		}
	}
}

// Every client gets the NUL refusal, not only the CLI.
func TestSetSecretRefusesNULPassword(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, gmailConfig, daemontest.Options{})
	_, err := h.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{Name: "accounts/host", Value: []byte("hunter\x00two")}))
	if got := api.Detail(err).GetReason(); got != api.ReasonInvalidArgument {
		t.Errorf("reason = %s, want INVALID_ARGUMENT (err %v)", got, err)
	}
	if err != nil && strings.Contains(err.Error(), "hunter") {
		t.Errorf("error %v contains the value", err)
	}
	if _, err := h.Vault.Get("accounts/host"); err == nil {
		t.Error("a password with a NUL byte was stored")
	}
	if rows := h.AuditRows(t); len(rows) != 1 || rows[0].Result != "error" || rows[0].Reason != api.ReasonInvalidArgument {
		t.Errorf("audit rows = %+v", rows)
	}
}

func TestOAuthLogin(t *testing.T) {
	t.Parallel()
	o := oauthtest.Start(t)
	h := daemontest.Start(t, gmailConfig, daemontest.Options{AuthOptions: []auth.Option{auth.WithOAuthEndpoint(o.Endpoint())}})
	setSecret(t, h, "oauth_clients/g", oauthtest.ClientSecret)

	begin, err := h.Client.Admin.BeginAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.BeginAuthLoginRequest{Account: "gm"}))
	if err != nil {
		t.Fatalf("BeginAuthLogin: %v", err)
	}
	if begin.Msg.GetExpiresAt() == nil || !strings.HasPrefix(begin.Msg.GetConsentUrl(), o.URL) {
		t.Errorf("begin = %+v", begin.Msg)
	}
	redirect := oauthtest.RedirectFor(t, begin.Msg.GetConsentUrl(), oauthtest.Code)
	if _, err := h.Client.Admin.FinishAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.FinishAuthLoginRequest{Account: "gm", RedirectUrl: redirect})); err != nil {
		t.Fatalf("FinishAuthLogin: %v", err)
	}
	rt, err := h.Vault.Get(auth.RefreshTokenSecret("gm"))
	if err != nil || string(rt) != oauthtest.RefreshToken {
		t.Errorf("refresh token = %q, %v", rt, err)
	}
	var actions []string
	for _, r := range h.AuditRows(t) {
		actions = append(actions, r.Action)
	}
	if want := []string{"secret.set", "auth.login.begin", "auth.login.finish"}; !slices.Equal(actions, want) {
		t.Errorf("audit actions = %v, want %v", actions, want)
	}
}

// A refused exchange reaches the client as AUTH_REJECTED with a hint.
func TestFinishAuthLoginRefusalHasHint(t *testing.T) {
	t.Parallel()
	o := oauthtest.Start(t)
	h := daemontest.Start(t, gmailConfig, daemontest.Options{AuthOptions: []auth.Option{auth.WithOAuthEndpoint(o.Endpoint())}})
	setSecret(t, h, "oauth_clients/g", oauthtest.ClientSecret)
	begin, err := h.Client.Admin.BeginAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.BeginAuthLoginRequest{Account: "gm"}))
	if err != nil {
		t.Fatal(err)
	}
	redirect := oauthtest.RedirectFor(t, begin.Msg.GetConsentUrl(), "used-code")
	_, err = h.Client.Admin.FinishAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.FinishAuthLoginRequest{Account: "gm", RedirectUrl: redirect}))
	d := api.Detail(err)
	if d.GetReason() != api.ReasonAuthRejected || !strings.Contains(d.GetHint(), "chainmail auth login gm again") {
		t.Errorf("err = %v, detail = %+v, want AUTH_REJECTED with the login hint", err, d)
	}
}

func TestBeginAuthLoginErrors(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, gmailConfig, daemontest.Options{})
	tests := []struct {
		account string
		reason  string
	}{
		{account: "host", reason: api.ReasonInvalidArgument},
		{account: "gm", reason: api.ReasonAuthRequired},
		{account: "nope", reason: api.ReasonNotFound},
	}
	for _, tt := range tests {
		_, err := h.Client.Admin.BeginAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.BeginAuthLoginRequest{Account: tt.account}))
		if got := api.Detail(err).GetReason(); got != tt.reason {
			t.Errorf("%s: reason = %s, want %s (err %v)", tt.account, got, tt.reason, err)
		}
	}
}

func TestForbiddenPeer(t *testing.T) {
	t.Parallel()
	other := daemon.NewAuthorizer(uint32(os.Getuid())+1, -1)
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{Authorizer: other})
	_, err := h.Client.Admin.GetStatus(t.Context(), connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("code = %v, want PermissionDenied (err %v)", connect.CodeOf(err), err)
	}
	if got := api.Detail(err).GetReason(); got != api.ReasonForbidden {
		t.Errorf("reason = %s", got)
	}
}

func TestSetSecretRejectsOversizedRequest(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	big := strings.Repeat("x", 1<<20+1)
	_, err := h.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{Name: "accounts/host", Value: []byte(big)}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted (err %v)", connect.CodeOf(err), err)
	}
	if names := h.Vault.Names(); len(names) != 0 {
		t.Errorf("vault names = %v, want none", names)
	}
}

// seedAudit writes rows straight into the harness's audit table.
func seedAudit(t *testing.T, h *daemontest.Harness, entries ...audit.Entry) {
	t.Helper()
	w := audit.NewWriter(h.DB, nil)
	for _, e := range entries {
		if e.Result == "" {
			e.Result = audit.ResultOK
		}
		if err := w.Record(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
}

func readAudit(t *testing.T, h *daemontest.Harness, req *chainmailv1.ReadAuditRequest) (*chainmailv1.ReadAuditResponse, error) {
	t.Helper()
	resp, err := h.Client.Admin.ReadAudit(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestReadAuditLimitBounds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{Now: func() time.Time { return now }})
	entries := make([]audit.Entry, 101)
	for i := range entries {
		entries[i] = audit.Entry{At: now.Add(-time.Hour), Action: "x"}
	}
	seedAudit(t, h, entries...)
	for _, limit := range []int32{-1, 1001} {
		_, err := readAudit(t, h, &chainmailv1.ReadAuditRequest{Limit: limit})
		if connect.CodeOf(err) != connect.CodeInvalidArgument || api.Detail(err).GetReason() != api.ReasonInvalidArgument {
			t.Errorf("limit %d: err = %v, want INVALID_ARGUMENT", limit, err)
			continue
		}
		var cerr *connect.Error
		if errors.As(err, &cerr) && cerr.Message() != "limit must be 0 to 1000" {
			t.Errorf("limit %d: message = %q", limit, cerr.Message())
		}
	}
	resp, err := readAudit(t, h, &chainmailv1.ReadAuditRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.GetEntries()); n != 100 || resp.GetNextBeforeId() == 0 {
		t.Errorf("limit 0: %d entries, next %d, want 100 and a next page", n, resp.GetNextBeforeId())
	}
	resp, err = readAudit(t, h, &chainmailv1.ReadAuditRequest{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.GetEntries()); n != 101 || resp.GetNextBeforeId() != 0 {
		t.Errorf("limit 1000: %d entries, next %d, want 101 and no next page", n, resp.GetNextBeforeId())
	}
}

func TestReadAuditUnknownAccount(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	for _, account := range []string{"nope", "../x", strings.Repeat("a", 10<<10)} {
		_, err := readAudit(t, h, &chainmailv1.ReadAuditRequest{Account: account})
		if connect.CodeOf(err) != connect.CodeInvalidArgument || api.Detail(err).GetReason() != api.ReasonInvalidArgument {
			t.Errorf("%.20q: err = %.200v, want INVALID_ARGUMENT", account, err)
			continue
		}
		var cerr *connect.Error
		if errors.As(err, &cerr) && cerr.Message() != "unknown account" {
			t.Errorf("%.20q: message = %.200q, want fixed text", account, cerr.Message())
		}
	}
	if _, err := readAudit(t, h, &chainmailv1.ReadAuditRequest{Account: "host"}); err != nil {
		t.Errorf("a configured account was refused: %v", err)
	}
}

func TestReadAuditDefaultSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{Now: func() time.Time { return now }})
	seedAudit(t, h,
		audit.Entry{At: now.Add(-25 * time.Hour), Action: "old"},
		audit.Entry{At: now.Add(-23 * time.Hour), Action: "recent"},
	)
	resp, err := readAudit(t, h, &chainmailv1.ReadAuditRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range resp.GetEntries() {
		actions = append(actions, e.GetAction())
	}
	if !slices.Equal(actions, []string{"recent"}) {
		t.Errorf("actions = %v, want only the row from 23 hours ago", actions)
	}
	resp, err = readAudit(t, h, &chainmailv1.ReadAuditRequest{Since: timestamppb.New(now.Add(-26 * time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.GetEntries()); n != 2 {
		t.Errorf("an explicit since gave %d entries, want 2", n)
	}
}

func TestReadAuditMapsEntries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{Now: func() time.Time { return now }})
	at := now.Add(-time.Minute)
	seedAudit(t, h,
		audit.Entry{
			At: at, CallerUID: 501, Client: "mcp", Account: "host", Action: "mail.send",
			Targets: []string{"blocked:2", "too_many"}, Decision: "held", Result: audit.ResultOK,
		},
		audit.Entry{At: at, Client: "cli", Account: "host", Action: "mail.read", Result: audit.ResultError, Reason: api.ReasonNotFound},
	)
	resp, err := readAudit(t, h, &chainmailv1.ReadAuditRequest{Account: "host", Action: "mail.send", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetEntries()) != 1 {
		t.Fatalf("entries = %v", resp.GetEntries())
	}
	want := &chainmailv1.AuditEntry{
		Id: 1, At: timestamppb.New(at), CallerUid: 501, Client: "mcp", Account: "host", Action: "mail.send",
		Targets: []string{"blocked:2", "too_many"}, Decision: "held", Result: "ok",
	}
	if got := resp.GetEntries()[0]; !proto.Equal(got, want) {
		t.Errorf("entry = %v, want %v", got, want)
	}
	// The read itself is not audited, as GetStatus is not.
	if rows := h.AuditRows(t); len(rows) != 2 {
		t.Errorf("audit rows = %d, want only the 2 seeded", len(rows))
	}
}

func TestReadAuditPagesByBeforeID(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	seedAudit(t, h, audit.Entry{Action: "a"}, audit.Entry{Action: "b"}, audit.Entry{Action: "c"})
	var actions []string
	req := &chainmailv1.ReadAuditRequest{Limit: 2}
	for range 3 {
		resp, err := readAudit(t, h, req)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range resp.GetEntries() {
			actions = append(actions, e.GetAction())
		}
		if resp.GetNextBeforeId() == 0 {
			break
		}
		req.BeforeId = resp.GetNextBeforeId()
	}
	if !slices.Equal(actions, []string{"c", "b", "a"}) {
		t.Errorf("paged actions = %v, want c b a", actions)
	}
}

func TestReadAuditRejectsInvalidSince(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	_, err := readAudit(t, h, &chainmailv1.ReadAuditRequest{Since: &timestamppb.Timestamp{Seconds: 1 << 62}})
	if connect.CodeOf(err) != connect.CodeInvalidArgument || api.Detail(err).GetReason() != api.ReasonInvalidArgument {
		t.Errorf("err = %v, want INVALID_ARGUMENT", err)
	}
}

// TestReadAuditFiltersByClient checks the client filter alone, with an
// action filter and across pages, and that a value that cannot be a client
// kind is refused with fixed text.
func TestReadAuditFiltersByClient(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, hostingerConfig, daemontest.Options{})
	seedAudit(t, h,
		audit.Entry{Client: "cli", Action: "a"},
		audit.Entry{Client: "rule", Action: "b"},
		audit.Entry{Client: "rule", Action: "c"},
		audit.Entry{Client: "mcp", Action: "d"},
	)
	actionsOf := func(req *chainmailv1.ReadAuditRequest) []string {
		t.Helper()
		var out []string
		for {
			resp, err := readAudit(t, h, req)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range resp.GetEntries() {
				out = append(out, e.GetClient()+":"+e.GetAction())
			}
			if resp.GetNextBeforeId() == 0 {
				return out
			}
			req.BeforeId = resp.GetNextBeforeId()
		}
	}
	tests := []struct {
		name string
		req  *chainmailv1.ReadAuditRequest
		want []string
	}{
		{name: "client", req: &chainmailv1.ReadAuditRequest{Client: "rule"}, want: []string{"rule:c", "rule:b"}},
		{name: "client and action", req: &chainmailv1.ReadAuditRequest{Client: "rule", Action: "b"}, want: []string{"rule:b"}},
		{name: "paged", req: &chainmailv1.ReadAuditRequest{Client: "rule", Limit: 1}, want: []string{"rule:c", "rule:b"}},
		{name: "every client", req: &chainmailv1.ReadAuditRequest{}, want: []string{"mcp:d", "rule:c", "rule:b", "cli:a"}},
	}
	for _, tt := range tests {
		if got := actionsOf(tt.req); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
	for _, client := range []string{"Rule", "bad kind", strings.Repeat("a", 33), "(invalid)"} {
		_, err := readAudit(t, h, &chainmailv1.ReadAuditRequest{Client: client})
		var cerr *connect.Error
		if reasonOf(err) != api.ReasonInvalidArgument || !errors.As(err, &cerr) || cerr.Message() != "invalid client kind" {
			t.Errorf("client %.20q: %v, want INVALID_ARGUMENT with fixed text", client, err)
		}
	}
}

// issuerConfig is a custom account logging in through the issuer client kc.
func issuerConfig(issuer string) string {
	return `
[oauth_clients.kc]
client_id = "chainmail"
issuer = "` + issuer + `"
scopes = ["openid", "offline_access"]

[accounts.mw]
provider = "custom"
address = "james@example.org"
auth = "oauth2"
oauth_client = "kc"
imap = { host = "127.0.0.1", port = 993 }
smtp = { host = "127.0.0.1", port = 465 }
`
}

// A public issuer client logs in with no client secret in the vault, and its
// accounts report a login, not a secret, as what they need. Its secret
// may still be set, though chainmail never sends it.
func TestOAuthLoginWithPublicIssuerClient(t *testing.T) {
	t.Parallel()
	o := oauthtest.StartWith(t, oauthtest.Options{Public: true})
	h := daemontest.Start(t, issuerConfig(o.Issuer()), daemontest.Options{})
	list, err := h.Client.Account.ListAccounts(t.Context(), connect.NewRequest(&chainmailv1.ListAccountsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if accts := list.Msg.GetAccounts(); len(accts) != 1 || accts[0].GetAuthState() != chainmailv1.AuthState_AUTH_STATE_REQUIRED ||
		accts[0].GetAuthDetail() != "run: chainmail auth login mw" {
		t.Errorf("accounts = %+v, want mw needing a login", accts)
	}
	begin, err := h.Client.Admin.BeginAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.BeginAuthLoginRequest{Account: "mw"}))
	if err != nil {
		t.Fatalf("BeginAuthLogin: %v", err)
	}
	if !strings.HasPrefix(begin.Msg.GetConsentUrl(), o.Issuer()+"/auth?") {
		t.Errorf("consent URL = %q, want the discovered endpoint", begin.Msg.GetConsentUrl())
	}
	redirect := oauthtest.RedirectFor(t, begin.Msg.GetConsentUrl(), oauthtest.Code)
	if _, err := h.Client.Admin.FinishAuthLogin(t.Context(), connect.NewRequest(&chainmailv1.FinishAuthLoginRequest{Account: "mw", RedirectUrl: redirect})); err != nil {
		t.Fatalf("FinishAuthLogin: %v", err)
	}
	if rt, err := h.Vault.Get(auth.RefreshTokenSecret("mw")); err != nil || string(rt) != oauthtest.RefreshToken {
		t.Errorf("refresh token = %q, %v", rt, err)
	}
	setSecret(t, h, "oauth_clients/kc", "a-confidential-secret")
}
