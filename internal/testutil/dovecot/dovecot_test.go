package dovecot

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRunArgs(t *testing.T) {
	t.Parallel()
	expires := time.Unix(1_800_000_000, 0)
	args := runArgs("chainmail-dovecot-x", "/tmp/d", 20*time.Minute, expires)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--name chainmail-dovecot-x",
		"--label chainmail-test=dovecot",
		"--label chainmail-test.expires=1800000000",
		"--entrypoint /usr/bin/tini",
		Image + " -- /usr/bin/timeout -k 10s 1200s /usr/sbin/dovecot -F",
		"-v /tmp/d/dovecot.conf:/etc/dovecot/dovecot.conf:ro",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker %s\nlacks %q", joined, want)
		}
	}
	if slices.Contains(args, "--rm") {
		t.Errorf("docker %s: --rm loses the log of a container that dies at startup", joined)
	}
}

func TestStaleContainers(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	out := "old 1799999999\n" +
		"live 1800000600\n" +
		"unlabelled \n" +
		"garbled soon\n" +
		"\n"
	got := staleContainers(out, now)
	if want := []string{"old", "unlabelled", "garbled"}; !slices.Equal(got, want) {
		t.Errorf("staleContainers = %v, want %v", got, want)
	}
}

func TestSweepDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	lifetime := 20 * time.Minute
	mk := func(name string, mtime time.Time) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "key.pem"), []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	past := strconv.FormatInt(now.Add(-time.Second).Unix(), 10)
	future := strconv.FormatInt(now.Add(time.Minute).Unix(), 10)
	mk(dirPrefix+past+"-123", now)
	mk(dirPrefix+future+"-123", now.Add(-time.Hour)) // the expiry in the name wins over the mtime
	mk(dirPrefix+"123", now.Add(-lifetime-time.Second))
	mk(dirPrefix+"456", now.Add(-time.Minute))
	mk("other-"+past, now.Add(-time.Hour))
	sweepDirs(root, now, lifetime)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := []string{dirPrefix + "456", dirPrefix + future + "-123", "other-" + past}
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
}

func TestLifetimeFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		base, timeout, want time.Duration
	}{
		{20 * time.Minute, 15 * time.Minute, 20 * time.Minute},
		{20 * time.Minute, 0, 20 * time.Minute},
		{20 * time.Minute, 30 * time.Minute, 34 * time.Minute},
		{20 * time.Minute, 17 * time.Minute, 21 * time.Minute},
		{30 * time.Second, 0, 30 * time.Second},
	}
	for _, tt := range tests {
		if got := lifetimeFor(tt.base, tt.timeout); got != tt.want {
			t.Errorf("lifetimeFor(%v, %v) = %v, want %v", tt.base, tt.timeout, got, tt.want)
		}
	}
}

type mainProbe struct {
	srv     *Server
	stops   atomic.Int32
	stopErr error
	shared  *Server
}

func (p *mainProbe) start() (*Server, error) { return p.srv, nil }

func (p *mainProbe) stop(s *Server) error {
	if s != p.srv {
		return errors.New("stopped the wrong server")
	}
	p.stops.Add(1)
	return p.stopErr
}

func noExit(int) { panic("exit called") }

// runMain tests share the package's Shared server, so they do not run in parallel.

func TestRunMain(t *testing.T) {
	p := &mainProbe{srv: &Server{id: "a"}}
	code := runMain(p.start, p.stop, func() int {
		p.shared = Shared()
		if p.stops.Load() != 0 {
			t.Error("stopped before the tests ran")
		}
		return 3
	}, nil, noExit)
	if code != 3 || p.stops.Load() != 1 || p.shared != p.srv || Shared() != nil {
		t.Errorf("code %d, %d stops, Shared during run %v, after %v", code, p.stops.Load(), p.shared, Shared())
	}
}

func TestRunMainStartFails(t *testing.T) {
	ran := false
	code := runMain(func() (*Server, error) { return nil, errors.New("no docker") }, nil, func() int { ran = true; return 0 }, nil, noExit)
	if code == 0 || ran {
		t.Errorf("code %d, tests ran %v; want a failure without running tests", code, ran)
	}
}

func TestRunMainStopFails(t *testing.T) {
	p := &mainProbe{srv: &Server{id: "a"}, stopErr: errors.New("rm failed")}
	if code := runMain(p.start, p.stop, func() int { return 0 }, nil, noExit); code == 0 {
		t.Error("a failed Stop after passing tests returned 0")
	}
}

func TestRunMainPanic(t *testing.T) {
	p := &mainProbe{srv: &Server{id: "a"}}
	func() {
		defer func() { _ = recover() }()
		runMain(p.start, p.stop, func() int { panic("boom") }, nil, noExit)
	}()
	if p.stops.Load() != 1 {
		t.Errorf("%d stops after a panic, want 1", p.stops.Load())
	}
}

func TestRunMainSignal(t *testing.T) {
	p := &mainProbe{srv: &Server{id: "a"}}
	sigs := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	release := make(chan struct{})
	done := make(chan int)
	go func() {
		done <- runMain(p.start, p.stop, func() int { <-release; return 0 }, sigs, func(code int) {
			exited <- code
			close(release)
		})
	}()
	sigs <- syscall.SIGTERM
	select {
	case code := <-exited:
		if code == 0 {
			t.Error("exit(0) after a signal")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no exit after SIGTERM")
	}
	<-done
	if p.stops.Load() != 1 {
		t.Errorf("%d stops after a signal, want 1", p.stops.Load())
	}
}

func TestRunMainSignalDuringStart(t *testing.T) {
	p := &mainProbe{srv: &Server{id: "a"}}
	sigs := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	ran := false
	start := func() (*Server, error) {
		sigs <- os.Interrupt
		select {
		case <-exited: // the signal was handled while start was still running
		case <-time.After(10 * time.Second):
			t.Error("a signal during start was not handled")
		}
		return p.start()
	}
	code := runMain(start, p.stop, func() int { ran = true; return 0 }, sigs, func(code int) { exited <- code })
	if code == 0 || ran || p.stops.Load() != 1 {
		t.Errorf("code %d, tests ran %v, %d stops; want a failure, no tests and the server stopped", code, ran, p.stops.Load())
	}
}

func TestRunMainSignalAfterRun(t *testing.T) {
	p := &mainProbe{srv: &Server{id: "a"}}
	sigs := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	code := runMain(p.start, p.stop, func() int {
		sigs <- syscall.SIGTERM
		<-exited
		return 0
	}, sigs, func(code int) { exited <- code })
	if code == 0 || p.stops.Load() != 1 {
		t.Errorf("code %d, %d stops; an interrupted run must not exit 0", code, p.stops.Load())
	}
}

func TestPrefixedConfig(t *testing.T) {
	t.Parallel()
	got := string(prefixedConfig)
	if !strings.Contains(got, "namespace inbox {\n  inbox = yes\n  prefix = INBOX.\n  separator = .\n") || strings.Contains(got, "separator = /") {
		t.Errorf("prefixed config has the wrong namespace:\n%s", got)
	}
	if strings.Contains(string(config), "prefix = INBOX.") {
		t.Error("the shared config gained the prefix")
	}
}
