package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

func TestCompleteWatchGroups(t *testing.T) {
	t.Parallel()
	f := newFakeWatch()
	f.groups = func() (*chainmailv1.ListGroupsResponse, error) { return watchGroupsFixture(), nil }
	env := sockEnv(startFakeWatch(t, f))
	cands, _ := completeArgs(t, env, "watch", "groups", "delete", "")
	if got := values(cands); !slices.Contains(got, "nightly") {
		t.Errorf("candidates %q, want nightly", got)
	}
	cands, _ = completeArgs(t, env, "watch", "--group", "ni")
	if got := values(cands); !slices.Equal(got, []string{"nightly"}) {
		t.Errorf("--group ni: %q, want nightly", got)
	}
}

func TestCompleteSecretNames(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{NewStore: offlineStores})
	cands, _ := completeArgs(t, sockEnv(h.Socket), "secret", "set", "accounts/")
	if got := values(cands); !slices.Equal(got, []string{"accounts/host", "accounts/icloud"}) {
		t.Errorf("candidates %q, want both password accounts", got)
	}
}

func TestCompleteAuditActions(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	env := sockEnv(m.socket)
	if got := runCLI(t, env, "folders"); got.code != 0 {
		t.Fatalf("folders: %+v", got)
	}
	cands, _ := completeArgs(t, env, "audit", "--action", "fol")
	if got := values(cands); !slices.Equal(got, []string{"folders.list"}) {
		t.Errorf("candidates %q, want folders.list", got)
	}
}

func TestCompleteSearchTerms(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	env := sockEnv(m.socket)
	tests := []struct {
		toComplete string
		want       []string
	}{
		{toComplete: "fr", want: []string{"from:"}},
		{toComplete: "-is:u", want: []string{"-is:unread"}},
		{toComplete: "has:", want: []string{"has:attachment"}},
		{toComplete: "account:", want: []string{"account:gmail", "account:host"}},
		{toComplete: "in:Arc", want: []string{"in:Archive"}},
		{toComplete: "in:in", want: []string{"in:inbox"}},
		{toComplete: "label:[Gm", want: []string{}},
	}
	for _, tt := range tests {
		args := []string{"search", "invoice", tt.toComplete}
		if strings.HasPrefix(tt.toComplete, "-") {
			// A term that starts with - needs -- before the query, as
			// searchHelp says, or cobra completes it as a flag name.
			args = []string{"search", "--", "invoice", tt.toComplete}
		}
		cands, d := completeArgs(t, env, args...)
		if got := values(cands); !slices.Equal(got, tt.want) {
			t.Errorf("search %q: %q, want %q", tt.toComplete, got, tt.want)
		}
		if d&cobra.ShellCompDirectiveNoFileComp == 0 {
			t.Errorf("search %q: directive %d lacks NoFileComp", tt.toComplete, d)
		}
	}
	if _, d := completeArgs(t, env, "search", "fr"); d&cobra.ShellCompDirectiveNoSpace == 0 {
		t.Errorf("an operator should not end in a space, directive %d", d)
	}
}

// TestEveryArgumentAndFlagCompletes is the coverage gate: every command,
// positional argument and flag in the tree has a completion, or is marked
// free text with noComplete. Bool flags take no value.
func TestEveryArgumentAndFlagCompletes(t *testing.T) {
	t.Parallel()
	env := Env{Getenv: func(string) string { return "" }}
	root := newRoot(t.Context(), env)
	paths := map[string]bool{}
	walk(root, func(c *cobra.Command) {
		paths[c.CommandPath()] = true
		if c.Name() == "help" {
			return
		}
		if c.ValidArgsFunction == nil {
			t.Errorf("%s: no completion for its arguments", c.CommandPath())
		}
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name == "help" || f.Value.Type() == "bool" {
				return
			}
			if _, ok := c.GetFlagCompletionFunc(f.Name); !ok {
				t.Errorf("%s --%s: no completion", c.CommandPath(), f.Name)
			}
		})
	})
	cp := &completer{ctx: t.Context(), env: env, g: &globals{}}
	for path := range cp.specs() {
		if !paths[path] {
			t.Errorf("specs has %q, which is not a command", path)
		}
	}
	if !paths["chainmail search"] || !paths["chainmail watch groups delete"] {
		t.Fatal("the walk did not reach every level of the tree")
	}
}
