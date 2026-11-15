package system

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeExit stands in for *exec.ExitError: an error with an exit status,
// whose text is the same as the real one's.
type fakeExit struct{ code int }

func (e *fakeExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *fakeExit) ExitCode() int { return e.code }

type reply struct {
	out    string
	err    error
	stderr string
}

// fakeRunner records every command line and answers from canned replies.
// A command with no reply succeeds with no output. A command with several
// replies gets them in order, and the last one repeats. A failing reply's
// error is wrapped with its stderr exactly as ExecRunner wraps a real one.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	replies map[string][]reply
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{replies: map[string][]reply{}}
}

func (f *fakeRunner) on(line, out string, err error) *fakeRunner {
	f.replies[line] = append(f.replies[line], reply{out: out, err: err})
	return f
}

// onFail answers line with out and an exit status code carrying stderr.
func (f *fakeRunner) onFail(line, out string, code int, stderr string) *fakeRunner {
	f.replies[line] = append(f.replies[line], reply{out: out, err: &fakeExit{code: code}, stderr: stderr})
	return f
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	q := f.replies[line]
	if len(q) == 0 {
		return nil, nil
	}
	r := q[0]
	if len(q) > 1 {
		f.replies[line] = q[1:]
	}
	if r.err != nil {
		return []byte(r.out), commandError(name, args, r.err, []byte(r.stderr))
	}
	return []byte(r.out), nil
}

func (f *fakeRunner) wantCalls(t *testing.T, want ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(want) == 0 {
		want = nil
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("commands run:\n  %s\nwant:\n  %s", strings.Join(f.calls, "\n  "), strings.Join(want, "\n  "))
	}
}

// pathRunner is the runner ExecRunner is built on, finding commands through
// PATH, so the tests of how it runs a command and reports a failure can use
// sh on any host. ExecRunner itself only runs commands from the trusted
// system directories, which on some hosts (NixOS) hold no trusted sh.
var pathRunner = execRunner(exec.LookPath)

func TestExitCode(t *testing.T) {
	t.Parallel()
	_, realErr := pathRunner(context.Background(), "sh", "-c", "exit 3")
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"exit status", &fakeExit{code: 2}, 2},
		{"wrapped exit status", fmt.Errorf("getent: %w", &fakeExit{code: 2}), 2},
		{"no exit status", errors.New("boom"), -1},
		{"real command", realErr, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCode(tt.err); got != tt.want {
				t.Fatalf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestExecRunner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	out, err := pathRunner(ctx, "sh", "-c", "echo hello; echo noise >&2")
	if err != nil {
		t.Fatalf("ExecRunner: %v", err)
	}
	if string(out) != "hello\n" {
		t.Fatalf("stdout = %q, want %q", out, "hello\n")
	}

	_, err = pathRunner(ctx, "sh", "-c", "echo broken >&2; exit 4")
	if err == nil {
		t.Fatal("ExecRunner succeeded for a failing command")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("error %q does not carry stderr", err)
	}
	if exitCode(err) != 4 {
		t.Fatalf("exitCode = %d, want 4", exitCode(err))
	}
}

// TestExecRunnerErrors pins the fake runner's errors to the real ones, so
// tests that match on error text match what ExecRunner returns.
func TestExecRunnerErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, err := pathRunner(ctx, "sh", "-c", "echo broken >&2; exit 4")
	want := commandError("sh", []string{"-c", "echo broken >&2; exit 4"}, &fakeExit{code: 4}, []byte("broken\n"))
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("ExecRunner error = %v, fake gives %v", err, want)
	}

	_, err = pathRunner(ctx, "sh", "-c", "exit 5")
	want = commandError("sh", []string{"-c", "exit 5"}, &fakeExit{code: 5}, nil)
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("ExecRunner error = %v, fake gives %v", err, want)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = pathRunner(cancelled, "sh", "-c", "true")
	if !errors.Is(err, context.Canceled) || exitCode(err) != -1 {
		t.Fatalf("cancelled ExecRunner error = %v (exit %d), want context.Canceled with no exit status", err, exitCode(err))
	}

	_, err = ExecRunner(ctx, "chainmail-no-such-command")
	if !errors.Is(err, exec.ErrNotFound) || exitCode(err) != -1 {
		t.Fatalf("missing command error = %v (exit %d), want exec.ErrNotFound with no exit status", err, exitCode(err))
	}
}

func TestPickFreeID(t *testing.T) {
	t.Parallel()
	full := map[uint32]bool{}
	for id := uint32(300); id < 500; id++ {
		full[id] = true
	}
	tests := []struct {
		name   string
		used   map[uint32]bool
		want   uint32
		wantOK bool
	}{
		{"none used", map[uint32]bool{}, 300, true},
		{"gap", map[uint32]bool{300: true, 301: true, 303: true}, 302, true},
		{"below range ignored", map[uint32]bool{0: true, 299: true}, 300, true},
		{"full", full, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := pickFreeID(tt.used)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("pickFreeID = %d, %v, want %d, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
