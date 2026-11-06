package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
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
		// in: and account: choose where to search and cannot be negated.
		{toComplete: "-i", want: []string{"-is:", "-item:"}},
		{toComplete: "-a", want: []string{"-after:"}},
		{toComplete: "-in:in", want: []string{}},
		{toComplete: "-account:", want: []string{}},
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

func TestCompleteSearchLabelsOnGoogleAccountsOnly(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	m.gmail.AddFolder("Receipts", mailstore.RoleNone)
	m.host.AddFolder("Projects", mailstore.RoleNone)
	cands, _ := completeArgs(t, sockEnv(m.socket), "search", "label:")
	if got, want := values(cands), []string{"label:Receipts"}; !slices.Equal(got, want) {
		t.Errorf("label: %q, want %q", got, want)
	}
	cands, _ = completeArgs(t, sockEnv(m.socket), "search", "in:Pro")
	if got, want := values(cands), []string{"in:Projects"}; !slices.Equal(got, want) {
		t.Errorf("in:Pro %q, want %q", got, want)
	}
}

// TestCompleteSearchTermsRoundTrip parses every completed in: and label:
// term and checks it names exactly the folder it was completed from, so a
// folder name the query language reads as syntax is never offered.
func TestCompleteSearchTermsRoundTrip(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	names := []string{
		"Receipts", "Work:2024", "Ünïcode", "-dash", "a\\b",
		"Receipts OR subject:secret", "(paren", "close)", `quo"te`, "nb\u00a0sp", "ideo\u3000sp", " Lead", "Trail ",
	}
	for _, n := range names {
		m.gmail.AddFolder(n, mailstore.RoleNone)
	}
	safe := []string{"Receipts", "Work:2024", "Ünïcode", "-dash", "a\\b"}
	for _, op := range []string{"in", "label"} {
		cands, _ := completeArgs(t, sockEnv(m.socket), "search", op+":")
		var offered []string
		for _, term := range values(cands) {
			n, err := query.Parse(term)
			if err != nil {
				t.Errorf("%s does not parse: %v", term, err)
				continue
			}
			if n.Op != query.OpTerm || string(n.Field) != op {
				t.Errorf("%s parses as %s, want one %s: term", term, n, op)
				continue
			}
			if want := strings.TrimPrefix(term, op+":"); n.Value != want {
				t.Errorf("%s names %q, want %q", term, n.Value, want)
			}
			offered = append(offered, n.Value)
		}
		for _, s := range safe {
			if !slices.Contains(offered, s) {
				t.Errorf("%s: offered %q, want it to include %q", op, offered, s)
			}
		}
		// Every other value is a role or a stock folder: none is a name
		// from the list cleaned or cut short.
		stock := append([]string{"INBOX", "Sent", "Drafts", "Archive", "Trash", "Junk"}, inRoles...)
		for _, n := range offered {
			if !slices.Contains(safe, n) && !slices.Contains(stock, n) && !strings.HasPrefix(n, "[Gmail]/") {
				t.Errorf("%s: offered %q, which is not a folder a query can name bare", op, n)
			}
		}
	}
}

// completionGaps lists the commands under root with no argument
// completion and the value flags with no completion function, nil ones
// included.
func completionGaps(root *cobra.Command) []string {
	var gaps []string
	walk(root, func(c *cobra.Command) {
		if c.Name() == "help" {
			return
		}
		if c.ValidArgsFunction == nil {
			gaps = append(gaps, c.CommandPath()+": no completion for its arguments")
		}
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name == "help" || f.Value.Type() == "bool" {
				return
			}
			if fn, ok := c.GetFlagCompletionFunc(f.Name); !ok || fn == nil {
				gaps = append(gaps, c.CommandPath()+" --"+f.Name+": no completion")
			}
		})
	})
	return gaps
}

func TestCompletionGapsCatchesANilFlagHandler(t *testing.T) {
	t.Parallel()
	c := &cobra.Command{Use: "throwaway", ValidArgsFunction: noComplete}
	c.Flags().String("name", "", "")
	if err := c.RegisterFlagCompletionFunc("name", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := completionGaps(c), []string{"throwaway --name: no completion"}; !slices.Equal(got, want) {
		t.Errorf("gaps %q, want %q", got, want)
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
	walk(root, func(c *cobra.Command) { paths[c.CommandPath()] = true })
	for _, gap := range completionGaps(root) {
		t.Error(gap)
	}
	cp := &completer{env: env, g: &globals{}}
	for path := range cp.specs() {
		if !paths[path] {
			t.Errorf("specs has %q, which is not a command", path)
		}
	}
	if !paths["chainmail search"] || !paths["chainmail watch groups delete"] {
		t.Fatal("the walk did not reach every level of the tree")
	}
}
