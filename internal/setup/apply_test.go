package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/setup/setuptest"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

// userExe is where the invoking user's chainmail binary lives in the
// apply tests, and newBinary its contents: a version the host fakes have
// not installed.
const userExe = "/home/alice/bin/chainmail"

var newBinary = []byte("chainmail binary v2")

// testSecret is the password the apply tests store.
var testSecret = []byte("app-password-1234")

// applyStart is the fake clock's time when a test starts.
var applyStart = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

// rig is an Applier wired to fakes that share one journal.
type rig struct {
	l        Layout
	h        host
	journal  *setuptest.Journal
	clock    *setuptest.Clock
	svc      *setuptest.Service
	daemon   *setuptest.Daemon
	tty      *setuptest.TTY
	ttyErr   error
	ttyOpens int
	stderr   bytes.Buffer
	applier  *Applier
}

// newRig wires h for platform p. The user's binary is newBinary at
// userExe.
func newRig(p Platform, h host) *rig {
	l := LayoutFor(p)
	r := &rig{l: l, h: h, journal: &setuptest.Journal{}, clock: setuptest.NewClock(applyStart)}
	h.files.Dir("/home/alice/bin", setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755).
		File(userExe, setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755, newBinary)
	h.files.Journal = r.journal
	h.accounts.Journal = r.journal
	r.svc = &setuptest.Service{Clock: r.clock, Journal: r.journal, Launchd: p == Darwin}
	r.daemon = &setuptest.Daemon{Service: r.svc, Socket: l.Socket, Journal: r.journal}
	r.tty = setuptest.NewTTY("\n")
	r.applier = &Applier{
		Layout:     l,
		Files:      h.files,
		Accounts:   h.accounts,
		Service:    r.svc,
		AsDaemon:   r.daemon.Run,
		Executable: userExe,
		OpenTTY: func() (io.ReadWriteCloser, error) {
			r.ttyOpens++
			if r.ttyErr != nil {
				return nil, r.ttyErr
			}
			return r.tty, nil
		},
		Now:    r.clock.Now,
		Sleep:  r.clock.Sleep,
		Stderr: &r.stderr,
	}
	return r
}

// freshRig is a fresh Linux or macOS host.
func freshRig(p Platform) *rig { return newRig(p, freshHost(p)) }

// installedRig is a host bootstrap set up for alice's Gmail account, with
// the daemon running and alice's secret stored.
func installedRig(p Platform) *rig {
	r := newRig(p, installedHost(p, aliceGmail, nil))
	r.svc.SetState(system.ServiceState{Loaded: true, Running: true})
	r.daemon.SetSecret("accounts/alice", []byte("old")).SetAuthState("alice", setuptest.AuthStateOK)
	return r
}

// request is gmailRequest running newBinary.
func request() Request {
	req := gmailRequest()
	req.ExecSHA256 = sha256Hex(newBinary)
	return req
}

// input plans req on the rig's host as alice would, with health, and
// returns the Input bootstrap apply gets.
func (r *rig) input(t *testing.T, req Request, health *AccountHealth, secret []byte) Input {
	t.Helper()
	cur := r.h.inspect(t, r.l.Platform)
	cur.Health = health
	alice, _ := r.h.accounts.User("alice")
	return Input{
		Plan:     mustBuild(t, r.l, cur, req),
		Request:  req,
		Secret:   secret,
		Health:   health,
		SudoUID:  alice.UID,
		SudoUser: "alice",
	}
}

func (r *rig) apply(in Input) (Result, error) {
	return r.applier.Apply(context.Background(), in)
}

// daemonCalls lists the daemon-user runs as their joined arguments.
func (r *rig) daemonCalls() []string {
	var out []string
	for _, c := range r.daemon.Calls() {
		out = append(out, strings.Join(c.Args, " "))
	}
	return out
}

// countPrefix counts entries starting with prefix.
func countPrefix(entries []string, prefix string) int {
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func wantRefusal(t *testing.T, err error, substr string) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("error = %v (%T), want a *Refusal", err, err)
	}
	if !strings.Contains(r.Msg, substr) {
		t.Errorf("refusal = %q, want it to mention %q", r.Msg, substr)
	}
}

func TestApplyFresh(t *testing.T) {
	t.Parallel()
	r := freshRig(Linux)
	l := r.l
	in := r.input(t, request(), nil, testSecret)

	res, err := r.apply(in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	lock := "lock " + l.Lock
	want := []string{
		// The lock lives in the config directory, so that comes first.
		"mkdir " + l.ConfigDir,
		lock,
		"service state",
		"create-group chainmail-clients",
		"create-user chainmail",
		"add-member chainmail-clients alice",
		"mkdir " + l.BinaryDir,
		"mkdir " + l.ConfigDir,
		"mkdir " + l.StateDir,
		"write " + l.Binary,
		"create " + l.VaultKey,
		"write " + l.Config,
		"write " + l.Service,
		"service activate true",
		"daemon admin status",
		"daemon secret set accounts/alice",
		"daemon accounts test alice",
		"service state",
		"unlock " + l.Lock,
	}
	if got := r.journal.Entries(); !slices.Equal(got, want) {
		t.Errorf("journal:\n got %q\nwant %q", got, want)
	}

	user, _ := r.h.accounts.User(l.DaemonUser)
	group, _ := r.h.accounts.Group(l.DaemonUser)
	files := []struct {
		path     string
		uid, gid uint32
		mode     fs.FileMode
		data     []byte
	}{
		{l.Binary, 0, 0, 0o755, newBinary},
		{l.Config, 0, 0, 0o644, actionData(t, in.Plan, ActWriteConfig)},
		{l.Service, 0, 0, 0o644, RenderService(l)},
		{l.VaultKey, user.UID, group.GID, 0o400, nil},
		{l.StateDir, user.UID, group.GID, fs.ModeDir | 0o700, nil},
		{l.BinaryDir, 0, 0, fs.ModeDir | 0o755, nil},
	}
	for _, f := range files {
		data, info, ok := r.h.files.Content(f.path)
		if !ok {
			t.Errorf("%s was not created", f.path)
			continue
		}
		if info.UID != f.uid || info.GID != f.gid || info.Mode != f.mode {
			t.Errorf("%s: %d:%d %v, want %d:%d %v", f.path, info.UID, info.GID, info.Mode, f.uid, f.gid, f.mode)
		}
		if f.data != nil && !bytes.Equal(data, f.data) {
			t.Errorf("%s holds %d bytes that differ from the %d expected", f.path, len(data), len(f.data))
		}
	}
	key, _, _ := r.h.files.Content(l.VaultKey)
	if !bytes.Contains(key, []byte("AGE-SECRET-KEY-")) {
		t.Errorf("vault key (%d bytes) is not an age identity", len(key))
	}

	calls := r.daemon.Calls()
	if got, want := r.daemonCalls(), []string{
		"admin status --output json --socket " + l.Socket,
		"secret set accounts/alice --output json --socket " + l.Socket,
		"accounts test alice --output json --socket " + l.Socket,
	}; !slices.Equal(got, want) {
		t.Errorf("daemon calls:\n got %q\nwant %q", got, want)
	}
	if !bytes.Equal(calls[1].Stdin, testSecret) {
		t.Errorf("secret set read %d bytes from stdin that differ from the %d-byte secret", len(calls[1].Stdin), len(testSecret))
	}
	if stored, _ := r.daemon.Secret("accounts/alice"); !bytes.Equal(stored, testSecret) {
		t.Errorf("stored secret (%d bytes) differs from the %d-byte secret", len(stored), len(testSecret))
	}

	if res.Test != "ok" || !res.ServiceRunning || res.Account != "alice" || res.Provider != "gmail" || res.KeyShown {
		t.Errorf("result = %+v", res)
	}
	if len(res.NextSteps) < 2 || res.NextSteps[0] != "log out and back in, or run: newgrp chainmail-clients" ||
		res.NextSteps[1] != "chainmail accounts list" {
		t.Errorf("NextSteps = %q", res.NextSteps)
	}
	for name, path := range map[string]string{"binary": l.Binary, "config": l.Config, "vault-key": l.VaultKey, "socket": l.Socket, "service": l.Service} {
		if res.Paths[name] != path {
			t.Errorf("Paths[%q] = %q, want %q", name, res.Paths[name], path)
		}
	}
	if r.h.files.Locked(l.Lock) {
		t.Error("the lock is still held")
	}
	if !strings.Contains(r.stderr.String(), in.Plan.Actions[0].Note) {
		t.Errorf("stderr has no progress line for the first step:\n%s", r.stderr.String())
	}
	if bytes.Contains(r.stderr.Bytes(), testSecret) {
		t.Error("the secret appears on stderr")
	}
	if out, _ := json.Marshal(res); bytes.Contains(out, testSecret) {
		t.Error("the secret appears in the result")
	}
}

func TestApplyFreshDarwin(t *testing.T) {
	t.Parallel()
	r := freshRig(Darwin)
	l := r.l
	if _, err := r.apply(r.input(t, request(), nil, testSecret)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// macOS has no /private/var/lib; Apply makes it, root:wheel 0755,
	// before the state directory.
	j := r.journal.Entries()
	parent, state := slices.Index(j, "mkdir /private/var/lib"), slices.Index(j, "mkdir /private/var/lib/chainmail")
	if parent < 0 || state < parent {
		t.Errorf("journal %q does not make /private/var/lib before the state directory", j)
	}
	if _, info, _ := r.h.files.Content("/private/var/lib"); info.UID != 0 || info.GID != 0 || info.Mode != fs.ModeDir|0o755 {
		t.Errorf("/private/var/lib is %+v", info)
	}
	user, _ := r.h.accounts.User(l.DaemonUser)
	clients, _ := r.h.accounts.Group(l.ClientGroup)
	for _, path := range []string{l.VaultKey, l.StateDir, l.SocketDir, l.LogDir} {
		if _, info, _ := r.h.files.Content(path); info.UID != user.UID || info.GID != clients.GID {
			t.Errorf("%s is %d:%d, want %s:%s (%d:%d)", path, info.UID, info.GID, l.DaemonUser, l.ClientGroup, user.UID, clients.GID)
		}
	}
	if c := r.daemon.Calls(); len(c) == 0 || c[0].Socket != "/var/db/chainmail/chainmail.sock" {
		t.Errorf("daemon calls %+v do not name the macOS socket", c)
	}
}

// actionData returns the data of the plan's first action of kind.
func actionData(t *testing.T, p Plan, kind ActionKind) []byte {
	t.Helper()
	a, ok := findAction(p, kind)
	if !ok {
		t.Fatalf("plan has no %s", kind)
	}
	return a.Data
}

func TestApplyLockAndRecheck(t *testing.T) {
	t.Parallel()

	t.Run("lock held from before the first write to the end", func(t *testing.T) {
		t.Parallel()
		r := installedRig(Linux)
		if _, err := r.apply(r.input(t, request(), okHealth, nil)); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		j := r.journal.Entries()
		if len(j) == 0 || j[0] != "lock "+r.l.Lock || j[len(j)-1] != "unlock "+r.l.Lock {
			t.Errorf("journal %q does not start with the lock and end with the unlock", j)
		}
	})

	changes := []struct {
		name    string
		prepare func(r *rig) // before planning
		change  func(r *rig, in *Input)
		want    string
	}{
		{
			name:   "service file removed after planning",
			change: func(r *rig, _ *Input) { r.h.files.Remove(r.l.Service) },
			want:   "the installation changed while bootstrap was running; run it again",
		},
		{
			name:    "config text altered in the plan",
			prepare: func(r *rig) { r.h.files.Remove(r.l.Config) },
			change: func(_ *rig, in *Input) {
				// Validate accepts this: the config still parses and holds
				// the plan's account. Only the rebuild under the lock sees
				// that it is not what bootstrap renders.
				for i, a := range in.Plan.Actions {
					if a.Kind == ActWriteConfig {
						in.Plan.Actions[i].Data = append(slices.Clone(a.Data), "\n[policy]\nallow_expunge = true\n"...)
					}
				}
			},
			want: "the installation changed while bootstrap was running; run it again",
		},
		{
			name:   "health differs from what the plan was built with",
			change: func(_ *rig, in *Input) { in.Health = okHealth },
			want:   "the installation changed while bootstrap was running; run it again",
		},
		{
			name: "sudo user is not the plan's invoking user",
			change: func(r *rig, in *Input) {
				r.h.accounts.AddUser(system.User{Name: "bob", UID: 1001, GID: 1001, Shell: "/bin/bash"})
				in.SudoUser, in.SudoUID = "bob", 1001
			},
			want: "invoking user",
		},
		{
			name:   "sudo uid does not match the sudo user",
			change: func(_ *rig, in *Input) { in.SudoUID = 0 },
			want:   "SUDO_UID",
		},
		{
			name:   "binary replaced after planning",
			change: func(r *rig, _ *Input) { r.h.files.File(userExe, 1000, 1000, 0o755, []byte("other")) },
			want:   "running binary",
		},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := installedRig(Linux)
			if tc.prepare != nil {
				tc.prepare(r)
			}
			in := r.input(t, request(), nil, testSecret)
			if err := Validate(r.l, in.Plan); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			tc.change(r, &in)
			if err := Validate(r.l, in.Plan); err != nil {
				t.Fatalf("Validate after the change: %v", err)
			}
			_, err := r.apply(in)
			wantRefusal(t, err, tc.want)
			if w := r.h.files.Writes(); len(w) != 0 {
				t.Errorf("writes after a refusal: %q", w)
			}
			if c := r.h.accounts.Changes(); len(c) != 0 {
				t.Errorf("account changes after a refusal: %q", c)
			}
			if r.h.files.Locked(r.l.Lock) {
				t.Error("the lock is still held")
			}
		})
	}
}

func TestApplyValidates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(in *Input)
	}{
		{"unknown entry", func(in *Input) {
			in.Plan.Actions = append(in.Plan.Actions, Action{Kind: ActEnsureDir, Entry: "home", Note: "ensure /home"})
		}},
		{"secret needed but missing", func(in *Input) { in.Secret = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := freshRig(Linux)
			in := r.input(t, request(), nil, testSecret)
			tc.change(&in)
			_, err := r.apply(in)
			var ref *Refusal
			if !errors.As(err, &ref) {
				t.Fatalf("Apply = %v, want a *Refusal", err)
			}
			if j := r.journal.Entries(); len(j) != 0 {
				t.Errorf("journal after a refused plan: %q", j)
			}
		})
	}
}

func TestApplyNoRestartWhenUnchanged(t *testing.T) {
	t.Parallel()

	t.Run("running", func(t *testing.T) {
		t.Parallel()
		r := installedRig(Linux)
		req := gmailRequest() // the installed binary
		r.h.files.File(userExe, 1000, 1000, 0o755, fakeBinary)
		in := r.input(t, req, okHealth, nil)
		if in.Plan.ServiceChanged {
			t.Fatal("fixture plan changes the service")
		}
		res, err := r.apply(in)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := r.svc.Calls(); slices.Contains(got, "restart") || countPrefix(got, "activate") != 0 {
			t.Errorf("service calls %q on an unchanged running install", got)
		}
		if got := r.daemonCalls(); len(got) != 1 || !strings.HasPrefix(got[0], "accounts test alice") {
			t.Errorf("daemon calls = %q, want only the account test", got)
		}
		if res.Test != "ok" || !res.ServiceRunning {
			t.Errorf("result = %+v", res)
		}
	})

	t.Run("stopped", func(t *testing.T) {
		t.Parallel()
		r := installedRig(Linux)
		r.svc.SetState(system.ServiceState{Loaded: true})
		r.h.files.File(userExe, 1000, 1000, 0o755, fakeBinary)
		in := r.input(t, gmailRequest(), okHealth, nil)
		res, err := r.apply(in)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got, want := r.svc.Calls(), []string{"state", "activate false", "state"}; !slices.Equal(got, want) {
			t.Errorf("service calls = %q, want %q", got, want)
		}
		if got := r.daemonCalls(); len(got) != 2 || !strings.HasPrefix(got[0], "admin status") {
			t.Errorf("daemon calls = %q, want readiness then the test", got)
		}
		if !res.ServiceRunning {
			t.Error("ServiceRunning = false")
		}
	})
}

func TestApplyRestartsRunningService(t *testing.T) {
	t.Parallel()
	r := installedRig(Linux)
	in := r.input(t, request(), okHealth, nil) // a new binary, nothing else
	if _, err := r.apply(in); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := r.svc.Calls(), []string{"state", "activate false", "restart", "state"}; !slices.Equal(got, want) {
		t.Errorf("service calls = %q, want %q", got, want)
	}
	// The rejected-credentials check reads auth state before the restart.
	j := r.journal.Entries()
	list, restart := slices.Index(j, "daemon accounts list"), slices.Index(j, "service restart")
	if list < 0 || restart < list {
		t.Errorf("journal %q does not list accounts before the restart", j)
	}
}

func TestApplyRejectedBeforeRestart(t *testing.T) {
	t.Parallel()

	t.Run("no new secret", func(t *testing.T) {
		t.Parallel()
		r := installedRig(Linux)
		in := r.input(t, request(), okHealth, nil)
		// Since the plan was made, the daemon found the password rejected.
		r.daemon.SetAuthState("alice", setuptest.AuthStateRequired)
		_, err := r.apply(in)
		var ae *AuthError
		if !errors.As(err, &ae) {
			t.Fatalf("Apply = %v, want an *AuthError", err)
		}
		if ae.Hint != "run bootstrap again and enter the new password" {
			t.Errorf("hint = %q", ae.Hint)
		}
		if got := r.daemonCalls(); len(got) != 2 ||
			!strings.HasPrefix(got[0], "accounts list --output json --socket ") ||
			!strings.HasPrefix(got[1], "admin status --output json --socket ") {
			t.Errorf("daemon calls = %q", got)
		}
		if slices.Contains(r.svc.Calls(), "restart") {
			t.Error("restarted a daemon with rejected credentials")
		}
		if w := r.h.files.Writes(); len(w) != 0 {
			t.Errorf("writes: %q", w)
		}
	})

	t.Run("new secret given", func(t *testing.T) {
		t.Parallel()
		r := installedRig(Linux)
		in := r.input(t, request(), okHealth, testSecret)
		r.daemon.SetAuthState("alice", setuptest.AuthStateRequired)
		res, err := r.apply(in)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		j := r.journal.Entries()
		restart, store := slices.Index(j, "service restart"), slices.Index(j, "daemon secret set accounts/alice")
		if restart < 0 || store < restart {
			t.Errorf("journal %q does not store the new secret after the restart", j)
		}
		if v, _ := r.daemon.Secret("accounts/alice"); !bytes.Equal(v, testSecret) || res.Test != "ok" {
			t.Errorf("stored secret (%d bytes) differs from the new one, or result %+v", len(v), res)
		}
	})

	t.Run("required but no secret stored", func(t *testing.T) {
		t.Parallel()
		// An account that never had a secret is not a rejection.
		r := installedRig(Linux)
		in := r.input(t, request(), okHealth, nil)
		r.daemon = &setuptest.Daemon{Service: r.svc, Socket: r.l.Socket, Journal: r.journal}
		r.applier.AsDaemon = r.daemon.Run
		r.daemon.SetAuthState("alice", setuptest.AuthStateRequired)
		if _, err := r.apply(in); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	})
}

func TestApplyOAuthNeedingLoginIsNotRejected(t *testing.T) {
	t.Parallel()
	// An OAuth account that needs auth needs a new login, not a new client
	// secret, so a binary update still goes ahead without one.
	client := &OAuthClientSpec{Name: "google", ClientID: workspaceClientID}
	spec := AccountSpec{Name: "alice", Provider: "google-workspace", Address: "alice@example.com", Auth: provider.AuthOAuth2, OAuthClient: "google"}
	r := newRig(Linux, installedHost(Linux, spec, client))
	r.svc.SetState(system.ServiceState{Loaded: true, Running: true})
	r.daemon.SetSecret("oauth_clients/google", []byte("old")).SetAuthState("alice", setuptest.AuthStateRequired)
	req := workspaceRequest(true)
	req.ExecSHA256 = sha256Hex(newBinary)
	if _, err := r.apply(r.input(t, req, okHealth, nil)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(r.svc.Calls(), "restart") {
		t.Errorf("service calls = %q, want a restart for the new binary", r.svc.Calls())
	}
}

func TestApplyReadinessTimeout(t *testing.T) {
	t.Parallel()
	r := freshRig(Linux)
	r.svc.Stuck = true
	for i := 1; i <= 30; i++ {
		r.svc.Log = append(r.svc.Log, fmt.Sprintf("log line %02d", i))
	}
	_, err := r.apply(r.input(t, request(), nil, testSecret))
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Apply = %v, want ErrNotReady", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "log line 11") || !strings.Contains(msg, "log line 30") || strings.Contains(msg, "log line 10") {
		t.Errorf("error does not carry the last 20 log lines:\n%s", msg)
	}
	if !slices.Contains(r.svc.Calls(), "logs 20") {
		t.Errorf("service calls %q do not read 20 log lines", r.svc.Calls())
	}
	if elapsed := r.clock.Now().Sub(applyStart); elapsed < readinessTimeout || elapsed > readinessTimeout+time.Second {
		t.Errorf("waited %v, want about %v", elapsed, readinessTimeout)
	}
	if n := countPrefix(r.daemonCalls(), "admin status"); n < 50 {
		t.Errorf("polled %d times in 15s, want every 250ms", n)
	}
	if countPrefix(r.daemonCalls(), "secret set") != 0 {
		t.Error("stored the secret on a daemon that never came up")
	}
	if r.h.files.Locked(r.l.Lock) {
		t.Error("the lock is still held")
	}
}

func TestApplyStaleSocket(t *testing.T) {
	t.Parallel()
	r := freshRig(Linux)
	refused := &setuptest.ExitError{Code: 7, Msg: "connect: connection refused"}
	r.daemon.StatusErrs = []error{refused, refused}
	res, err := r.apply(r.input(t, request(), nil, testSecret))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := countPrefix(r.daemonCalls(), "admin status"); n != 3 {
		t.Errorf("admin status ran %d times, want 3", n)
	}
	if res.Test != "ok" {
		t.Errorf("Test = %q", res.Test)
	}
}

func TestApplyKeyBackup(t *testing.T) {
	t.Parallel()
	showReq := func() Request {
		req := request()
		req.Backup = BackupShow
		return req
	}

	t.Run("show", func(t *testing.T) {
		t.Parallel()
		r := freshRig(Linux)
		res, err := r.apply(r.input(t, showReq(), nil, testSecret))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		key, _, _ := r.h.files.Content(r.l.VaultKey)
		i := bytes.Index(key, []byte("AGE-SECRET-KEY-"))
		if i < 0 {
			t.Fatalf("vault key (%d bytes) holds no identity", len(key))
		}
		secretLine := bytes.TrimSpace(key[i:])
		if !strings.Contains(r.tty.Output(), string(secretLine)) {
			t.Error("the key was not shown on the terminal")
		}
		if !r.tty.Closed() {
			t.Error("the terminal was not closed")
		}
		if bytes.Contains(r.stderr.Bytes(), secretLine) {
			t.Error("the key appears on stderr")
		}
		if out, _ := json.Marshal(res); bytes.Contains(out, secretLine) {
			t.Error("the key appears in the result")
		}
		for _, c := range r.daemon.Calls() {
			if bytes.Contains(c.Stdin, secretLine) || slices.ContainsFunc(c.Args, func(a string) bool { return strings.Contains(a, string(secretLine)) }) {
				t.Errorf("the key was given to the daemon command %q", c.Args[:min(2, len(c.Args))])
			}
		}
		if !res.KeyShown || len(res.Warnings) != 0 {
			t.Errorf("KeyShown = %v, warnings %q", res.KeyShown, res.Warnings)
		}
	})

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		r := freshRig(Linux)
		res, err := r.apply(r.input(t, request(), nil, testSecret))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if r.ttyOpens != 0 || res.KeyShown {
			t.Errorf("OpenTTY called %d times, KeyShown %v", r.ttyOpens, res.KeyShown)
		}
	})

	t.Run("no terminal", func(t *testing.T) {
		t.Parallel()
		r := freshRig(Linux)
		r.ttyErr = errors.New("open /dev/tty: no such device or address")
		res, err := r.apply(r.input(t, showReq(), nil, testSecret))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if res.KeyShown || !slices.Contains(res.Warnings, "could not open the terminal to show the key; it was not shown") {
			t.Errorf("KeyShown = %v, warnings %q", res.KeyShown, res.Warnings)
		}
		if res.Test != "ok" {
			t.Errorf("the install did not finish: %+v", res)
		}
	})

	t.Run("input ends before Enter", func(t *testing.T) {
		t.Parallel()
		r := freshRig(Linux)
		r.tty = setuptest.NewTTY("")
		res, err := r.apply(r.input(t, showReq(), nil, testSecret))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if !res.KeyShown || !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "not confirmed") }) {
			t.Errorf("KeyShown = %v, warnings %q", res.KeyShown, res.Warnings)
		}
	})
}

func TestApplyCreateKeyRace(t *testing.T) {
	t.Parallel()
	r := freshRig(Linux)
	in := r.input(t, request(), nil, testSecret)
	r.h.files.FailWrite(r.l.VaultKey, fs.ErrExist)
	_, err := r.apply(in)
	wantRefusal(t, err, "another bootstrap")
	j := r.journal.Entries()
	for _, later := range []string{"write " + r.l.Config, "write " + r.l.Service, "service activate true"} {
		if slices.Contains(j, later) {
			t.Errorf("ran %q after the key race", later)
		}
	}
	if len(r.daemon.Calls()) != 0 {
		t.Error("ran the daemon after the key race")
	}
	if r.h.files.Locked(r.l.Lock) {
		t.Error("the lock is still held")
	}
}

func TestApplyTestFailure(t *testing.T) {
	t.Parallel()

	t.Run("rejected", func(t *testing.T) {
		t.Parallel()
		r := freshRig(Linux)
		r.daemon.TestExit = 5
		res, err := r.apply(r.input(t, request(), nil, testSecret))
		var ae *AuthError
		if !errors.As(err, &ae) {
			t.Fatalf("Apply = %v, want an *AuthError", err)
		}
		if ae.Hint != "run bootstrap again and enter the new password" || !strings.Contains(ae.Msg, "alice") {
			t.Errorf("AuthError = %+v", ae)
		}
		if res.Test != "failed" || !res.ServiceRunning || res.Account != "alice" {
			t.Errorf("result = %+v", res)
		}
		if _, _, ok := r.h.files.Content(r.l.Service); !ok {
			t.Error("the install did not complete")
		}
	})

	t.Run("server unavailable", func(t *testing.T) {
		t.Parallel()
		r := freshRig(Linux)
		r.daemon.TestExit = 7
		res, err := r.apply(r.input(t, request(), nil, testSecret))
		if err != nil {
			t.Fatalf("Apply = %v, want no error for a test that could not reach the server", err)
		}
		if res.Test != "failed" || len(res.Warnings) == 0 {
			t.Errorf("result = %+v", res)
		}
	})
}

func TestApplyOAuthLogin(t *testing.T) {
	t.Parallel()

	t.Run("with a terminal", func(t *testing.T) {
		t.Parallel()
		r := freshRig(Linux)
		req := workspaceRequest(true)
		req.ExecSHA256 = sha256Hex(newBinary)
		res, err := r.apply(r.input(t, req, nil, []byte("client-secret")))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		var login *setuptest.DaemonCall
		calls := r.daemon.Calls()
		for i, c := range calls {
			if len(c.Args) >= 3 && c.Args[0] == "auth" && c.Args[1] == "login" {
				login = &calls[i]
			}
		}
		if login == nil {
			t.Fatalf("no auth login in %q", r.daemonCalls())
		}
		if !slices.Equal(login.Args, []string{"auth", "login", "alice", "--socket", r.l.Socket}) {
			t.Errorf("auth login args = %q", login.Args)
		}
		if login.In != io.Reader(r.tty) || login.Out != io.Writer(r.tty) {
			t.Error("auth login does not run on the terminal")
		}
		if !r.tty.Closed() {
			t.Error("the terminal was not closed")
		}
		if v, _ := r.daemon.Secret("oauth_clients/google"); string(v) != "client-secret" {
			t.Errorf("client secret (%d bytes) differs from the one given", len(v))
		}
		j := r.journal.Entries()
		store, login2, test := slices.Index(j, "daemon secret set oauth_clients/google"), slices.Index(j, "daemon auth login alice"), slices.Index(j, "daemon accounts test alice")
		if store < 0 || login2 < store || test < login2 {
			t.Errorf("journal %q: want secret, login, test in order", j)
		}
		if res.Test != "ok" {
			t.Errorf("Test = %q", res.Test)
		}
	})

	t.Run("without a terminal", func(t *testing.T) {
		t.Parallel()
		r := freshRig(Linux)
		req := workspaceRequest(false)
		req.ExecSHA256 = sha256Hex(newBinary)
		res, err := r.apply(r.input(t, req, nil, []byte("client-secret")))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if r.ttyOpens != 0 {
			t.Error("opened the terminal without one")
		}
		if res.Test != "skipped" || res.NextSteps[len(res.NextSteps)-1] != "chainmail auth login alice" {
			t.Errorf("result = %+v", res)
		}
	})
}

func TestApplyTrustsConfigDirBeforeWriting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		p      Platform
		tamper func(f *setuptest.Files)
		want   string
	}{
		{
			name: "symlinked config directory",
			p:    Linux,
			tamper: func(f *setuptest.Files) {
				f.Dir("/tmp", 0, 0, 0o755).Symlink("/etc/chainmail", 0, "/tmp")
			},
			want: "/etc/chainmail: is a symlink",
		},
		{
			name: "config directory owned by a user",
			p:    Linux,
			tamper: func(f *setuptest.Files) {
				f.Dir("/etc/chainmail", setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755)
			},
			want: "/etc/chainmail: owned by uid 1000",
		},
		{
			name:   "ancestor owned by a user",
			p:      Linux,
			tamper: func(f *setuptest.Files) { f.Dir("/etc", setuptest.AliceLinuxUID, 0, 0o755) },
			want:   "/etc: owned by uid 1000",
		},
		{
			name: "ancestor owned by a user on macOS",
			p:    Darwin,
			tamper: func(f *setuptest.Files) {
				f.Dir("/private/etc", setuptest.AliceDarwinUID, 0, 0o755)
			},
			want: "/private/etc: owned by uid 501",
		},
		{
			name: "config directory writable by others",
			p:    Linux,
			tamper: func(f *setuptest.Files) {
				f.Dir("/etc/chainmail", 0, 0, 0o777)
			},
			want: "/etc/chainmail: writable by group or others",
		},
		{
			name: "config directory is a file",
			p:    Linux,
			tamper: func(f *setuptest.Files) {
				f.File("/etc/chainmail", 0, 0, 0o644, []byte("x"))
			},
			want: "/etc/chainmail: is not a directory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := freshRig(tc.p)
			in := r.input(t, request(), nil, testSecret)
			tc.tamper(r.h.files)
			_, err := r.apply(in)
			wantRefusal(t, err, tc.want)
			if j := r.journal.Entries(); len(j) != 0 {
				t.Errorf("journal after a refused config directory: %q", j)
			}
			if w := r.h.files.Writes(); len(w) != 0 {
				t.Errorf("writes after a refused config directory: %q", w)
			}
			for _, lock := range []string{r.l.Lock, "/tmp/.bootstrap.lock"} {
				if _, _, ok := r.h.files.Content(lock); ok {
					t.Errorf("lock file %s was created", lock)
				}
			}
		})
	}
}

func TestEnsureAncestorsChecksTrust(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		tamper func(f *setuptest.Files)
		want   string
	}{
		{
			name:   "existing ancestor owned by a user",
			tamper: func(f *setuptest.Files) { f.Remove("/var/lib").Dir("/var", setuptest.AliceLinuxUID, 0, 0o755) },
			want:   "/var: owned by uid 1000",
		},
		{
			name: "symlinked ancestor owned by a user",
			tamper: func(f *setuptest.Files) {
				f.Remove("/var/lib").Dir("/opt", 0, 0, 0o755).Symlink("/var/lib", setuptest.AliceLinuxUID, "/opt")
			},
			want: "/var/lib: is a symlink owned by uid 1000",
		},
		{
			name: "symlinked ancestor into a user's directory",
			tamper: func(f *setuptest.Files) {
				f.Remove("/var/lib").Symlink("/var/lib", 0, "/home/alice")
			},
			want: "/home/alice: owned by uid 1000",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := freshRig(Linux)
			r.h.files.Dir("/home", 0, 0, 0o755).Dir("/home/alice", setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755)
			tc.tamper(r.h.files)
			err := r.applier.ensureAncestors(r.l.StateDir + "/sub")
			wantRefusal(t, err, tc.want)
			if j := r.journal.Entries(); len(j) != 0 {
				t.Errorf("journal after a refused ancestor: %q", j)
			}
		})
	}
}

func TestApplyReadinessBoundsHungStatus(t *testing.T) {
	t.Parallel()
	r := freshRig(Linux)
	r.applier.readyTimeout = 50 * time.Millisecond
	for i := 1; i <= 30; i++ {
		r.svc.Log = append(r.svc.Log, fmt.Sprintf("log line %02d", i))
	}
	run := r.daemon.Run
	r.applier.AsDaemon = func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
		if len(args) >= 2 && args[0] == "admin" && args[1] == "status" {
			<-ctx.Done() // a daemon that accepts the connection and never answers
			return ctx.Err()
		}
		return run(ctx, stdin, stdout, args...)
	}
	in := r.input(t, request(), nil, testSecret)

	done := make(chan error, 1)
	go func() {
		_, err := r.apply(in)
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Apply hung on an admin status that never returns")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Apply = %v, want ErrNotReady", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "log line 11") || !strings.Contains(msg, "log line 30") || strings.Contains(msg, "log line 10") {
		t.Errorf("error does not carry the last 20 log lines:\n%s", msg)
	}
	if r.h.files.Locked(r.l.Lock) {
		t.Error("the lock is still held")
	}
}

func TestApplyReadinessHonoursCancel(t *testing.T) {
	t.Parallel()
	r := freshRig(Linux)
	r.svc.Stuck = true
	ctx, cancel := context.WithCancel(context.Background())
	run := r.daemon.Run
	r.applier.AsDaemon = func(c context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
		cancel() // the caller gives up while the daemon is starting
		return run(c, stdin, stdout, args...)
	}
	_, err := r.applier.Apply(ctx, r.input(t, request(), nil, testSecret))
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotReady) {
		t.Fatalf("Apply = %v, want context.Canceled, not ErrNotReady", err)
	}
}

func TestApplyRestartRules(t *testing.T) {
	t.Parallel()
	// oldService is a marked service definition other than the one this
	// binary renders, as an older bootstrap would have written.
	oldService := func(r *rig) {
		r.h.files.File(r.l.Service, 0, 0, 0o644, append(RenderService(r.l), "\n"...))
	}
	sameBinary := func(r *rig) { r.h.files.File(userExe, 1000, 1000, 0o755, fakeBinary) }
	cases := []struct {
		name       string
		p          Platform
		prepare    func(r *rig)
		newBinary  bool   // run newBinary rather than the installed one
		secret     []byte // a new config holds a new account, which needs one
		wantCalls  []string
		wantStarts int
	}{
		{
			name:       "macOS plist rewritten",
			p:          Darwin,
			prepare:    func(r *rig) { sameBinary(r); oldService(r) },
			wantCalls:  []string{"state", "activate true", "state"},
			wantStarts: 1,
		},
		{
			name:       "macOS config only",
			p:          Darwin,
			prepare:    func(r *rig) { sameBinary(r); r.h.files.Remove(r.l.Config) },
			secret:     testSecret,
			wantCalls:  []string{"state", "activate false", "restart", "state"},
			wantStarts: 1,
		},
		{
			name:       "macOS binary only",
			p:          Darwin,
			prepare:    func(*rig) {},
			newBinary:  true,
			wantCalls:  []string{"state", "activate false", "restart", "state"},
			wantStarts: 1,
		},
		{
			name:       "Linux unit rewritten",
			p:          Linux,
			prepare:    func(r *rig) { sameBinary(r); oldService(r) },
			wantCalls:  []string{"state", "activate true", "restart", "state"},
			wantStarts: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := installedRig(tc.p)
			tc.prepare(r)
			req := gmailRequest()
			if tc.newBinary {
				req = request()
			}
			in := r.input(t, req, okHealth, tc.secret)
			if !in.Plan.ServiceChanged {
				t.Fatal("fixture plan does not change the service")
			}
			if _, err := r.apply(in); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := r.svc.Calls(); !slices.Equal(got, tc.wantCalls) {
				t.Errorf("service calls = %q, want %q", got, tc.wantCalls)
			}
			if got := r.svc.Starts(); got != tc.wantStarts {
				t.Errorf("starts = %d, want %d", got, tc.wantStarts)
			}
		})
	}
}

// hungLogs is a service whose log reader blocks until its context is
// done, as journalctl would on a wedged journal.
type hungLogs struct{ *setuptest.Service }

func (hungLogs) Logs(ctx context.Context, _ int) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// applyWithin runs Apply in the background and fails the test when it
// does not return within 5 seconds of real time.
func applyWithin(t *testing.T, r *rig, in Input, what string) (Result, error) {
	t.Helper()
	type out struct {
		res Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := r.apply(in)
		done <- out{res, err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-time.After(5 * time.Second):
		t.Fatalf("Apply hung on %s", what)
		return Result{}, nil
	}
}

func TestApplyReadinessBoundsHungLogs(t *testing.T) {
	t.Parallel()
	r := freshRig(Linux)
	r.svc.Stuck = true
	r.applier.readyTimeout = 50 * time.Millisecond
	r.applier.logTimeout = 50 * time.Millisecond
	r.applier.Service = hungLogs{r.svc}
	_, err := applyWithin(t, r, r.input(t, request(), nil, testSecret), "a log reader that never returns")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Apply = %v, want ErrNotReady", err)
	}
	if !strings.Contains(err.Error(), "could not read its log") || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %q does not say the log could not be read", err)
	}
	if r.h.files.Locked(r.l.Lock) {
		t.Error("the lock is still held")
	}
}

// blockDaemon makes the rig's daemon-user runs of the command starting
// with words block until their context is done, as a daemon that accepts
// the connection and never answers would. It reports whether a blocked
// call's context had a deadline.
func blockDaemon(r *rig, words ...string) *bool {
	var hadDeadline bool
	run := r.daemon.Run
	r.applier.AsDaemon = func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
		if len(args) >= len(words) && slices.Equal(args[:len(words)], words) {
			_, hadDeadline = ctx.Deadline()
			<-ctx.Done()
			return ctx.Err()
		}
		return run(ctx, stdin, stdout, args...)
	}
	return &hadDeadline
}

func TestApplyBoundsDaemonCalls(t *testing.T) {
	t.Parallel()
	const short = 50 * time.Millisecond
	cases := []struct {
		name  string
		rig   func() *rig
		block []string
		// secret is the secret given; the installed rig plans a new binary
		// with none, so the rejected-credentials check runs.
		secret []byte
	}{
		{name: "accounts list before a restart", rig: func() *rig { return installedRig(Linux) }, block: []string{"accounts", "list"}},
		{name: "admin status before a restart", rig: func() *rig { return installedRig(Linux) }, block: []string{"admin", "status"}},
		{name: "secret set", rig: func() *rig { return freshRig(Linux) }, block: []string{"secret", "set"}, secret: testSecret},
		{name: "accounts test", rig: func() *rig { return freshRig(Linux) }, block: []string{"accounts", "test"}, secret: testSecret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := tc.rig()
			r.applier.callTimeout = short
			r.applier.secretTimeout = short
			r.applier.testTimeout = short
			r.applier.readyTimeout = time.Second
			hadDeadline := blockDaemon(r, tc.block...)
			in := r.input(t, request(), okHealth, tc.secret)
			_, err := applyWithin(t, r, in, strings.Join(tc.block, " "))
			if !*hadDeadline {
				t.Errorf("%s ran without a deadline", strings.Join(tc.block, " "))
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Apply = %v, want a deadline error", err)
			}
			if errors.Is(err, ErrNotReady) {
				t.Errorf("Apply = %v, want a step error, not ErrNotReady", err)
			}
			if want := strings.Join(tc.block, " ") + " did not answer within"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name the step (%q)", err, want)
			}
			if r.h.files.Locked(r.l.Lock) {
				t.Error("the lock is still held")
			}
		})
	}
}

func TestApplyLeavesAuthLoginUnbounded(t *testing.T) {
	t.Parallel()
	r := freshRig(Linux)
	r.applier.callTimeout = 50 * time.Millisecond
	r.applier.testTimeout = 50 * time.Millisecond
	var loginDeadline, sawLogin bool
	run := r.daemon.Run
	r.applier.AsDaemon = func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
		if len(args) >= 2 && args[0] == "auth" && args[1] == "login" {
			sawLogin = true
			_, loginDeadline = ctx.Deadline()
		}
		return run(ctx, stdin, stdout, args...)
	}
	req := workspaceRequest(true)
	req.ExecSHA256 = sha256Hex(newBinary)
	if _, err := r.apply(r.input(t, req, nil, []byte("client-secret"))); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !sawLogin || loginDeadline {
		t.Errorf("auth login ran %v, with a deadline %v; want it run with none", sawLogin, loginDeadline)
	}
}

func TestApplyDaemonTimeouts(t *testing.T) {
	t.Parallel()
	a := &Applier{}
	if got := a.logWait(); got != 5*time.Second {
		t.Errorf("log timeout = %v, want 5s", got)
	}
	if got := a.callWait(); got != 15*time.Second {
		t.Errorf("daemon call timeout = %v, want 15s", got)
	}
	if got := a.secretWait(); got != 150*time.Second {
		t.Errorf("secret set timeout = %v, want 150s", got)
	}
	if got := a.testWait(); got != 60*time.Second {
		t.Errorf("account test timeout = %v, want 60s", got)
	}
}

// slowSecretSet makes the rig's secret set take probe, as the real
// command does when it stores a password and then probes the account's
// IMAP connection limit. It does not wait in real time: a call whose
// deadline leaves less than probe is killed at once, as the deadline
// would kill it, and a call with enough time runs the fake daemon. It
// reports the time each secret set had left.
func slowSecretSet(r *rig, probe time.Duration) *[]time.Duration {
	var left []time.Duration
	run := r.daemon.Run
	r.applier.AsDaemon = func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
		if len(args) >= 2 && args[0] == "secret" && args[1] == "set" {
			deadline, ok := ctx.Deadline()
			remaining := time.Until(deadline)
			left = append(left, remaining)
			if ok && remaining < probe {
				return fmt.Errorf("secret set killed after %v of %v: %w", remaining.Round(time.Second), probe, context.DeadlineExceeded)
			}
		}
		return run(ctx, stdin, stdout, args...)
	}
	return &left
}

func TestApplySecretSetCoversTheProbe(t *testing.T) {
	t.Parallel()
	// Longer than the 15 s other daemon calls get, shorter than the 150 s
	// secret set gets.
	const probe = 20 * time.Second
	r := freshRig(Linux)
	left := slowSecretSet(r, probe)
	res, err := r.apply(r.input(t, request(), nil, testSecret))
	if err != nil {
		t.Fatalf("Apply with a secret set taking %v: %v", probe, err)
	}
	if len(*left) != 1 {
		t.Fatalf("secret set ran %d times, want once", len(*left))
	}
	if got := (*left)[0]; got <= probe || got > 150*time.Second {
		t.Errorf("secret set had %v left, want more than %v and at most 150s", got, probe)
	}
	if res.Test != testOK {
		t.Errorf("Test = %q, want %q", res.Test, testOK)
	}
	if got, ok := r.daemon.Secret("accounts/alice"); !ok || !bytes.Equal(got, testSecret) {
		t.Errorf("stored secret present %v, length %d; want the given secret, length %d", ok, len(got), len(testSecret))
	}
}

func TestApplyRepairsFileMetadata(t *testing.T) {
	t.Parallel()
	for _, p := range []Platform{Linux, Darwin} {
		for _, tc := range metadataCases() {
			t.Run(string(p)+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				r := installedRig(p)
				r.h.files.File(userExe, 1000, 1000, 0o755, fakeBinary) // the installed binary
				e, _ := r.l.Entry(tc.entry)
				before, _, _ := r.h.files.Content(e.Path)
				daemon, _ := r.h.accounts.User(r.l.DaemonUser)
				tc.tamper(r.h, r.l, daemon.UID)

				in := r.input(t, gmailRequest(), okHealth, nil)
				if _, err := r.apply(in); err != nil {
					t.Fatalf("Apply: %v", err)
				}
				data, info, ok := r.h.files.Content(e.Path)
				if !ok || !bytes.Equal(data, before) {
					t.Errorf("%s holds %d bytes (present %v), want the %d bytes it had", e.Path, len(data), ok, len(before))
				}
				want := system.FileInfo{Exists: true, Mode: e.Mode, UID: 0, GID: 0}
				if info != want {
					t.Errorf("%s metadata = uid %d gid %d mode %v, want uid 0 gid 0 mode %v", e.Path, info.UID, info.GID, info.Mode, e.Mode)
				}
			})
		}
	}
}
