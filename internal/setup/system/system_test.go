package system

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeExit stands in for *exec.ExitError: an error with an exit status.
type fakeExit struct {
	code   int
	stderr string
}

func (e *fakeExit) Error() string { return fmt.Sprintf("exit status %d: %s", e.code, e.stderr) }
func (e *fakeExit) ExitCode() int { return e.code }

type reply struct {
	out string
	err error
}

// fakeRunner records every command line and answers from canned replies.
// A command with no reply succeeds with no output. A command with several
// replies gets them in order, and the last one repeats.
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
	return []byte(r.out), r.err
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

func TestExitCode(t *testing.T) {
	t.Parallel()
	_, realErr := ExecRunner(context.Background(), "sh", "-c", "exit 3")
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

	out, err := ExecRunner(ctx, "sh", "-c", "echo hello; echo noise >&2")
	if err != nil {
		t.Fatalf("ExecRunner: %v", err)
	}
	if string(out) != "hello\n" {
		t.Fatalf("stdout = %q, want %q", out, "hello\n")
	}

	_, err = ExecRunner(ctx, "sh", "-c", "echo broken >&2; exit 4")
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
