package setuptest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/setup/system"
)

func TestFilesFollowAncestorLinks(t *testing.T) {
	t.Parallel()
	h := FreshHost("darwin")
	if err := h.Files.EnsureDir("/etc/chainmail", 0, 0, 0o755); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if got := h.Files.Writes(); !slices.Equal(got, []string{"/private/etc/chainmail"}) {
		t.Errorf("Writes = %q, want the resolved path", got)
	}
	info, err := h.Files.Lstat("/etc")
	if err != nil || info.Mode&fs.ModeSymlink == 0 {
		t.Errorf("Lstat(/etc) = %+v, %v; want the link itself", info, err)
	}
	if err := h.Files.EnsureDir("/var/lib/chainmail", 0, 0, 0o700); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("EnsureDir without a parent = %v, want fs.ErrNotExist", err)
	}
}

func TestFilesLockAndFailures(t *testing.T) {
	t.Parallel()
	j := &Journal{}
	f := FreshHost("linux").Files
	f.Journal = j
	if _, err := f.Lock("/etc/chainmail/.bootstrap.lock"); err == nil {
		t.Error("Lock without the parent directory succeeded")
	}
	unlock, err := f.Lock("/etc/.lock")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, err := f.Lock("/etc/.lock"); err == nil {
		t.Error("second Lock succeeded")
	}
	if err := unlock(); err != nil || f.Locked("/etc/.lock") {
		t.Errorf("unlock = %v, Locked = %v", err, f.Locked("/etc/.lock"))
	}
	f.FailWrite("/etc/key", fs.ErrExist)
	if err := f.CreateExclusive("/etc/key", nil, 0, 0, 0o400); !errors.Is(err, fs.ErrExist) {
		t.Errorf("CreateExclusive = %v, want fs.ErrExist", err)
	}
	if got, want := j.Entries(), []string{"lock /etc/.lock", "unlock /etc/.lock"}; !slices.Equal(got, want) {
		t.Errorf("journal = %q, want %q", got, want)
	}
}

func TestAccountsCreateDaemonUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, goos := range []string{"linux", "darwin"} {
		a := NewAccounts(goos)
		g, err := a.CreateGroup(ctx, "chainmail-clients")
		if err != nil {
			t.Fatalf("%s: CreateGroup: %v", goos, err)
		}
		u, err := a.CreateDaemonUser(ctx, "chainmail", "chainmail-clients", "/var/lib/chainmail")
		if err != nil {
			t.Fatalf("%s: CreateDaemonUser: %v", goos, err)
		}
		own, hasOwn := a.Group("chainmail")
		switch goos {
		case "linux":
			if !hasOwn || own.GID != u.GID || !slices.Contains(u.Groups, "chainmail-clients") {
				t.Errorf("linux user %+v, own group %+v %v", u, own, hasOwn)
			}
		case "darwin":
			if hasOwn || u.GID != g.GID {
				t.Errorf("darwin user %+v, want primary group %d and no group of its own", u, g.GID)
			}
		}
	}
}

func TestDaemonFollowsService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := NewClock(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	svc := &Service{Clock: clock}
	d := &Daemon{Service: svc, Socket: "/run/chainmail/chainmail.sock"}
	status := func() (time.Time, error) {
		var out bytes.Buffer
		if err := d.Run(ctx, nil, &out, "admin", "status", "--output", "json", "--socket", d.Socket); err != nil {
			return time.Time{}, err
		}
		var resp struct {
			StartedAt time.Time `json:"started_at"`
		}
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
		return resp.StartedAt, nil
	}

	var exit interface{ ExitCode() int }
	if _, err := status(); !errors.As(err, &exit) || exit.ExitCode() != exitUnavailable {
		t.Errorf("status of a stopped daemon = %v, want exit %d", err, exitUnavailable)
	}
	before := clock.Now()
	if err := svc.Activate(ctx, true); err != nil {
		t.Fatal(err)
	}
	got, err := status()
	if err != nil || !got.After(before) {
		t.Errorf("status after start = %v, %v; want a start after %v", got, err, before)
	}
	if err := d.Run(ctx, nil, nil, "admin", "status"); err == nil {
		t.Error("a call without --socket succeeded")
	}
	if err := d.Run(ctx, strings.NewReader("pw\n"), nil, "secret", "set", "accounts/alice", "--socket", d.Socket); err != nil {
		t.Fatalf("secret set: %v", err)
	}
	if v, ok := d.Secret("accounts/alice"); !ok || string(v) != "pw" {
		t.Errorf("stored secret (%d bytes, found %v) differs from the 2 bytes set", len(v), ok)
	}
	d.TestExit = exitAuth
	if err := d.Run(ctx, nil, nil, "accounts", "test", "alice", "--socket", d.Socket); !errors.As(err, &exit) || exit.ExitCode() != exitAuth {
		t.Errorf("accounts test = %v, want exit %d", err, exitAuth)
	}
	if st, _ := svc.State(ctx); st != (system.ServiceState{Loaded: true, Running: true}) {
		t.Errorf("State = %+v", st)
	}
}

func TestServiceStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	running := system.ServiceState{Loaded: true, Running: true}

	t.Run("systemd never restarts on activate", func(t *testing.T) {
		t.Parallel()
		svc := (&Service{}).SetState(running)
		if err := svc.Activate(ctx, true); err != nil {
			t.Fatal(err)
		}
		if n := svc.Starts(); n != 0 {
			t.Errorf("Starts after Activate(true) on a running unit = %d, want 0", n)
		}
		if err := svc.Restart(ctx); err != nil {
			t.Fatal(err)
		}
		if n := svc.Starts(); n != 1 {
			t.Errorf("Starts after Restart = %d, want 1", n)
		}
	})

	t.Run("launchd reloads a loaded job on a changed plist", func(t *testing.T) {
		t.Parallel()
		svc := (&Service{Launchd: true}).SetState(running)
		if err := svc.Activate(ctx, false); err != nil {
			t.Fatal(err)
		}
		if n := svc.Starts(); n != 0 {
			t.Errorf("Starts after Activate(false) on a running job = %d, want 0", n)
		}
		if err := svc.Activate(ctx, true); err != nil {
			t.Fatal(err)
		}
		if n := svc.Starts(); n != 1 {
			t.Errorf("Starts after Activate(true) on a loaded job = %d, want 1", n)
		}
	})
}
