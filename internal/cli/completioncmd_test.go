package cli

import (
	"strings"
	"testing"
)

func TestCompletionScripts(t *testing.T) {
	t.Parallel()
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, "completion", shell)
			if got.code != 0 || got.stderr != "" {
				t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
			}
			if !strings.Contains(got.stdout, "chainmail") || !strings.Contains(got.stdout, "__complete") {
				t.Errorf("%s script does not call chainmail __complete:\n%.300s", shell, got.stdout)
			}
		})
	}
}

func TestManPage(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{}, "man")
	if got.code != 0 || !strings.Contains(got.stdout, ".TH") || !strings.Contains(got.stdout, "search") {
		t.Errorf("exit %d, stdout %.300q", got.code, got.stdout)
	}
}

func TestManIsHidden(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{}, "--help")
	if strings.Contains(got.stdout, "\n  man ") {
		t.Errorf("help lists man:\n%s", got.stdout)
	}
}
