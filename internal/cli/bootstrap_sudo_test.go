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

// TestSudoDirsIncludeWrappers shows NixOS's setuid wrapper directory is
// searched for sudo, after the directories every other host uses.
func TestSudoDirsIncludeWrappers(t *testing.T) {
	t.Parallel()
	if want := []string{"/usr/bin", "/bin", "/usr/local/bin", "/run/wrappers/bin"}; !slices.Equal(sudoDirs, want) {
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

// The secrets the tests of the secret checks use.
const (
	checkTyped  = "typed-secret-1"
	checkStored = "other-secret-22"
)

// secretChecks are the checks the bootstrap tests use on secrets.
type secretChecks struct {
	secret func(tb testing.TB, name string, got []byte, ok bool, want string)
	argv   func(tb testing.TB, what string, args []string, secret string)
}

// verifySecretChecks fails tb unless c reports exactly the three expected
// failures, none holding a secret, and nothing for matching values. Its
// own failures give indexes and counts, never what c reported, which may
// hold a secret.
func verifySecretChecks(tb testing.TB, c secretChecks) {
	tb.Helper()
	rec := &recordTB{TB: tb}
	c.secret(rec, "accounts/alice", []byte(checkStored), true, checkTyped)
	c.secret(rec, "accounts/bob", nil, false, checkTyped)
	c.argv(rec, "sudo", []string{"/bin/chainmail", "x" + checkTyped, "apply"}, checkTyped)
	if len(rec.msgs) != 3 {
		tb.Errorf("checks reported %d failures, want 3", len(rec.msgs))
		return
	}
	for i, m := range rec.msgs {
		if strings.Contains(m, checkTyped) || strings.Contains(m, checkStored) {
			tb.Errorf("failure %d holds a secret", i)
		}
	}
	for i, wantPart := range []string{"15 bytes", "no secret", "position 1 of 3"} {
		if !strings.Contains(rec.msgs[i], wantPart) {
			tb.Errorf("failure %d (%d bytes) does not say %q", i, len(rec.msgs[i]), wantPart)
		}
	}

	// Matching values report nothing.
	rec = &recordTB{TB: tb}
	c.secret(rec, "accounts/alice", []byte(checkTyped), true, checkTyped)
	c.argv(rec, "sudo", []string{"/bin/chainmail", "apply"}, checkTyped)
	if len(rec.msgs) != 0 {
		tb.Errorf("checks of good values reported %d failures, want none", len(rec.msgs))
	}
}

// The checks the bootstrap tests use on secrets report a failure without
// the secret in it.
func TestSecretChecksHideValues(t *testing.T) {
	t.Parallel()
	verifySecretChecks(t, secretChecks{secret: checkSecret, argv: checkArgv})
}

// verifySecretChecks catches checks that print secrets without printing
// them itself.
func TestVerifySecretChecksHidesValues(t *testing.T) {
	t.Parallel()
	leaky := func(times int) secretChecks {
		return secretChecks{
			secret: func(tb testing.TB, name string, got []byte, _ bool, want string) {
				for range times {
					tb.Errorf("%s holds %q, want %q", name, got, want)
				}
			},
			argv: func(tb testing.TB, what string, args []string, _ string) {
				for range times {
					tb.Errorf("%s argv %q", what, args)
				}
			},
		}
	}
	for _, times := range []int{1, 2} {
		rec := &recordTB{TB: t}
		verifySecretChecks(rec, leaky(times))
		if len(rec.msgs) == 0 {
			t.Errorf("checks that print secrets %d times each passed", times)
		}
		for i, m := range rec.msgs {
			if strings.Contains(m, checkTyped) || strings.Contains(m, checkStored) {
				t.Errorf("checks printing %d times: failure %d of verifySecretChecks holds a secret", times, i)
			}
		}
	}
}
