package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/api"
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
	got := runCLI(t, Env{}, "--output", "json", "version")
	if got.code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", got.code, got.stderr)
	}
	if want := `{"version":"dev"}` + "\n"; got.stdout != want {
		t.Errorf("stdout = %q, want %q", got.stdout, want)
	}
}

// usageErrorCases are invocations that must exit 2 with USAGE.
var usageErrorCases = []struct {
	name string
	args []string
}{
	{name: "unknown flag", args: []string{"version", "--nope"}},
	{name: "extra argument", args: []string{"version", "extra"}},
	{name: "unknown command", args: []string{"nope"}},
	{name: "no arguments", args: nil},
	{name: "bare accounts", args: []string{"accounts"}},
	{name: "unknown accounts subcommand", args: []string{"accounts", "nope"}},
	{name: "bare secret", args: []string{"secret"}},
	{name: "unknown secret subcommand", args: []string{"secret", "nope"}},
	{name: "bare auth", args: []string{"auth"}},
	{name: "unknown auth subcommand", args: []string{"auth", "nope"}},
	{name: "bare admin", args: []string{"admin"}},
	{name: "unknown admin subcommand", args: []string{"admin", "nope"}},
	{name: "bare item", args: []string{"item"}},
	{name: "unknown item subcommand", args: []string{"item", "nope"}},
	{name: "bare item criteria", args: []string{"item", "criteria"}},
	{name: "bare rule", args: []string{"rule"}},
	{name: "unknown rule subcommand", args: []string{"rule", "nope"}},
	{name: "extra argument to a subcommand", args: []string{"accounts", "list", "extra"}},
	{name: "missing argument", args: []string{"accounts", "test"}},
	{name: "daemon argument", args: []string{"daemon", "extra"}},
	{name: "completion command", args: []string{"completion", "nope"}},
	{name: "completion without a shell", args: []string{"completion"}},
	{name: "completion for two shells", args: []string{"completion", "bash", "zsh"}},
	{name: "man with an argument", args: []string{"man", "extra"}},
	{name: "help for an unknown topic", args: []string{"help", "nope"}},
	{name: "help for an unknown subcommand", args: []string{"help", "accounts", "nope"}},
}

func TestUsageErrorsExitTwo(t *testing.T) {
	t.Parallel()
	for _, tt := range usageErrorCases {
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
			if out.Error.Reason != api.ReasonUsage {
				t.Errorf("error.reason = %q, want %q", out.Error.Reason, api.ReasonUsage)
			}
		})
	}
}

func TestGroupUsageHints(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		hint string
	}{
		{args: nil, hint: "run: chainmail --help"},
		{args: []string{"accounts"}, hint: "run: chainmail accounts --help"},
		{args: []string{"secret", "nope"}, hint: "run: chainmail secret --help"},
		{args: []string{"auth", "nope"}, hint: "run: chainmail auth --help"},
		{args: []string{"admin"}, hint: "run: chainmail admin --help"},
		{args: []string{"accounts", "test"}, hint: "run: chainmail accounts test --help"},
		{args: []string{"admin", "status", "--nope"}, hint: "run: chainmail admin status --help"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, tt.args...)
			var out errorOutput
			if err := json.Unmarshal([]byte(got.stderr), &out); err != nil {
				t.Fatalf("stderr = %q is not JSON: %v", got.stderr, err)
			}
			if out.Error.Hint != tt.hint {
				t.Errorf("hint = %q, want %q", out.Error.Hint, tt.hint)
			}
		})
	}
}

func TestGroupHelpExitsZero(t *testing.T) {
	t.Parallel()
	for _, group := range []string{"accounts", "secret", "auth", "admin"} {
		got := runCLI(t, Env{}, group, "--help")
		if got.code != 0 || !strings.Contains(got.stdout, group) || got.stderr != "" {
			t.Errorf("%s --help: %+v", group, got)
		}
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

func TestHelpCommand(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"help"}, {"help", "accounts"}, {"help", "accounts", "list"}} {
		got := runCLI(t, Env{}, args...)
		want := "chainmail " + strings.Join(args[1:], " ")
		if got.code != 0 || got.stderr != "" || !strings.Contains(got.stdout, strings.TrimSpace(want)) {
			t.Errorf("%v: %+v", args, got)
		}
	}
}

func TestOutputFlagHelpNamesDefault(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{}, "--help")
	if !strings.Contains(got.stdout, "table on a terminal, json otherwise") {
		t.Errorf("help = %s", got.stdout)
	}
}
