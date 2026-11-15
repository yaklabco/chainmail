package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// plantSudo puts a fake sudo first in PATH that records being run in a
// marker file, and returns the marker.
func plantSudo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script := "#!/bin/sh\ntouch '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func wantSudoNotPlanted(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the sudo planted first in PATH was run")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestSudoDirs(t *testing.T) {
	t.Parallel()
	if want := []string{"/usr/bin", "/bin", "/usr/local/bin"}; !slices.Equal(sudoDirs, want) {
		t.Fatalf("sudoDirs = %v, want %v", sudoDirs, want)
	}
}

// TestRunSudoUsesTrustedPath shows bootstrap runs the sudo its finder
// returns, with the payload on stdin, and never one found through PATH.
func TestRunSudoUsesTrustedPath(t *testing.T) {
	marker := plantSudo(t)
	dir := t.TempDir()
	args, stdin := filepath.Join(dir, "args"), filepath.Join(dir, "stdin")
	trusted := filepath.Join(t.TempDir(), "sudo")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + args + "'\ncat > '" + stdin + "'\necho done\n"
	if err := os.WriteFile(trusted, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	find := func() (string, error) { return trusted, nil }
	if err := runSudo(t.Context(), find, strings.NewReader("payload"), &out, "/opt/chainmail", "bootstrap", "apply"); err != nil {
		t.Fatalf("runSudo: %v", err)
	}
	wantSudoNotPlanted(t, marker)
	if got, err := os.ReadFile(args); err != nil || string(got) != "/opt/chainmail\nbootstrap\napply\n" {
		t.Fatalf("trusted sudo got args %q (%v)", got, err)
	}
	if got, err := os.ReadFile(stdin); err != nil || string(got) != "payload" {
		t.Fatalf("trusted sudo read %d bytes of stdin (%v), want the 7-byte payload", len(got), err)
	}
	if out.String() != "done\n" {
		t.Fatalf("stdout = %q, want the trusted sudo's", out.String())
	}
}

// TestRunSudoNoTrustedSudo shows a host with no trusted sudo gets an error,
// not the sudo first in PATH.
func TestRunSudoNoTrustedSudo(t *testing.T) {
	marker := plantSudo(t)
	find := func() (string, error) { return "", errors.New("command sudo has no trusted copy") }
	err := runSudo(t.Context(), find, strings.NewReader("payload"), &bytes.Buffer{}, "/opt/chainmail", "bootstrap", "apply")
	if err == nil || !strings.Contains(err.Error(), "no trusted copy") {
		t.Fatalf("runSudo with no trusted sudo = %v, want the finder's error", err)
	}
	wantSudoNotPlanted(t, marker)
}

// recordTB records the failures a check reports instead of failing.
type recordTB struct {
	testing.TB
	msgs []string
}

func (r *recordTB) Helper() {}

func (r *recordTB) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

// The checks the bootstrap tests use on secrets report a failure without
// the secret in it.
func TestSecretChecksHideValues(t *testing.T) {
	t.Parallel()
	const want, stored = "typed-secret-1", "other-secret-22"

	rec := &recordTB{TB: t}
	checkSecret(rec, "accounts/alice", []byte(stored), true, want)
	checkSecret(rec, "accounts/bob", nil, false, want)
	checkArgv(rec, "sudo", []string{"/bin/chainmail", "x" + want, "apply"}, want)
	if len(rec.msgs) != 3 {
		t.Fatalf("checks reported %d failures, want 3: %q", len(rec.msgs), rec.msgs)
	}
	for _, m := range rec.msgs {
		if strings.Contains(m, want) || strings.Contains(m, stored) {
			t.Errorf("failure message holds a secret: %q", m)
		}
	}
	for i, wantPart := range []string{"15 bytes", "no secret", "position 1 of 3"} {
		if !strings.Contains(rec.msgs[i], wantPart) {
			t.Errorf("failure %d = %q, want it to say %q", i, rec.msgs[i], wantPart)
		}
	}

	// Matching values report nothing.
	rec = &recordTB{TB: t}
	checkSecret(rec, "accounts/alice", []byte(want), true, want)
	checkArgv(rec, "sudo", []string{"/bin/chainmail", "apply"}, want)
	if len(rec.msgs) != 0 {
		t.Fatalf("checks of good values reported %q", rec.msgs)
	}
}
