package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type cliResult struct {
	stdout string
	stderr string
	code   int
}

// runCLI runs chainmail in-process. Fields left nil in env get test defaults.
func runCLI(t *testing.T, env Env, args ...string) cliResult {
	t.Helper()
	var out, errOut bytes.Buffer
	if env.Stdout == nil {
		env.Stdout = &out
	}
	if env.Stderr == nil {
		env.Stderr = &errOut
	}
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	code := Execute(t.Context(), args, env)
	return cliResult{stdout: out.String(), stderr: errOut.String(), code: code}
}

func TestVersion(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{}, "version")
	if got.code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", got.code, got.stderr)
	}
	if want := `{"version":"dev"}` + "\n"; got.stdout != want {
		t.Errorf("stdout = %q, want %q", got.stdout, want)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "unknown flag", args: []string{"version", "--nope"}},
		{name: "extra argument", args: []string{"version", "extra"}},
		{name: "unknown command", args: []string{"nope"}},
		{name: "no arguments", args: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, tt.args...)
			if got.code != 2 {
				t.Errorf("exit code = %d, want 2 (stderr %s)", got.code, got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			if n := strings.Count(got.stderr, "\n"); n != 1 || !strings.HasSuffix(got.stderr, "\n") {
				t.Fatalf("stderr = %q, want exactly one line", got.stderr)
			}
			var out errorOutput
			if err := json.Unmarshal([]byte(got.stderr), &out); err != nil {
				t.Fatalf("stderr = %q is not JSON: %v", got.stderr, err)
			}
			if out.Error.Reason != reasonUsage {
				t.Errorf("error.reason = %q, want %q", out.Error.Reason, reasonUsage)
			}
		})
	}
}

func TestHelpExitsZero(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{}, "--help")
	if got.code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "chainmail") {
		t.Errorf("stdout = %q, want help text", got.stdout)
	}
	if got.stderr != "" {
		t.Errorf("stderr = %q, want empty", got.stderr)
	}
}
