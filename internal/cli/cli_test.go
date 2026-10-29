package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/oauthtest"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

var update = flag.Bool("update", false, "rewrite golden files")

const twoAccounts = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.icloud]
provider = "icloud"
address = "me@icloud.com"
`

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s\n got: %s\nwant: %s", path, got, want)
	}
}

func errorReason(t *testing.T, stderr string) string {
	t.Helper()
	var out struct {
		Error struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &out); err != nil {
		t.Fatalf("stderr is not an error JSON line: %q", stderr)
	}
	return out.Error.Reason
}

func TestAccountsListGolden(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{})
	if got := runCLI(t, Env{Stdin: strings.NewReader("pw\n")}, "--socket", h.Socket, "secret", "set", "accounts/icloud"); got.code != 0 {
		t.Fatalf("secret set: %+v", got)
	}
	got := runCLI(t, Env{}, "--socket", h.Socket, "accounts", "list")
	if got.code != 0 {
		t.Fatalf("accounts list: %+v", got)
	}
	golden(t, "accounts_list", got.stdout)
}

func TestAccountsListTable(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{})
	got := runCLI(t, Env{}, "--socket", h.Socket, "--output", "table", "accounts", "list")
	if got.code != 0 {
		t.Fatalf("accounts list: %+v", got)
	}
	if !strings.HasPrefix(got.stdout, "NAME") || !strings.Contains(got.stdout, "auth required") {
		t.Errorf("table = %q", got.stdout)
	}
}

func TestAccountsTestExitCodes(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	imapEP := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: "me@example.test", Password: "pw"})
	smtpEP := smtptest.Start(t, ca, provider.SecurityStartTLS, smtptest.User{Username: "me@example.test", Password: "pw"})
	cfg := fmt.Sprintf(`
[accounts.good]
provider = "custom"
address = "me@example.test"
imap = { host = "127.0.0.1", port = %[1]d }
smtp = { host = "127.0.0.1", port = %[2]d, security = "starttls" }

[accounts.bad]
provider = "custom"
address = "me@example.test"
imap = { host = "127.0.0.1", port = %[1]d }
smtp = { host = "127.0.0.1", port = %[2]d, security = "starttls" }
`, imapEP.Port, smtpEP.Port)
	h := daemontest.Start(t, cfg, daemontest.Options{RootCAs: ca.Pool})
	for name, pw := range map[string]string{"good": "pw", "bad": "wrong"} {
		if got := runCLI(t, Env{Stdin: strings.NewReader(pw)}, "--socket", h.Socket, "secret", "set", "accounts/"+name); got.code != 0 {
			t.Fatalf("secret set %s: %+v", name, got)
		}
	}

	good := runCLI(t, Env{}, "--socket", h.Socket, "accounts", "test", "good")
	if good.code != 0 {
		t.Errorf("good: %+v", good)
	}
	var resp struct {
		Imap struct {
			Ok bool `json:"ok"`
		} `json:"imap"`
	}
	if err := json.Unmarshal([]byte(good.stdout), &resp); err != nil || !resp.Imap.Ok {
		t.Errorf("good stdout = %s (%v)", good.stdout, err)
	}

	bad := runCLI(t, Env{}, "--socket", h.Socket, "accounts", "test", "bad")
	if bad.code != 5 || errorReason(t, bad.stderr) != "AUTH_REJECTED" {
		t.Errorf("bad: %+v", bad)
	}

	unknown := runCLI(t, Env{}, "--socket", h.Socket, "accounts", "test", "nope")
	if unknown.code != 6 || errorReason(t, unknown.stderr) != "NOT_FOUND" {
		t.Errorf("unknown: %+v", unknown)
	}
}

func TestSecretSet(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{})
	got := runCLI(t, Env{Stdin: strings.NewReader("hunter2\n")}, "--socket", h.Socket, "secret", "set", "accounts/host")
	if got.code != 0 || got.stdout != `{"name":"accounts/host","set":true}`+"\n" {
		t.Fatalf("secret set: %+v", got)
	}
	if v, err := h.Vault.Get("accounts/host"); err != nil || string(v) != "hunter2" {
		t.Errorf("vault = %q, %v (the trailing newline must be trimmed)", v, err)
	}
	assertNoSecret(t, "hunter2", got.stdout, got.stderr)
	status := runCLI(t, Env{}, "--socket", h.Socket, "admin", "status")
	if status.code != 0 || !strings.Contains(status.stdout, `"secret_names":["accounts/host"]`) {
		t.Errorf("admin status: %+v", status)
	}
}

func TestSecretSetFromPass(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{})
	var asked string
	env := Env{Getenv: passStore(t, "", "email/hostinger"), PassShow: func(_ context.Context, path string) ([]byte, error) {
		asked = path
		return []byte("s3cret\nuser: me@example.com\n"), nil
	}}
	got := runCLI(t, env, "--socket", h.Socket, "secret", "set", "accounts/host", "--from-pass", "email/hostinger")
	if got.code != 0 {
		t.Fatalf("secret set: %+v", got)
	}
	if asked != "email/hostinger" {
		t.Errorf("pass path = %q", asked)
	}
	if v, err := h.Vault.Get("accounts/host"); err != nil || string(v) != "s3cret" {
		t.Errorf("vault = %q, %v, want only the first line", v, err)
	}
	assertNoSecret(t, "s3cret", got.stdout, got.stderr)
}

func TestSecretSetFromTerminal(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{})
	var stderr bytes.Buffer
	env := Env{Stderr: &stderr, ReadPassword: func() ([]byte, error) { return []byte("typed-value"), nil }}
	got := runCLI(t, env, "--socket", h.Socket, "secret", "set", "accounts/host")
	if got.code != 0 {
		t.Fatalf("secret set: %+v %s", got, stderr.String())
	}
	assertNoSecret(t, "typed-value", got.stdout, stderr.String())
	if !strings.Contains(stderr.String(), "Secret for accounts/host:") {
		t.Errorf("no prompt: %q", stderr.String())
	}
	if v, err := h.Vault.Get("accounts/host"); err != nil || string(v) != "typed-value" {
		t.Errorf("vault = %q, %v", v, err)
	}
}

func TestSecretSetErrors(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{})
	empty := runCLI(t, Env{Stdin: strings.NewReader("\n")}, "--socket", h.Socket, "secret", "set", "accounts/host")
	if empty.code != 2 || errorReason(t, empty.stderr) != "USAGE" {
		t.Errorf("empty: %+v", empty)
	}
	badName := runCLI(t, Env{Stdin: strings.NewReader("x")}, "--socket", h.Socket, "secret", "set", "../x")
	if badName.code != 2 || errorReason(t, badName.stderr) != "INVALID_ARGUMENT" {
		t.Errorf("bad name: %+v", badName)
	}
	// PLAIN separates its fields with NUL, so a password cannot hold one.
	nul := runCLI(t, Env{Stdin: strings.NewReader("hunter\x00two\n")}, "--socket", h.Socket, "secret", "set", "accounts/host")
	if nul.code != 2 || errorReason(t, nul.stderr) != "INVALID_ARGUMENT" {
		t.Errorf("NUL in password: %+v", nul)
	}
	assertNoSecret(t, "hunter", nul.stdout, nul.stderr)
	if _, err := h.Vault.Get("accounts/host"); err == nil {
		t.Error("a password with a NUL byte was stored")
	}
	passFails := runCLI(t, Env{Getenv: passStore(t, "", "x"), PassShow: func(context.Context, string) ([]byte, error) { return nil, errors.New("gpg locked") }},
		"--socket", h.Socket, "secret", "set", "accounts/host", "--from-pass", "x")
	if passFails.code != 1 || !strings.Contains(passFails.stderr, "gpg locked") {
		t.Errorf("pass failure: %+v", passFails)
	}
}

// pasteReader plays the user pasting the redirect URL. It builds the URL from
// the consent URL the CLI has already printed to stderr.
type pasteReader struct {
	t      *testing.T
	stderr *bytes.Buffer
	r      *strings.Reader
}

func (p *pasteReader) Read(b []byte) (int, error) {
	if p.r == nil {
		consent := ""
		for line := range strings.Lines(p.stderr.String()) {
			if s := strings.TrimSpace(line); strings.HasPrefix(s, "http") {
				consent = s
				break
			}
		}
		p.r = strings.NewReader(oauthtest.RedirectFor(p.t, consent, oauthtest.Code) + "\n")
	}
	return p.r.Read(b)
}

func TestAuthLogin(t *testing.T) {
	t.Parallel()
	o := oauthtest.Start(t)
	cfg := `
[oauth_clients.g]
client_id = "client-id"

[accounts.gm]
provider = "gmail"
address = "me@gmail.com"
auth = "oauth2"
oauth_client = "g"
`
	h := daemontest.Start(t, cfg, daemontest.Options{AuthOptions: []auth.Option{auth.WithOAuthEndpoint(o.Endpoint())}})
	if got := runCLI(t, Env{Stdin: strings.NewReader(oauthtest.ClientSecret)}, "--socket", h.Socket, "secret", "set", "oauth_clients/g"); got.code != 0 {
		t.Fatalf("secret set: %+v", got)
	}
	var stderr bytes.Buffer
	env := Env{Stderr: &stderr, Stdin: &pasteReader{t: t, stderr: &stderr}}
	got := runCLI(t, env, "--socket", h.Socket, "auth", "login", "gm")
	if got.code != 0 {
		t.Fatalf("auth login: code %d, stderr %s", got.code, stderr.String())
	}
	if got.stdout != `{"account":"gm","logged_in":true}`+"\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if rt, err := h.Vault.Get(auth.RefreshTokenSecret("gm")); err != nil || string(rt) != oauthtest.RefreshToken {
		t.Errorf("refresh token = %q, %v", rt, err)
	}
}

func TestDaemonUnreachable(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{}, "--socket", filepath.Join(t.TempDir(), "none.sock"), "admin", "status")
	if got.code != 7 || errorReason(t, got.stderr) != "DAEMON_UNREACHABLE" {
		t.Errorf("got %+v", got)
	}
}

func TestSocketFromEnvironment(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{})
	env := Env{Getenv: func(k string) string {
		if k == "CHAINMAIL_SOCKET" {
			return h.Socket
		}
		return ""
	}}
	if got := runCLI(t, env, "admin", "status"); got.code != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestOutputFlagValidated(t *testing.T) {
	t.Parallel()
	if got := runCLI(t, Env{}, "--output", "yaml", "version"); got.code != 2 {
		t.Errorf("got %+v", got)
	}
}

func TestDaemonFlagErrors(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(twoAccounts), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "dev with production state", args: []string{"daemon", "--dev", "--config", cfgPath, "--state-dir", "/var/lib/chainmail", "--vault-key-file", "/x"}, want: "--state-dir other than"},
		{name: "no vault key", args: []string{"daemon", "--dev", "--config", cfgPath, "--state-dir", t.TempDir()}, want: "no vault key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, tt.args...)
			if got.code != 2 || errorReason(t, got.stderr) != "USAGE" || !strings.Contains(got.stderr, tt.want) {
				t.Errorf("got %+v, want exit 2 with %q", got, tt.want)
			}
		})
	}
}

func TestDaemonCommandServes(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "cmcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(twoAccounts), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "vault-key")
	if err := os.WriteFile(keyPath, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "d.sock")

	ctx, cancel := context.WithCancel(t.Context())
	var daemonErr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Execute(ctx, []string{
			"--socket", socket, "daemon", "--dev",
			"--config", cfgPath, "--state-dir", filepath.Join(dir, "state"), "--vault-key-file", keyPath,
		}, Env{Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &daemonErr, Getenv: func(string) string { return "" }})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		got := runCLI(t, Env{}, "--socket", socket, "admin", "status")
		if got.code == 0 {
			if !strings.Contains(got.stdout, `"dev_mode":true`) {
				t.Errorf("status = %s", got.stdout)
			}
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done // the daemon goroutine writes daemonErr; wait before reading it
			t.Fatalf("daemon never answered; last: %+v; daemon stderr: %s", got, daemonErr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("daemon exit code = %d; stderr: %s", code, daemonErr.String())
	}
	if !strings.Contains(daemonErr.String(), socket) {
		t.Errorf("daemon did not log its socket %s: %s", socket, daemonErr.String())
	}
}

func TestSecretSetInvalidNameDoesNotPrompt(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{})
	env := Env{
		ReadPassword: func() ([]byte, error) { t.Error("prompted for a secret with an invalid name"); return []byte("x"), nil },
		PassShow: func(context.Context, string) ([]byte, error) {
			t.Error("ran pass for a secret with an invalid name")
			return []byte("x"), nil
		},
	}
	for _, args := range [][]string{{"../x"}, {"Bad", "--from-pass", "x"}} {
		got := runCLI(t, env, append([]string{"--socket", h.Socket, "secret", "set"}, args...)...)
		if got.code != 2 || errorReason(t, got.stderr) != "INVALID_ARGUMENT" {
			t.Errorf("%v: %+v", args, got)
		}
	}
}

func TestDevRefusesProductionStateDirSpellings(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"/var/lib/chainmail/", "/var/lib/./chainmail", "/var/lib/../lib/chainmail"} {
		got := runCLI(t, Env{}, "daemon", "--dev", "--state-dir", dir, "--vault-key-file", "/x")
		if got.code != 2 || !strings.Contains(got.stderr, "/var/lib/chainmail") {
			t.Errorf("%s: %+v", dir, got)
		}
	}
}

func TestRunCaptureReportsStderr(t *testing.T) {
	t.Parallel()
	out, err := runCapture(t.Context(), "sh", "-c", "printf 'top\\nrest\\n'")
	if err != nil || string(out) != "top\nrest\n" {
		t.Errorf("success: %q, %v", out, err)
	}
	_, err = runCapture(t.Context(), "sh", "-c", "echo 'Error: x is not in the password store.' >&2; exit 1")
	if err == nil || !strings.Contains(err.Error(), "Error: x is not in the password store.") {
		t.Errorf("failure error = %v", err)
	}
}

func TestAuthLoginNothingPasted(t *testing.T) {
	t.Parallel()
	o := oauthtest.Start(t)
	cfg := `
[oauth_clients.g]
client_id = "client-id"

[accounts.gm]
provider = "gmail"
address = "me@gmail.com"
auth = "oauth2"
oauth_client = "g"
`
	h := daemontest.Start(t, cfg, daemontest.Options{AuthOptions: []auth.Option{auth.WithOAuthEndpoint(o.Endpoint())}})
	if got := runCLI(t, Env{Stdin: strings.NewReader(oauthtest.ClientSecret)}, "--socket", h.Socket, "secret", "set", "oauth_clients/g"); got.code != 0 {
		t.Fatalf("secret set: %+v", got)
	}
	var stderr bytes.Buffer
	got := runCLI(t, Env{Stderr: &stderr, Stdin: strings.NewReader("\n")}, "--socket", h.Socket, "auth", "login", "gm")
	last := strings.TrimSpace(stderr.String())
	last = last[strings.LastIndex(last, "\n")+1:]
	if got.code != 2 || errorReason(t, last) != "USAGE" {
		t.Errorf("code %d, stderr %s", got.code, stderr.String())
	}
}

func TestSecretSetRejectsBadInput(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{})
	passCalled := false
	env := Env{PassShow: func(context.Context, string) ([]byte, error) { passCalled = true; return []byte("x"), nil }}
	dash := runCLI(t, env, "--socket", h.Socket, "secret", "set", "accounts/host", "--from-pass=--clip")
	if dash.code != 2 || errorReason(t, dash.stderr) != "INVALID_ARGUMENT" || passCalled {
		t.Errorf("pass path starting with -: %+v (pass called %t)", dash, passCalled)
	}
	dots := runCLI(t, env, "--socket", h.Socket, "secret", "set", "accounts/host", "--from-pass", "email/../../x")
	if dots.code != 2 || errorReason(t, dots.stderr) != "INVALID_ARGUMENT" || passCalled {
		t.Errorf("pass path with ..: %+v (pass called %t)", dots, passCalled)
	}
	multi := runCLI(t, Env{Stdin: strings.NewReader("s3cret\nuser: me@example.com\n")}, "--socket", h.Socket, "secret", "set", "accounts/host")
	if multi.code != 2 || errorReason(t, multi.stderr) != "USAGE" || !strings.Contains(multi.stderr, "--from-pass") {
		t.Errorf("multi-line stdin: %+v", multi)
	}
	huge := runCLI(t, Env{Stdin: strings.NewReader(strings.Repeat("a", maxSecretBytes+1))}, "--socket", h.Socket, "secret", "set", "accounts/host")
	if huge.code != 2 || errorReason(t, huge.stderr) != "USAGE" {
		t.Errorf("oversized stdin: %+v", huge)
	}
	if _, err := h.Vault.Get("accounts/host"); err == nil {
		t.Error("a rejected secret was stored")
	}
}

func TestIsProductionDirFollowsSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	prod := filepath.Join(dir, "prod")
	if err := os.Mkdir(prod, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(prod, link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	for path, want := range map[string]bool{
		prod: true, prod + "/": true, link: true,
		other: false, filepath.Join(dir, "missing"): false,
	} {
		if got := isProductionDir(path, prod); got != want {
			t.Errorf("isProductionDir(%s) = %t, want %t", path, got, want)
		}
	}
}

func TestCheckResultWithoutReason(t *testing.T) {
	t.Parallel()
	err := checkResult(&chainmailv1.TestAccountResponse{Account: "a", Imap: &chainmailv1.Check{}, Smtp: &chainmailv1.Check{Ok: true}})
	if c := classify(err); c.reason != "INTERNAL" || c.code != 1 {
		t.Errorf("classify = %+v", c)
	}
}

func TestWriteJSONDoesNotEscapeHTML(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeJSON(&buf, map[string]string{"hint": "a&b <c>"}); err != nil {
		t.Fatal(err)
	}
	if want := `{"hint":"a&b <c>"}` + "\n"; buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

// assertNoSecret fails if secret appears in any of the outputs.
func assertNoSecret(t *testing.T, secret string, outputs ...string) {
	t.Helper()
	for _, out := range outputs {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks the secret: %q", out)
		}
	}
}

// passStore creates a password store holding entries and returns a Getenv
// that points at it through PASSWORD_STORE_DIR, or through HOME when via is "HOME".
func passStore(t *testing.T, via string, entries ...string) func(string) string {
	t.Helper()
	dir := t.TempDir()
	store := dir
	if via == "HOME" {
		store = filepath.Join(dir, ".password-store")
	}
	for _, e := range entries {
		path := filepath.Join(store, e+".gpg")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("encrypted"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	key := "PASSWORD_STORE_DIR"
	if via == "HOME" {
		key = "HOME"
	}
	return func(k string) string {
		if k == key {
			return dir
		}
		return ""
	}
}

func TestSecretSetFromPassRefusesFolders(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{})
	const value = "do-not-print-me"

	t.Run("missing entry", func(t *testing.T) {
		t.Parallel()
		called := false
		env := Env{Getenv: passStore(t, "", "email/other"), PassShow: func(context.Context, string) ([]byte, error) {
			called = true
			return []byte(value), nil
		}}
		got := runCLI(t, env, "--socket", h.Socket, "secret", "set", "accounts/host", "--from-pass", "email")
		if got.code != 6 || errorReason(t, got.stderr) != "NOT_FOUND" || !strings.Contains(got.stderr, `\"email\"`) || called {
			t.Errorf("got %+v (pass called %t)", got, called)
		}
	})
	t.Run("store found through HOME", func(t *testing.T) {
		t.Parallel()
		env := Env{Getenv: passStore(t, "HOME", "email/hostinger"), PassShow: func(context.Context, string) ([]byte, error) {
			return []byte(value + "\n"), nil
		}}
		if got := runCLI(t, env, "--socket", h.Socket, "secret", "set", "accounts/host", "--from-pass", "email/hostinger"); got.code != 0 {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("no store", func(t *testing.T) {
		t.Parallel()
		env := Env{PassShow: func(context.Context, string) ([]byte, error) { return []byte(value), nil }}
		got := runCLI(t, env, "--socket", h.Socket, "secret", "set", "accounts/host", "--from-pass", "email/hostinger")
		if got.code == 0 || !strings.Contains(got.stderr, "PASSWORD_STORE_DIR") {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("folder listing", func(t *testing.T) {
		t.Parallel()
		// Output with terminal escape codes is refused even when a .gpg file
		// exists, since it cannot be the entry's first line.
		listing := "\x1b[01;34memail\x1b[0m\n├── " + value + "\n"
		env := Env{Getenv: passStore(t, "", "email"), PassShow: func(context.Context, string) ([]byte, error) { return []byte(listing), nil }}
		got := runCLI(t, env, "--socket", h.Socket, "secret", "set", "accounts/icloud", "--from-pass", "email")
		if got.code != 2 || errorReason(t, got.stderr) != "INVALID_ARGUMENT" || !strings.Contains(got.stderr, `\"email\"`) {
			t.Errorf("got %+v", got)
		}
		assertNoSecret(t, value, got.stdout, got.stderr)
		if _, err := h.Vault.Get("accounts/icloud"); err == nil {
			t.Error("a folder listing was stored")
		}
	})
}

// cancelReader cancels the command's context on its first Read, then blocks
// like a terminal nobody types into.
type cancelReader struct {
	cancel context.CancelFunc
	block  chan struct{}
}

func (r *cancelReader) Read([]byte) (int, error) {
	r.cancel()
	<-r.block
	return 0, io.EOF
}

func TestAuthLoginPasteCancel(t *testing.T) {
	t.Parallel()
	o := oauthtest.Start(t)
	cfg := `
[oauth_clients.g]
client_id = "client-id"

[accounts.gm]
provider = "gmail"
address = "me@gmail.com"
auth = "oauth2"
oauth_client = "g"
`
	h := daemontest.Start(t, cfg, daemontest.Options{AuthOptions: []auth.Option{auth.WithOAuthEndpoint(o.Endpoint())}})
	if got := runCLI(t, Env{Stdin: strings.NewReader(oauthtest.ClientSecret)}, "--socket", h.Socket, "secret", "set", "oauth_clients/g"); got.code != 0 {
		t.Fatalf("secret set: %+v", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	r := &cancelReader{cancel: cancel, block: make(chan struct{})}
	t.Cleanup(func() { close(r.block) })
	done := make(chan int, 1)
	go func() {
		done <- Execute(ctx, []string{"--socket", h.Socket, "auth", "login", "gm"},
			Env{Stdin: r, Stdout: io.Discard, Stderr: io.Discard, Getenv: func(string) string { return "" }})
	}()
	select {
	case code := <-done:
		if code == 0 {
			t.Error("a cancelled login exited 0")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("auth login kept waiting for a paste after its context was cancelled")
	}
}

func TestReadInterruptible(t *testing.T) {
	t.Parallel()
	got, err := readInterruptible(func() ([]byte, error) { return []byte("pw"), nil }, make(chan os.Signal), func() { t.Error("restored after a normal read") })
	if err != nil || string(got) != "pw" {
		t.Errorf("read = %q, %v", got, err)
	}

	block := make(chan struct{})
	defer close(block)
	sig := make(chan os.Signal, 1)
	sig <- os.Interrupt
	restored := false
	_, err = readInterruptible(func() ([]byte, error) { <-block; return nil, nil }, sig, func() { restored = true })
	if !errors.Is(err, errInterrupted) || !restored {
		t.Errorf("interrupted read: err %v, restored %t", err, restored)
	}
}

func TestDaemonSocketResolution(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(twoAccounts+"\n[daemon]\nsocket = \"/run/from-config.sock\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		flag    string
		env     map[string]string
		dev     bool
		want    string
		wantErr bool
	}{
		{name: "dev flag wins", flag: "/f.sock", env: map[string]string{"CHAINMAIL_SOCKET": "/e.sock", "XDG_RUNTIME_DIR": "/x"}, dev: true, want: "/f.sock"},
		{name: "dev environment next", env: map[string]string{"CHAINMAIL_SOCKET": "/e.sock", "XDG_RUNTIME_DIR": "/x"}, dev: true, want: "/e.sock"},
		{name: "dev runtime dir last", env: map[string]string{"XDG_RUNTIME_DIR": "/x"}, dev: true, want: "/x/chainmail-dev.sock"},
		{name: "dev with nothing", dev: true, wantErr: true},
		{name: "production flag", flag: "/f.sock", want: "/f.sock"},
		{name: "production uses the config", env: map[string]string{"CHAINMAIL_SOCKET": "/e.sock"}, want: "/run/from-config.sock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &daemonFlags{configPath: cfgPath, stateDir: t.TempDir(), vaultKey: "/k", dev: tt.dev}
			env := Env{Getenv: func(k string) string { return tt.env[k] }}
			p, err := f.resolve(env, &globals{socket: tt.flag})
			if tt.wantErr {
				var ue usageError
				if !errors.As(err, &ue) {
					t.Fatalf("err = %v, want a usage error", err)
				}
				return
			}
			if err != nil || p.socket != tt.want {
				t.Errorf("socket = %q, %v, want %q", p.socket, err, tt.want)
			}
		})
	}
}

func TestChecksTable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := checksTable(&buf, &chainmailv1.TestAccountResponse{
		Account: "a",
		Imap:    &chainmailv1.Check{Ok: true, LatencyMs: 12},
		Smtp:    &chainmailv1.Check{Reason: "AUTH_REJECTED", LatencyMs: 30, Error: "535 bad credentials"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "CHECK  OK     REASON         LATENCY  ERROR\n" +
		"imap   true                  12ms     \n" +
		"smtp   false  AUTH_REJECTED  30ms     535 bad credentials\n"
	if buf.String() != want {
		t.Errorf("table =\n%s\nwant\n%s", buf.String(), want)
	}
}
