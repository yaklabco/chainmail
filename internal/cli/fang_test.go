package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/jamesainslie/chainmail/internal/api"
)

// flat lowercases s and collapses its whitespace, so styled output that
// wraps or capitalises can be searched.
func flat(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func TestTerminalErrorsAreStyled(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{StderrTerminal: true}, "accounts", "nope")
	if got.code != api.ExitUsage {
		t.Fatalf("exit code = %d, want %d (stderr %q)", got.code, api.ExitUsage, got.stderr)
	}
	if json.Valid([]byte(strings.TrimSpace(got.stderr))) {
		t.Errorf("stderr is JSON on a terminal: %q", got.stderr)
	}
	for _, want := range []string{"usage", `unknown command "nope" for "chainmail accounts"`, "run: chainmail accounts --help"} {
		if !strings.Contains(flat(got.stderr), want) {
			t.Errorf("stderr %q does not contain %q", got.stderr, want)
		}
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want empty", got.stdout)
	}
}

func TestTerminalExitCodesMatchPlain(t *testing.T) {
	t.Parallel()
	for _, tt := range usageErrorCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plain := runCLI(t, Env{}, tt.args...)
			styled := runCLI(t, Env{StderrTerminal: true}, tt.args...)
			if plain.code != styled.code {
				t.Errorf("exit code plain %d, terminal %d", plain.code, styled.code)
			}
			if styled.stdout != "" {
				t.Errorf("terminal stdout = %q, want empty", styled.stdout)
			}
		})
	}
}

func TestTerminalHelpShowsGroups(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{StderrTerminal: true}, "--help")
	if got.code != 0 {
		t.Fatalf("exit code = %d, stderr %q", got.code, got.stderr)
	}
	for _, title := range []string{"READ", "COMPOSE", "ORGANIZE", "WATCH", "ACCOUNTS AND AUTH", "ADMIN"} {
		if !strings.Contains(got.stdout, title) {
			t.Errorf("help does not show group %q:\n%s", title, got.stdout)
		}
	}
}

func TestPlainHelpShowsGroups(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{}, "--help")
	if got.code != 0 {
		t.Fatalf("exit code = %d, stderr %q", got.code, got.stderr)
	}
	for _, title := range []string{"Read", "Compose", "Organize", "Watch", "Accounts and auth", "Admin"} {
		if !strings.Contains(got.stdout, "\n"+title+"\n") {
			t.Errorf("help does not show group %q:\n%s", title, got.stdout)
		}
	}
	if strings.Contains(got.stdout, "Additional Commands") {
		t.Errorf("help lists ungrouped commands:\n%s", got.stdout)
	}
}

func TestWalkVisitsParentsFirst(t *testing.T) {
	t.Parallel()
	root := newRoot(t.Context(), Env{Getenv: func(string) string { return "" }})
	seen := map[string]bool{}
	walk(root, func(c *cobra.Command) {
		if p := c.Parent(); p != nil && !seen[p.CommandPath()] {
			t.Errorf("%s visited before its parent %s", c.CommandPath(), p.CommandPath())
		}
		seen[c.CommandPath()] = true
	})
	for _, path := range []string{"chainmail", "chainmail accounts", "chainmail accounts list", "chainmail admin"} {
		if !seen[path] {
			t.Errorf("walk did not visit %q", path)
		}
	}
}

func TestEveryTopLevelCommandHasAGroup(t *testing.T) {
	t.Parallel()
	root := newRoot(t.Context(), Env{Getenv: func(string) string { return "" }})
	for _, c := range root.Commands() {
		if c.Hidden {
			continue
		}
		if c.GroupID == "" {
			t.Errorf("%s has no group: add it to groupOf", c.Name())
		}
	}
}
