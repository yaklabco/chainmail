package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// fakePipe is a PipeRunner that records each command line with a copy of
// its stdin, and answers with out and err.
type fakePipe struct {
	mu     sync.Mutex
	out    []byte
	err    error
	calls  []string
	stdins [][]byte
}

func (f *fakePipe) run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, commandLine(name, args))
	f.stdins = append(f.stdins, bytes.Clone(stdin))
	return f.out, f.err
}

// wantCall fails unless exactly one command ran, line, with stdin.
func (f *fakePipe) wantCall(t *testing.T, line string, stdin []byte) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 || f.calls[0] != line {
		t.Fatalf("commands run = %q, want [%q]", f.calls, line)
	}
	if !bytes.Equal(f.stdins[0], stdin) {
		t.Fatal("stdin is not what the caller passed")
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

const (
	encryptLine = "systemd-creds encrypt --name=vault-key - -"
	decryptLine = "systemd-creds decrypt --name=vault-key - -"
	plainKey    = "AGE-SECRET-KEY-1PLAINTEXTNEVERINARGV"
)

func TestCredsEncrypt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("seals through stdin", func(t *testing.T) {
		t.Parallel()
		p := &fakePipe{out: []byte("sealed credential")}
		got, err := NewCreds(p.run).Encrypt(ctx, "vault-key", []byte(plainKey))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if string(got) != "sealed credential" {
			t.Fatalf("Encrypt = %q, want the command's stdout", got)
		}
		p.wantCall(t, encryptLine, []byte(plainKey))
		if strings.Contains(p.calls[0], plainKey) {
			t.Fatal("the plaintext is in argv")
		}
	})

	t.Run("runner error", func(t *testing.T) {
		t.Parallel()
		args := []string{"encrypt", "--name=vault-key", "-", "-"}
		p := &fakePipe{err: commandError("systemd-creds", args, &fakeExit{code: 1}, []byte("no TPM"))}
		got, err := NewCreds(p.run).Encrypt(ctx, "vault-key", []byte(plainKey))
		var exit *fakeExit
		if !errors.As(err, &exit) || !strings.Contains(err.Error(), "no TPM") {
			t.Fatalf("Encrypt error = %v, want the runner's error wrapped", err)
		}
		if strings.Contains(err.Error(), plainKey) {
			t.Fatal("the Encrypt error holds the plaintext")
		}
		if got != nil {
			t.Fatal("Encrypt returned a credential with its error")
		}
	})
}

func TestCredsCheckDecrypt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cred := []byte("sealed credential")

	t.Run("decrypts", func(t *testing.T) {
		t.Parallel()
		out := []byte(plainKey)
		p := &fakePipe{out: out}
		if err := NewCreds(p.run).CheckDecrypt(ctx, "vault-key", cred); err != nil {
			t.Fatalf("CheckDecrypt: %v", err)
		}
		p.wantCall(t, decryptLine, cred)
		if !allZero(out) {
			t.Fatal("the decrypted plaintext was not cleared")
		}
	})

	t.Run("command exits non-zero", func(t *testing.T) {
		t.Parallel()
		out := []byte(plainKey)
		args := []string{"decrypt", "--name=vault-key", "-", "-"}
		p := &fakePipe{out: out, err: commandError("systemd-creds", args, &fakeExit{code: 1}, []byte("bad MAC"))}
		err := NewCreds(p.run).CheckDecrypt(ctx, "vault-key", cred)
		var exit *fakeExit
		if !errors.As(err, &exit) {
			t.Fatalf("CheckDecrypt error = %v, want the runner's error wrapped", err)
		}
		if !errors.Is(err, ErrNotDecrypted) {
			t.Fatalf("CheckDecrypt error = %v, want it to wrap ErrNotDecrypted", err)
		}
		const prefix = "the credential does not decrypt on this host: "
		if !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), "bad MAC") {
			t.Fatalf("CheckDecrypt error = %q, want %q and the command's stderr", err, prefix)
		}
		if !allZero(out) {
			t.Fatal("output returned with the error was not cleared")
		}
	})

	// Errors where systemd-creds did not run, or did not exit with a
	// status, say nothing about the credential.
	operational := []struct {
		name string
		err  error
	}{
		{"not found", fmt.Errorf("systemd-creds decrypt --name=vault-key - -: %w", exec.ErrNotFound)},
		{"untrusted copy", errors.New("systemd-creds decrypt --name=vault-key - -: /usr/bin/systemd-creds is writable by group or others")},
		{"killed by a signal", commandError("systemd-creds", nil, &fakeExit{code: -1}, nil)},
		{"context ended", fmt.Errorf("systemd-creds: %w", context.DeadlineExceeded)},
	}
	for _, tc := range operational {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &fakePipe{err: tc.err}
			err := NewCreds(p.run).CheckDecrypt(ctx, "vault-key", cred)
			if err == nil || errors.Is(err, ErrNotDecrypted) {
				t.Fatalf("CheckDecrypt error = %v, want an error without ErrNotDecrypted", err)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("CheckDecrypt error = %v, want the runner's error wrapped", err)
			}
		})
	}

	t.Run("finder finds no systemd-creds", func(t *testing.T) {
		t.Parallel()
		tr := newTree(t)
		err := NewCreds(execPipeRunner(tr.finder().find)).CheckDecrypt(ctx, "vault-key", cred)
		if !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("CheckDecrypt error = %v, want exec.ErrNotFound", err)
		}
		if errors.Is(err, ErrNotDecrypted) {
			t.Fatalf("CheckDecrypt error = %v wraps ErrNotDecrypted, but systemd-creds never ran", err)
		}
	})
}

func TestExecPipeRunner(t *testing.T) {
	ctx := context.Background()
	// Through PATH, as pathRunner does, so the real cat runs on any host.
	pipe := execPipeRunner(exec.LookPath)

	t.Run("stdin in, stdout out", func(t *testing.T) {
		out, err := pipe(ctx, []byte("line one\nline two\n"), "cat")
		if err != nil {
			t.Fatalf("cat: %v", err)
		}
		if string(out) != "line one\nline two\n" {
			t.Fatalf("stdout = %q, want stdin back", out)
		}
	})

	t.Run("failure carries stderr, never stdin", func(t *testing.T) {
		_, err := pipe(ctx, []byte(plainKey), "sh", "-c", "cat >/dev/null; echo broken >&2; exit 4")
		if err == nil {
			t.Fatal("a failing command succeeded")
		}
		if !strings.Contains(err.Error(), "broken") {
			t.Fatalf("error %q does not carry stderr", err)
		}
		if strings.Contains(err.Error(), plainKey) {
			t.Fatal("the error holds stdin")
		}
		if exitCode(err) != 4 {
			t.Fatalf("exitCode = %d, want 4", exitCode(err))
		}
	})

	t.Run("trusted root, minimal environment, working directory /", func(t *testing.T) {
		t.Setenv("CHAINMAIL_SOCKET", "/tmp/leak.sock")
		t.Chdir(t.TempDir())
		tr := newTree(t)
		tr.file("/usr/bin/echo-stdin",
			`IFS= read -r line; printf '%s|%s|%s|%s|%s\n' "$line" "$PATH" "$CHAINMAIL_SOCKET" "$0" "$(pwd)"`, 0o755)
		out, err := execPipeRunner(tr.finder().find)(ctx, []byte("piped\n"), "echo-stdin")
		if err != nil {
			t.Fatalf("echo-stdin: %v", err)
		}
		want := "piped|/usr/sbin:/usr/bin:/sbin:/bin||" + tr.path("/usr/bin/echo-stdin") + "|/\n"
		if string(out) != want {
			t.Fatalf("output = %q, want %q", out, want)
		}
	})

	t.Run("ignores PATH", func(t *testing.T) {
		marker := hostilePath(t, "chainmail-hostile-command")
		_, err := ExecPipeRunner(ctx, nil, "chainmail-hostile-command")
		if !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("ExecPipeRunner of a command only in PATH = %v, want exec.ErrNotFound", err)
		}
		wantNotRun(t, marker)
	})
}
