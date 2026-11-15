package system

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"
)

const (
	unit       = "chainmail.service"
	unitPath   = "/etc/systemd/system/chainmail.service"
	label      = "sh.chainmail.daemon"
	plistPath  = "/Library/LaunchDaemons/sh.chainmail.daemon.plist"
	daemonLog  = "/var/log/chainmail/daemon.log"
	showState  = "systemctl show --property=LoadState,ActiveState --value chainmail.service"
	printLabel = "launchctl print system/sh.chainmail.daemon"
)

func TestServiceSystemd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	newSvc := func(r *fakeRunner) Service {
		return NewService("linux", unit, unitPath, "", r.run, nil)
	}

	stateTests := []struct {
		name string
		out  string
		want ServiceState
	}{
		{"running", "loaded\nactive\n", ServiceState{Loaded: true, Running: true}},
		{"loaded and stopped", "loaded\ninactive\n", ServiceState{Loaded: true}},
		{"failed", "loaded\nfailed\n", ServiceState{Loaded: true}},
		{"missing", "not-found\ninactive\n", ServiceState{}},
	}
	for _, tt := range stateTests {
		t.Run("State "+tt.name, func(t *testing.T) {
			t.Parallel()
			r := newFakeRunner().on(showState, tt.out, nil)
			got, err := newSvc(r).State(ctx)
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != tt.want {
				t.Fatalf("State = %+v, want %+v", got, tt.want)
			}
			r.wantCalls(t, showState)
		})
	}

	t.Run("State malformed", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(showState, "loaded\n", nil)
		if _, err := newSvc(r).State(ctx); err == nil {
			t.Fatal("State accepted one line of output")
		}
	})

	t.Run("State failure", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(showState, "", &fakeExit{code: 1, stderr: "no bus"})
		if _, err := newSvc(r).State(ctx); err == nil {
			t.Fatal("State succeeded when systemctl failed")
		}
	})

	t.Run("Activate changed", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner()
		if err := newSvc(r).Activate(ctx, true); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		r.wantCalls(t, "systemctl daemon-reload", "systemctl enable --now chainmail.service")
	})

	t.Run("Activate unchanged", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner()
		if err := newSvc(r).Activate(ctx, false); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		r.wantCalls(t, "systemctl enable --now chainmail.service")
	})

	t.Run("Activate reload failure", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on("systemctl daemon-reload", "", &fakeExit{code: 1})
		if err := newSvc(r).Activate(ctx, true); err == nil {
			t.Fatal("Activate succeeded when daemon-reload failed")
		}
		r.wantCalls(t, "systemctl daemon-reload")
	})

	t.Run("Restart", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner()
		if err := newSvc(r).Restart(ctx); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		r.wantCalls(t, "systemctl restart chainmail.service")
	})

	t.Run("Logs", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on("journalctl --unit chainmail.service --lines 20 --no-pager --output cat", "a\nb\n", nil)
		out, err := newSvc(r).Logs(ctx, 20)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		if string(out) != "a\nb\n" {
			t.Fatalf("Logs = %q", out)
		}
		r.wantCalls(t, "journalctl --unit chainmail.service --lines 20 --no-pager --output cat")
	})
}

// logFiles is a fake Files that serves one log file.
type logFiles struct {
	Files
	path string
	data []byte
	err  error
}

func (f *logFiles) ReadFile(path string) ([]byte, error) {
	if path != f.path {
		return nil, fmt.Errorf("unexpected read of %s", path)
	}
	return f.data, f.err
}

const launchdRunning = `system/sh.chainmail.daemon = {
	active count = 1
	path = /Library/LaunchDaemons/sh.chainmail.daemon.plist
	state = running
}
`

const launchdWaiting = `system/sh.chainmail.daemon = {
	active count = 0
	path = /Library/LaunchDaemons/sh.chainmail.daemon.plist
	state = not running
}
`

var notLoaded = &fakeExit{code: 113, stderr: `Could not find service "sh.chainmail.daemon" in domain for system`}

func TestServiceLaunchd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	newSvc := func(r *fakeRunner, files Files) Service {
		s := NewService("darwin", label, plistPath, daemonLog, r.run, files)
		s.(*launchdService).sleep = func(time.Duration) {}
		return s
	}

	stateTests := []struct {
		name string
		out  string
		err  error
		want ServiceState
	}{
		{"running", launchdRunning, nil, ServiceState{Loaded: true, Running: true}},
		{"loaded and waiting", launchdWaiting, nil, ServiceState{Loaded: true}},
		{"not loaded", "", notLoaded, ServiceState{}},
	}
	for _, tt := range stateTests {
		t.Run("State "+tt.name, func(t *testing.T) {
			t.Parallel()
			r := newFakeRunner().on(printLabel, tt.out, tt.err)
			got, err := newSvc(r, nil).State(ctx)
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != tt.want {
				t.Fatalf("State = %+v, want %+v", got, tt.want)
			}
			r.wantCalls(t, printLabel)
		})
	}

	t.Run("Activate not loaded", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(printLabel, "", notLoaded)
		if err := newSvc(r, nil).Activate(ctx, true); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		r.wantCalls(t, printLabel, "launchctl bootstrap system "+plistPath)
	})

	t.Run("Activate changed while loaded", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().
			on(printLabel, launchdRunning, nil).
			on(printLabel, launchdRunning, nil). // still unloading
			on(printLabel, "", notLoaded)
		if err := newSvc(r, nil).Activate(ctx, true); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		r.wantCalls(t,
			printLabel,
			"launchctl bootout system/sh.chainmail.daemon",
			printLabel,
			printLabel,
			"launchctl bootstrap system "+plistPath,
		)
	})

	t.Run("Activate changed but never unloads", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(printLabel, launchdRunning, nil)
		err := newSvc(r, nil).Activate(ctx, true)
		if err == nil {
			t.Fatal("Activate succeeded while the old service stayed loaded")
		}
		for _, c := range r.calls {
			if strings.HasPrefix(c, "launchctl bootstrap") {
				t.Fatalf("ran %q after bootout did not finish", c)
			}
		}
	})

	t.Run("Activate unchanged and stopped", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(printLabel, launchdWaiting, nil)
		if err := newSvc(r, nil).Activate(ctx, false); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		r.wantCalls(t, printLabel, "launchctl kickstart system/sh.chainmail.daemon")
	})

	t.Run("Activate unchanged and running", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(printLabel, launchdRunning, nil)
		if err := newSvc(r, nil).Activate(ctx, false); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		r.wantCalls(t, printLabel)
	})

	t.Run("Restart", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner()
		if err := newSvc(r, nil).Restart(ctx); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		r.wantCalls(t, "launchctl kickstart -k system/sh.chainmail.daemon")
	})

	t.Run("Logs", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		for i := 1; i <= 30; i++ {
			fmt.Fprintf(&b, "line %d\n", i)
		}
		r := newFakeRunner()
		out, err := newSvc(r, &logFiles{path: daemonLog, data: []byte(b.String())}).Logs(ctx, 20)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		if len(lines) != 20 || lines[0] != "line 11" || lines[19] != "line 30" {
			t.Fatalf("Logs returned %d lines from %q to %q", len(lines), lines[0], lines[len(lines)-1])
		}
		r.wantCalls(t)
	})

	t.Run("Logs short file", func(t *testing.T) {
		t.Parallel()
		out, err := newSvc(newFakeRunner(), &logFiles{path: daemonLog, data: []byte("one\ntwo")}).Logs(ctx, 20)
		if err != nil || string(out) != "one\ntwo" {
			t.Fatalf("Logs = %q, %v", out, err)
		}
	})

	t.Run("Logs missing file", func(t *testing.T) {
		t.Parallel()
		out, err := newSvc(newFakeRunner(), &logFiles{path: daemonLog, err: fs.ErrNotExist}).Logs(ctx, 20)
		if err != nil || len(out) != 0 {
			t.Fatalf("Logs = %q, %v, want empty", out, err)
		}
	})

	t.Run("Logs read failure", func(t *testing.T) {
		t.Parallel()
		_, err := newSvc(newFakeRunner(), &logFiles{path: daemonLog, err: errors.New("denied")}).Logs(ctx, 20)
		if err == nil {
			t.Fatal("Logs succeeded when the read failed")
		}
	})
}

func TestTail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"", 3, ""},
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\nb\n", 5, "a\nb\n"},
		{"a\nb\n", 0, ""},
	}
	for _, tt := range tests {
		if got := string(tail([]byte(tt.in), tt.n)); got != tt.want {
			t.Errorf("tail(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}
