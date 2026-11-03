package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestEveryCommandHasExamples(t *testing.T) {
	t.Parallel()
	root := newRoot(t.Context(), Env{Getenv: func(string) string { return "" }})
	paths := map[string]bool{}
	walk(root, func(c *cobra.Command) {
		paths[c.CommandPath()] = true
		if c.Hidden || c.Annotations[groupAnnotation] != "" {
			return
		}
		if c.Example == "" {
			t.Errorf("%s has no Example: add it to examples in helptext.go", c.CommandPath())
		}
	})
	for path := range examples {
		if !paths[path] {
			t.Errorf("examples has %q, which is not a command", path)
		}
	}
	for path := range longHelp {
		if !paths[path] {
			t.Errorf("longHelp has %q, which is not a command", path)
		}
	}
}

// TestLongHelpHasNoExamples keeps examples in one place: cobra prints
// Example under its own heading, so examples left in Long show twice.
func TestLongHelpHasNoExamples(t *testing.T) {
	t.Parallel()
	root := newRoot(t.Context(), Env{Getenv: func(string) string { return "" }})
	walk(root, func(c *cobra.Command) {
		if strings.Contains(c.Long, "Example:\n") || strings.Contains(c.Long, "Examples:\n") {
			t.Errorf("%s has examples in Long: move them to examples in helptext.go", c.CommandPath())
		}
	})
}

func TestSearchHelpKeepsExamplesOutOfLong(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{}, "search", "--help")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if want := "Examples:\n  chainmail search invoice from:acme newer_than:30d"; !strings.Contains(got.stdout, want) {
		t.Errorf("search help lacks %q:\n%s", want, got.stdout)
	}
}
