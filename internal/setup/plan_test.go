package setup

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

const workspaceClientID = "cid.apps.googleusercontent.com"

// gmailRequest asks for alice@gmail.com with a password, from a terminal,
// running the binary the host fakes install.
func gmailRequest() Request {
	return Request{
		Address:    "alice@gmail.com",
		Detection:  Detection{Provider: "gmail", Auth: provider.AuthPassword},
		ExecSHA256: fakeBinarySHA,
		Backup:     BackupNone,
		Terminal:   true,
	}
}

// workspaceRequest asks for alice@example.com on Google Workspace.
func workspaceRequest(terminal bool) Request {
	return Request{
		Address:       "alice@example.com",
		Detection:     Detection{Provider: "google-workspace", Auth: provider.AuthOAuth2, ViaMX: true},
		OAuthClientID: workspaceClientID,
		ExecSHA256:    fakeBinarySHA,
		Backup:        BackupNone,
		Terminal:      terminal,
	}
}

// okHealth is what the daemon says about an account that works.
var okHealth = &AccountHealth{Exists: true, SecretStored: true, AuthOK: true}

// describe renders actions as "kind target" strings for comparison.
func describe(actions []Action) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		s := string(a.Kind)
		switch {
		case a.Entry != "":
			s += " " + a.Entry
		case a.Name != "":
			s += " " + a.Name
		}
		out = append(out, s)
	}
	return out
}

// dirEnsures lists "ensure-dir <entry>" for every directory entry of l, in
// layout order.
func dirEnsures(l Layout) []string {
	var out []string
	for _, e := range l.Entries() {
		if e.Kind == KindDir {
			out = append(out, "ensure-dir "+e.Name)
		}
	}
	return out
}

func wantActions(t *testing.T, got []Action, want []string) {
	t.Helper()
	if d := describe(got); !slices.Equal(d, want) {
		t.Errorf("actions:\n got %q\nwant %q", d, want)
	}
}

func mustBuild(t *testing.T, l Layout, cur Current, req Request) Plan {
	t.Helper()
	p, err := Build(l, cur, req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return p
}

func findAction(p Plan, kind ActionKind) (Action, bool) {
	for _, a := range p.Actions {
		if a.Kind == kind {
			return a, true
		}
	}
	return Action{}, false
}

func TestBuildFresh(t *testing.T) {
	t.Parallel()
	for _, p := range []Platform{Linux, Darwin} {
		t.Run(string(p), func(t *testing.T) {
			t.Parallel()
			l := LayoutFor(p)
			cur := freshHost(p).inspect(t, p)
			plan := mustBuild(t, l, cur, gmailRequest())

			want := []string{
				"create-group " + l.ClientGroup,
				"create-user " + l.DaemonUser,
				"add-member alice",
			}
			want = append(want, dirEnsures(l)...)
			want = append(want,
				"install-binary binary",
				"create-key vault-key",
				"write-config config",
				"write-service service",
				"activate-service",
				"store-secret alice",
				"test-account alice",
			)
			wantActions(t, plan.Actions, want)

			if plan.Version != 1 || plan.Platform != p || plan.InvokingUser != "alice" {
				t.Errorf("Version %d Platform %q InvokingUser %q", plan.Version, plan.Platform, plan.InvokingUser)
			}
			if plan.Account != aliceGmail {
				t.Errorf("Account = %+v, want %+v", plan.Account, aliceGmail)
			}
			if plan.OAuthClient != nil {
				t.Errorf("OAuthClient = %+v, want nil", plan.OAuthClient)
			}
			if !plan.NeedSecret || plan.SecretName != "accounts/alice" || !plan.ServiceChanged {
				t.Errorf("NeedSecret %v SecretName %q ServiceChanged %v", plan.NeedSecret, plan.SecretName, plan.ServiceChanged)
			}
			if plan.Backup != BackupNone {
				t.Errorf("Backup = %q, want none", plan.Backup)
			}
			cfg, _ := findAction(plan, ActWriteConfig)
			if !bytes.Equal(cfg.Data, NewConfig(l, aliceGmail, nil)) {
				t.Errorf("write-config data:\n%s\nwant NewConfig", cfg.Data)
			}
			svc, _ := findAction(plan, ActWriteService)
			if !bytes.Equal(svc.Data, RenderService(l)) {
				t.Error("write-service data is not RenderService")
			}
			for _, a := range plan.Actions {
				if a.Note == "" {
					t.Errorf("%s has no note", a.Kind)
				}
				if a.Data != nil && a.Kind != ActWriteConfig && a.Kind != ActWriteService {
					t.Errorf("%s carries data", a.Kind)
				}
			}
		})
	}

	t.Run("backup show is carried when the key is new", func(t *testing.T) {
		t.Parallel()
		req := gmailRequest()
		req.Backup = BackupShow
		plan := mustBuild(t, LayoutFor(Linux), freshHost(Linux).inspect(t, Linux), req)
		if plan.Backup != BackupShow {
			t.Errorf("Backup = %q, want show", plan.Backup)
		}
	})

	t.Run("name override", func(t *testing.T) {
		t.Parallel()
		req := gmailRequest()
		req.Name = "personal"
		plan := mustBuild(t, LayoutFor(Linux), freshHost(Linux).inspect(t, Linux), req)
		if plan.Account.Name != "personal" || plan.SecretName != "accounts/personal" {
			t.Errorf("Account.Name %q SecretName %q, want personal", plan.Account.Name, plan.SecretName)
		}
	})
}

func TestBuildRerunNoop(t *testing.T) {
	t.Parallel()
	for _, p := range []Platform{Linux, Darwin} {
		t.Run(string(p), func(t *testing.T) {
			t.Parallel()
			l := LayoutFor(p)
			cur := installedHost(p, aliceGmail, nil).inspect(t, p)
			cur.Health = okHealth
			req := gmailRequest()
			req.Backup = BackupShow // ignored: the key is reused
			plan := mustBuild(t, l, cur, req)

			wantActions(t, plan.Actions, append(dirEnsures(l), "test-account alice"))
			if plan.NeedSecret || plan.ServiceChanged {
				t.Errorf("NeedSecret %v ServiceChanged %v, want both false", plan.NeedSecret, plan.ServiceChanged)
			}
			if plan.Backup != BackupNone {
				t.Errorf("Backup = %q, want none when the key is reused", plan.Backup)
			}
		})
	}

	t.Run("new binary", func(t *testing.T) {
		t.Parallel()
		l := LayoutFor(Linux)
		cur := installedHost(Linux, aliceGmail, nil).inspect(t, Linux)
		cur.Health = okHealth
		req := gmailRequest()
		req.ExecSHA256 = sha256Hex([]byte("chainmail binary v2"))
		plan := mustBuild(t, l, cur, req)
		want := append(dirEnsures(l), "install-binary binary", "activate-service", "test-account alice")
		wantActions(t, plan.Actions, want)
		if !plan.ServiceChanged {
			t.Error("ServiceChanged = false after a binary change")
		}
	})

	t.Run("changed service definition", func(t *testing.T) {
		t.Parallel()
		l := LayoutFor(Linux)
		h := installedHost(Linux, aliceGmail, nil)
		h.files.File(l.Service, 0, 0, 0o644, append(RenderService(l), "# local tweak\n"...))
		cur := h.inspect(t, Linux)
		cur.Health = okHealth
		plan := mustBuild(t, l, cur, gmailRequest())
		want := append(dirEnsures(l), "write-service service", "activate-service", "test-account alice")
		wantActions(t, plan.Actions, want)
		if !plan.ServiceChanged {
			t.Error("ServiceChanged = false after a service change")
		}
	})

	t.Run("invoking user not yet in the group", func(t *testing.T) {
		t.Parallel()
		l := LayoutFor(Linux)
		h := installedHost(Linux, aliceGmail, nil)
		h.accounts.AddGroup(system.GroupInfo{Name: l.ClientGroup, GID: fakeClientsGID})
		alice, _ := h.accounts.User("alice")
		alice.Groups = nil
		h.accounts.AddUser(alice)
		cur := h.inspect(t, Linux)
		cur.Health = okHealth
		plan := mustBuild(t, l, cur, gmailRequest())
		want := append([]string{"add-member alice"}, dirEnsures(l)...)
		wantActions(t, plan.Actions, append(want, "test-account alice"))
	})

	t.Run("macos daemon user has the group as its primary group", func(t *testing.T) {
		t.Parallel()
		// _chainmail's Groups does not list chainmail-clients: it is the
		// primary group. That is the right shape, not a missing membership.
		l := LayoutFor(Darwin)
		cur := installedHost(Darwin, aliceGmail, nil).inspect(t, Darwin)
		if slices.Contains(cur.DaemonUser.Groups, l.ClientGroup) {
			t.Fatal("fixture lists the primary group as supplementary")
		}
		cur.Health = okHealth
		plan := mustBuild(t, l, cur, gmailRequest())
		wantActions(t, plan.Actions, append(dirEnsures(l), "test-account alice"))
	})

	t.Run("linux daemon user missing from the client group", func(t *testing.T) {
		t.Parallel()
		l := LayoutFor(Linux)
		h := installedHost(Linux, aliceGmail, nil)
		u, _ := h.accounts.User(l.DaemonUser)
		u.Groups = nil
		h.accounts.AddUser(u)
		cur := h.inspect(t, Linux)
		cur.Health = okHealth
		plan := mustBuild(t, l, cur, gmailRequest())
		want := append([]string{"add-member " + l.DaemonUser}, dirEnsures(l)...)
		wantActions(t, plan.Actions, append(want, "test-account alice"))
	})
}

func TestBuildSecondAccount(t *testing.T) {
	t.Parallel()
	for _, p := range []Platform{Linux, Darwin} {
		t.Run(string(p), func(t *testing.T) {
			t.Parallel()
			l := LayoutFor(p)
			cur := installedHost(p, aliceGmail, nil).inspect(t, p)
			req := gmailRequest()
			req.Address = "bob@gmail.com"
			plan := mustBuild(t, l, cur, req)

			want := append(dirEnsures(l),
				"write-config config",
				"activate-service",
				"store-secret bob",
				"test-account bob",
			)
			wantActions(t, plan.Actions, want)
			bob := AccountSpec{Name: "bob", Provider: "gmail", Address: "bob@gmail.com", Auth: provider.AuthPassword}
			wantData, err := AppendAccount(cur.Config, bob, nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg, _ := findAction(plan, ActWriteConfig)
			if !bytes.Equal(cfg.Data, wantData) {
				t.Errorf("write-config data:\n%s\nwant\n%s", cfg.Data, wantData)
			}
			if !plan.ServiceChanged || !plan.NeedSecret || plan.SecretName != "accounts/bob" {
				t.Errorf("ServiceChanged %v NeedSecret %v SecretName %q", plan.ServiceChanged, plan.NeedSecret, plan.SecretName)
			}
		})
	}

	t.Run("appended to a hand-written config", func(t *testing.T) {
		t.Parallel()
		l := LayoutFor(Linux)
		h := installedHost(Linux, aliceGmail, nil)
		h.files.File(l.Config, 0, 0, 0o644, []byte(handWritten))
		cur := h.inspect(t, Linux)
		plan := mustBuild(t, l, cur, gmailRequest())
		cfg, ok := findAction(plan, ActWriteConfig)
		if !ok || !bytes.HasPrefix(cfg.Data, []byte(handWritten)) {
			t.Errorf("write-config data does not keep the hand-written config:\n%s", cfg.Data)
		}
	})
}

func TestBuildRefusals(t *testing.T) {
	t.Parallel()
	linux := LayoutFor(Linux)
	workspaceAlice := AccountSpec{
		Name: "alice", Provider: "google-workspace", Address: "alice@example.com",
		Auth: provider.AuthOAuth2, OAuthClient: "google",
	}
	gmailOAuthAlice := AccountSpec{
		Name: "alice", Provider: "gmail", Address: "alice@gmail.com",
		Auth: provider.AuthOAuth2, OAuthClient: "google",
	}
	google := &OAuthClientSpec{Name: "google", ClientID: "id-1.apps.googleusercontent.com"}

	cases := []struct {
		name string
		host func() host
		req  func() Request
		// health is set on the inspected Current.
		health *AccountHealth
		// want is a substring of the refusal message.
		want string
	}{
		{
			name: "nixos",
			host: func() host {
				h := freshHost(Linux)
				h.files.File("/etc/NIXOS", 0, 0, 0o644, nil)
				return h
			},
			want: "NixOS",
		},
		{
			name: "same name with another address",
			host: func() host { return installedHost(Linux, aliceGmail, nil) },
			req: func() Request {
				r := gmailRequest()
				r.Address = "alice@googlemail.com"
				return r
			},
			want: "another address",
		},
		{
			name: "same account with another provider",
			host: func() host { return installedHost(Linux, aliceGmail, nil) },
			req: func() Request {
				r := gmailRequest()
				r.Detection = Detection{Provider: "hostinger", Auth: provider.AuthPassword}
				return r
			},
			want: "provider",
		},
		{
			name: "same account with another auth",
			host: func() host { return installedHost(Linux, gmailOAuthAlice, google) },
			want: "auth",
		},
		{
			name: "oauth client with another client id",
			host: func() host { return installedHost(Linux, workspaceAlice, google) },
			req: func() Request {
				r := workspaceRequest(true)
				r.Address = "bob@example.com"
				r.OAuthClientID = "id-2.apps.googleusercontent.com"
				return r
			},
			want: "client ID",
		},
		{
			name: "oauth client with another client id on the same account",
			host: func() host { return installedHost(Linux, workspaceAlice, google) },
			req: func() Request {
				r := workspaceRequest(true)
				r.OAuthClientID = "id-2.apps.googleusercontent.com"
				return r
			},
			want: "client ID",
		},
		{
			name: "workspace without a client id",
			host: func() host { return freshHost(Linux) },
			req: func() Request {
				r := workspaceRequest(true)
				r.OAuthClientID = ""
				return r
			},
			want: "--oauth-client-id",
		},
		{
			name: "vault without a key",
			host: func() host {
				h := installedHost(Linux, aliceGmail, nil)
				h.files.Remove(linux.VaultKey)
				return h
			},
			want: "vault",
		},
		{
			name: "service file without the marker",
			host: func() host {
				h := installedHost(Linux, aliceGmail, nil)
				h.files.File(linux.Service, 0, 0, 0o644, []byte(deployPageUnit))
				return h
			},
			want: "marker",
		},
		{
			name: "daemon user with a login shell",
			host: func() host {
				h := installedHost(Linux, aliceGmail, nil)
				u, _ := h.accounts.User(linux.DaemonUser)
				u.Shell = "/bin/bash"
				h.accounts.AddUser(u)
				return h
			},
			want: "shell",
		},
		{
			name: "daemon user that is root",
			host: func() host {
				// On a fresh tree, so the trust check passes and the shape is
				// what is refused.
				h := freshHost(Linux)
				h.accounts.AddUser(system.User{Name: linux.DaemonUser, Shell: "/usr/sbin/nologin"})
				return h
			},
			want: "uid 0",
		},
		{
			name: "client group with gid 0",
			host: func() host {
				h := freshHost(Linux)
				h.accounts.AddGroup(system.GroupInfo{Name: linux.ClientGroup, GID: 0})
				return h
			},
			want: "gid 0",
		},
		{
			name: "trust chain: user-owned ancestor",
			host: func() host {
				h := freshHost(Linux)
				h.files.Dir("/usr/local", fakeUserUID, fakeUserUID, 0o755)
				return h
			},
			want: "unsafe path /usr/local",
		},
		{
			name: "trust chain: group-writable config directory",
			host: func() host {
				h := installedHost(Linux, aliceGmail, nil)
				h.files.Dir(linux.ConfigDir, 0, 0, 0o775)
				return h
			},
			want: "unsafe path /etc/chainmail",
		},
		{
			name: "trust chain: state directory owned by the user",
			host: func() host {
				h := installedHost(Linux, aliceGmail, nil)
				h.files.Dir(linux.StateDir, fakeUserUID, fakeUserUID, 0o700)
				return h
			},
			want: "unsafe path /var/lib/chainmail",
		},
		{
			name: "trust chain: socket directory writable by others",
			host: func() host {
				h := installedHost(Linux, aliceGmail, nil)
				h.files.Dir(linux.SocketDir, fakeLinuxDaemon, fakeLinuxGroup, 0o777)
				return h
			},
			want: "unsafe path /run/chainmail",
		},
		{
			name: "unparseable config",
			host: func() host {
				h := installedHost(Linux, aliceGmail, nil)
				h.files.File(linux.Config, 0, 0, 0o644, []byte("[accounts.alice\nprovider = "))
				return h
			},
			want: "does not parse",
		},
		{
			name: "config that is a symlink",
			host: func() host {
				h := installedHost(Linux, aliceGmail, nil)
				h.files.Symlink(linux.Config, 0, "/nix/store/abc-config.toml")
				return h
			},
			want: "symlink",
		},
		{
			name: "unsupported provider",
			host: func() host { return freshHost(Linux) },
			req: func() Request {
				r := gmailRequest()
				r.Detection = Detection{Provider: "custom", Auth: provider.AuthPassword}
				return r
			},
			want: "custom",
		},
		{
			name: "auth the provider does not take",
			host: func() host { return freshHost(Linux) },
			req: func() Request {
				r := gmailRequest()
				r.Detection = Detection{Provider: "hostinger", Auth: provider.AuthOAuth2}
				return r
			},
			want: "oauth2",
		},
		{
			name: "invalid account name",
			host: func() host { return freshHost(Linux) },
			req: func() Request {
				r := gmailRequest()
				r.Name = "Not_Valid"
				return r
			},
			want: "Not_Valid",
		},
		{
			name: "invalid address",
			host: func() host { return freshHost(Linux) },
			req: func() Request {
				r := gmailRequest()
				r.Address = "Alice <alice@gmail.com>"
				return r
			},
			want: "bare email address",
		},
		{
			name: "invalid oauth client name",
			host: func() host { return freshHost(Linux) },
			req: func() Request {
				r := workspaceRequest(true)
				r.OAuthClient = "Bad Client"
				return r
			},
			want: "Bad Client",
		},
		{
			name: "endpoint overrides without the e2e build",
			host: func() host { return freshHost(Linux) },
			req: func() Request {
				r := gmailRequest()
				r.IMAP = e2eOverrideForTest()
				return r
			},
			want: "override",
		},
		{
			name: "no binary hash",
			host: func() host { return freshHost(Linux) },
			req: func() Request {
				r := gmailRequest()
				r.ExecSHA256 = ""
				return r
			},
			want: "binary",
		},
		{
			name: "unknown backup",
			host: func() host { return freshHost(Linux) },
			req: func() Request {
				r := gmailRequest()
				r.Backup = "paper"
				return r
			},
			want: "paper",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.name == "endpoint overrides without the e2e build" && E2EEnabled() {
				t.Skip("overrides are allowed under the e2e build tag")
			}
			cur := tc.host().inspect(t, Linux)
			cur.Health = tc.health
			req := gmailRequest()
			if tc.req != nil {
				req = tc.req()
			}
			_, err := Build(linux, cur, req)
			r := asRefusal(t, err)
			if !strings.Contains(r.Msg, tc.want) {
				t.Errorf("refusal %q does not mention %q", r.Msg, tc.want)
			}
			if r.Hint == "" {
				t.Errorf("refusal %q has no hint", r.Msg)
			}
		})
	}

	t.Run("darwin trust chain through a user-owned symlink target", func(t *testing.T) {
		t.Parallel()
		h := freshHost(Darwin)
		h.files.Dir("/private/etc", fakeUserUID, 0, 0o755)
		cur := h.inspect(t, Darwin)
		r := asRefusal(t, mustFail(Build(LayoutFor(Darwin), cur, gmailRequest())))
		if !strings.Contains(r.Msg, "/private/etc") {
			t.Errorf("refusal %q does not name /private/etc", r.Msg)
		}
	})

	t.Run("platform mismatch", func(t *testing.T) {
		t.Parallel()
		cur := freshHost(Linux).inspect(t, Linux)
		if _, err := Build(LayoutFor(Darwin), cur, gmailRequest()); err == nil {
			t.Error("Build with a Linux Current and a macOS layout succeeded")
		}
	})

	t.Run("current not from Inspect", func(t *testing.T) {
		t.Parallel()
		cur := Current{Platform: Linux, InvokingUser: system.User{Name: "alice", UID: fakeUserUID}}
		if _, err := Build(linux, cur, gmailRequest()); err == nil {
			t.Error("Build without an inspected trust chain succeeded")
		}
	})

	t.Run("invoking user is root", func(t *testing.T) {
		t.Parallel()
		cur := freshHost(Linux).inspect(t, Linux)
		cur.InvokingUser = system.User{Name: "root"}
		if r := asRefusal(t, mustFail(Build(linux, cur, gmailRequest()))); r.Hint == "" {
			t.Errorf("refusal %q has no hint", r.Msg)
		}
	})

	t.Run("invoking user is the daemon user", func(t *testing.T) {
		t.Parallel()
		h := installedHost(Linux, aliceGmail, nil)
		cur, err := Inspect(t.Context(), linux, h.files, h.accounts, linux.DaemonUser)
		if err != nil {
			t.Fatal(err)
		}
		if r := asRefusal(t, mustFail(Build(linux, cur, gmailRequest()))); r.Hint == "" {
			t.Errorf("refusal %q has no hint", r.Msg)
		}
	})
}

func mustFail(_ Plan, err error) error { return err }

// e2eOverrideForTest is an IMAP override like the e2e hook makes.
func e2eOverrideForTest() *config.EndpointOverride {
	return &config.EndpointOverride{Host: "127.0.0.1", Port: 3993, Security: "tls"}
}

func TestBuildOAuth(t *testing.T) {
	t.Parallel()
	l := LayoutFor(Linux)

	t.Run("with a terminal", func(t *testing.T) {
		t.Parallel()
		plan := mustBuild(t, l, freshHost(Linux).inspect(t, Linux), workspaceRequest(true))
		d := describe(plan.Actions)
		if tail := d[len(d)-4:]; !slices.Equal(tail, []string{"activate-service", "store-secret alice", "auth-login alice", "test-account alice"}) {
			t.Errorf("actions end %q", tail)
		}
		if plan.SecretName != "oauth_clients/google" || !plan.NeedSecret {
			t.Errorf("SecretName %q NeedSecret %v", plan.SecretName, plan.NeedSecret)
		}
		wantClient := &OAuthClientSpec{Name: "google", ClientID: workspaceClientID}
		if plan.OAuthClient == nil || *plan.OAuthClient != *wantClient {
			t.Errorf("OAuthClient = %+v, want %+v", plan.OAuthClient, wantClient)
		}
		if plan.Account.Auth != provider.AuthOAuth2 || plan.Account.OAuthClient != "google" {
			t.Errorf("Account = %+v", plan.Account)
		}
		cfg, _ := findAction(plan, ActWriteConfig)
		if !bytes.Equal(cfg.Data, NewConfig(l, plan.Account, wantClient)) {
			t.Errorf("write-config data:\n%s", cfg.Data)
		}
		if len(plan.NextSteps) != 0 {
			t.Errorf("NextSteps = %q, want none", plan.NextSteps)
		}
	})

	t.Run("without a terminal", func(t *testing.T) {
		t.Parallel()
		plan := mustBuild(t, l, freshHost(Linux).inspect(t, Linux), workspaceRequest(false))
		d := describe(plan.Actions)
		if tail := d[len(d)-2:]; !slices.Equal(tail, []string{"activate-service", "store-secret alice"}) {
			t.Errorf("actions end %q", tail)
		}
		if _, ok := findAction(plan, ActAuthLogin); ok {
			t.Error("auth-login planned without a terminal")
		}
		if _, ok := findAction(plan, ActTestAccount); ok {
			t.Error("test-account planned without a login")
		}
		if !slices.Contains(plan.NextSteps, "chainmail auth login alice") {
			t.Errorf("NextSteps = %q, want chainmail auth login alice", plan.NextSteps)
		}
	})

	t.Run("named client", func(t *testing.T) {
		t.Parallel()
		req := workspaceRequest(true)
		req.OAuthClient = "work"
		plan := mustBuild(t, l, freshHost(Linux).inspect(t, Linux), req)
		if plan.SecretName != "oauth_clients/work" || plan.Account.OAuthClient != "work" || plan.OAuthClient.Name != "work" {
			t.Errorf("SecretName %q Account %+v OAuthClient %+v", plan.SecretName, plan.Account, plan.OAuthClient)
		}
	})

	t.Run("rerun keeps the existing client", func(t *testing.T) {
		t.Parallel()
		spec := AccountSpec{
			Name: "alice", Provider: "google-workspace", Address: "alice@example.com",
			Auth: provider.AuthOAuth2, OAuthClient: "work",
		}
		client := &OAuthClientSpec{Name: "work", ClientID: workspaceClientID}
		cur := installedHost(Linux, spec, client).inspect(t, Linux)
		cur.Health = okHealth
		req := workspaceRequest(true)
		req.OAuthClientID = "" // taken from the config
		plan := mustBuild(t, l, cur, req)
		if plan.Account != spec || plan.OAuthClient == nil || *plan.OAuthClient != *client {
			t.Errorf("Account %+v OAuthClient %+v, want the existing ones", plan.Account, plan.OAuthClient)
		}
		wantActions(t, plan.Actions, append(dirEnsures(l), "test-account alice"))
	})
}

func TestBuildHealth(t *testing.T) {
	t.Parallel()
	l := LayoutFor(Linux)
	cases := []struct {
		name   string
		health *AccountHealth
		need   bool
	}{
		{"healthy", okHealth, false},
		{"rejected secret", &AccountHealth{Exists: true, SecretStored: true, AuthOK: false}, true},
		{"no secret stored", &AccountHealth{Exists: true}, true},
		{"daemon does not know the account", &AccountHealth{}, true},
		{"daemon could not be asked", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cur := installedHost(Linux, aliceGmail, nil).inspect(t, Linux)
			cur.Health = tc.health
			plan := mustBuild(t, l, cur, gmailRequest())
			if plan.NeedSecret != tc.need {
				t.Errorf("NeedSecret = %v, want %v", plan.NeedSecret, tc.need)
			}
			_, store := findAction(plan, ActStoreSecret)
			if store != tc.need {
				t.Errorf("store-secret planned = %v, want %v", store, tc.need)
			}
			if _, ok := findAction(plan, ActTestAccount); !ok {
				t.Error("no test-account")
			}
		})
	}
}

func TestSame(t *testing.T) {
	t.Parallel()
	l := LayoutFor(Linux)
	build := func() Plan {
		return mustBuild(t, l, freshHost(Linux).inspect(t, Linux), gmailRequest())
	}
	a, b := build(), build()
	if !Same(a, b) {
		t.Fatal("two builds from the same inputs differ")
	}

	mutations := map[string]func(p *Plan){
		"action kind":      func(p *Plan) { p.Actions[0].Kind = ActCreateUser },
		"action entry":     func(p *Plan) { p.Actions[3].Entry = "state-dir" },
		"action name":      func(p *Plan) { p.Actions[2].Name = "bob" },
		"action data":      func(p *Plan) { i := indexOf(*p, ActWriteConfig); p.Actions[i].Data = append(p.Actions[i].Data, '#') },
		"action note":      func(p *Plan) { p.Actions[0].Note = "something else" },
		"dropped action":   func(p *Plan) { p.Actions = p.Actions[1:] },
		"added action":     func(p *Plan) { p.Actions = append(p.Actions, Action{Kind: ActTestAccount, Name: "alice", Note: "x"}) },
		"need secret":      func(p *Plan) { p.NeedSecret = false },
		"service changed":  func(p *Plan) { p.ServiceChanged = false },
		"secret name":      func(p *Plan) { p.SecretName = "accounts/bob" },
		"account":          func(p *Plan) { p.Account.Address = "bob@gmail.com" },
		"oauth client":     func(p *Plan) { p.OAuthClient = &OAuthClientSpec{Name: "google", ClientID: "x"} },
		"backup":           func(p *Plan) { p.Backup = BackupShow },
		"invoking user":    func(p *Plan) { p.InvokingUser = "bob" },
		"platform":         func(p *Plan) { p.Platform = Darwin },
		"version":          func(p *Plan) { p.Version = 2 },
		"next steps":       func(p *Plan) { p.NextSteps = []string{"x"} },
		"account override": func(p *Plan) { p.Account.IMAP = e2eOverrideForTest() },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := build()
			mutate(&c)
			if Same(a, c) || Same(c, a) {
				t.Errorf("Same is true after changing %s", name)
			}
		})
	}
}

func indexOf(p Plan, kind ActionKind) int {
	for i, a := range p.Actions {
		if a.Kind == kind {
			return i
		}
	}
	return -1
}

func TestPlanRender(t *testing.T) {
	t.Parallel()
	for _, p := range []Platform{Linux, Darwin} {
		t.Run(string(p), func(t *testing.T) {
			t.Parallel()
			plan := mustBuild(t, LayoutFor(p), freshHost(p).inspect(t, p), workspaceRequest(true))
			var b bytes.Buffer
			if err := plan.Render(&b); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
			if len(lines) != len(plan.Actions) {
				t.Fatalf("%d lines for %d actions:\n%s", len(lines), len(plan.Actions), b.String())
			}
			for i, line := range lines {
				want := strconv.Itoa(i+1) + ". " + plan.Actions[i].Note
				if strings.TrimLeft(line, " ") != want {
					t.Errorf("line %d = %q, want %q", i+1, line, want)
				}
			}
			out := b.String()
			for _, banned := range []string{"AGE-SECRET-KEY", "client_id", workspaceClientID, "[accounts"} {
				if strings.Contains(out, banned) {
					t.Errorf("rendered plan holds %q:\n%s", banned, out)
				}
			}
		})
	}

	t.Run("write error", func(t *testing.T) {
		t.Parallel()
		plan := mustBuild(t, LayoutFor(Linux), freshHost(Linux).inspect(t, Linux), gmailRequest())
		if err := plan.Render(failWriter{}); err == nil {
			t.Error("Render to a failing writer succeeded")
		}
	})
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, fs.ErrClosed }

// The plan reaches bootstrap apply as JSON, so it must survive the trip.
func TestPlanJSONRoundTrip(t *testing.T) {
	t.Parallel()
	for _, req := range []Request{gmailRequest(), workspaceRequest(false)} {
		l := LayoutFor(Darwin)
		p := mustBuild(t, l, freshHost(Darwin).inspect(t, Darwin), req)
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var back Plan
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		if !Same(p, back) {
			t.Errorf("%s: plan changed in a JSON round trip", req.Address)
		}
		if err := Validate(l, back); err != nil {
			t.Errorf("%s: Validate after a JSON round trip: %v", req.Address, err)
		}
	}
}
