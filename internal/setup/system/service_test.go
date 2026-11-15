package system

import (
	"context"
	"os/exec"
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
		return NewService("linux", unit, unitPath, "", r.run)
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
		r := newFakeRunner().onFail(showState, "", 1, "no bus")
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
		r := newFakeRunner().onFail("systemctl daemon-reload", "", 1, "")
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

// notLoadedStderr is what launchctl print says, exiting 113, about a job
// launchd does not know.
const notLoadedStderr = `Could not find service "sh.chainmail.daemon" in domain for system`

func TestServiceLaunchd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	newSvc := func(r *fakeRunner) Service {
		s := NewService("darwin", label, plistPath, daemonLog, r.run)
		s.(*launchdService).sleep = func(time.Duration) {}
		return s
	}
	notLoaded := func(r *fakeRunner) *fakeRunner { return r.onFail(printLabel, "", 113, notLoadedStderr) }

	stateTests := []struct {
		name  string
		reply func(*fakeRunner) *fakeRunner
		want  ServiceState
	}{
		{"running", func(r *fakeRunner) *fakeRunner { return r.on(printLabel, launchdRunning, nil) }, ServiceState{Loaded: true, Running: true}},
		{"loaded and waiting", func(r *fakeRunner) *fakeRunner { return r.on(printLabel, launchdWaiting, nil) }, ServiceState{Loaded: true}},
		{"not loaded", notLoaded, ServiceState{}},
		{"not loaded exit status only", func(r *fakeRunner) *fakeRunner { return r.onFail(printLabel, "", 113, "") }, ServiceState{}},
		{"not loaded message only", func(r *fakeRunner) *fakeRunner { return r.onFail(printLabel, "", 1, notLoadedStderr) }, ServiceState{}},
	}
	for _, tt := range stateTests {
		t.Run("State "+tt.name, func(t *testing.T) {
			t.Parallel()
			r := tt.reply(newFakeRunner())
			got, err := newSvc(r).State(ctx)
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != tt.want {
				t.Fatalf("State = %+v, want %+v", got, tt.want)
			}
			r.wantCalls(t, printLabel)
		})
	}

	// Only launchd saying it does not know the job means not loaded.
	// Anything else is a failure to find out.
	failures := []struct {
		name  string
		reply func(*fakeRunner) *fakeRunner
	}{
		{"other failure", func(r *fakeRunner) *fakeRunner {
			return r.onFail(printLabel, "", 1, "Operation not permitted while System Integrity Protection is engaged")
		}},
		{"cancelled before start", func(r *fakeRunner) *fakeRunner { return r.on(printLabel, "", context.Canceled) }},
		{"killed on cancel", func(r *fakeRunner) *fakeRunner { return r.onFail(printLabel, "", -1, "") }},
		{"missing launchctl", func(r *fakeRunner) *fakeRunner {
			return r.on(printLabel, "", &exec.Error{Name: "launchctl", Err: exec.ErrNotFound})
		}},
	}
	for _, tt := range failures {
		t.Run("State "+tt.name, func(t *testing.T) {
			t.Parallel()
			r := tt.reply(newFakeRunner())
			if got, err := newSvc(r).State(ctx); err == nil {
				t.Fatalf("State = %+v, want an error", got)
			}
		})
		t.Run("Activate "+tt.name, func(t *testing.T) {
			t.Parallel()
			r := tt.reply(newFakeRunner())
			if err := newSvc(r).Activate(ctx, true); err == nil {
				t.Fatal("Activate succeeded when launchctl print failed")
			}
			r.wantCalls(t, printLabel)
		})
		t.Run("Activate waiting for bootout "+tt.name, func(t *testing.T) {
			t.Parallel()
			r := tt.reply(newFakeRunner().on(printLabel, launchdRunning, nil))
			if err := newSvc(r).Activate(ctx, true); err == nil {
				t.Fatal("Activate succeeded when launchctl print failed after bootout")
			}
			r.wantCalls(t, printLabel, "launchctl bootout system/sh.chainmail.daemon", printLabel)
		})
	}

	t.Run("Activate not loaded", func(t *testing.T) {
		t.Parallel()
		r := notLoaded(newFakeRunner())
		if err := newSvc(r).Activate(ctx, true); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		r.wantCalls(t, printLabel, "launchctl bootstrap system "+plistPath)
	})

	t.Run("Activate changed while loaded", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().
			on(printLabel, launchdRunning, nil).
			on(printLabel, launchdRunning, nil). // still unloading
			onFail(printLabel, "", 113, notLoadedStderr)
		if err := newSvc(r).Activate(ctx, true); err != nil {
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
		err := newSvc(r).Activate(ctx, true)
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
		if err := newSvc(r).Activate(ctx, false); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		r.wantCalls(t, printLabel, "launchctl kickstart system/sh.chainmail.daemon")
	})

	t.Run("Activate unchanged and running", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner().on(printLabel, launchdRunning, nil)
		if err := newSvc(r).Activate(ctx, false); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		r.wantCalls(t, printLabel)
	})

	t.Run("Restart", func(t *testing.T) {
		t.Parallel()
		r := newFakeRunner()
		if err := newSvc(r).Restart(ctx); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		r.wantCalls(t, "launchctl kickstart -k system/sh.chainmail.daemon")
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
