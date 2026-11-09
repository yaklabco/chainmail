package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// completeArgs runs chainmail __complete args and returns its candidate
// lines ("value" or "value\tdescription") and its directive.
func completeArgs(t *testing.T, env Env, args ...string) ([]string, cobra.ShellCompDirective) {
	t.Helper()
	got := runCLI(t, env, append([]string{cobra.ShellCompRequestCmd}, args...)...)
	if got.code != 0 {
		t.Fatalf("__complete %q: exit %d, stderr %q", args, got.code, got.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(got.stdout, "\n"), "\n")
	last := lines[len(lines)-1]
	d, err := strconv.Atoi(strings.TrimPrefix(last, ":"))
	if err != nil {
		t.Fatalf("no directive line in %q", got.stdout)
	}
	return lines[:len(lines)-1], cobra.ShellCompDirective(d)
}

// values drops the descriptions from candidate lines.
func values(cands []string) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i], _, _ = strings.Cut(c, "\t")
	}
	return out
}

// sockEnv points the CLI at socket through CHAINMAIL_SOCKET.
func sockEnv(socket string) Env {
	return Env{Getenv: vars(map[string]string{"CHAINMAIL_SOCKET": socket})}
}

func TestCompleteStatic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want []string
		dir  cobra.ShellCompDirective
	}{
		{name: "output", args: []string{"version", "--output", ""}, want: []string{"json", "table"}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "output prefix", args: []string{"version", "--output", "t"}, want: []string{"table"}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "shells", args: []string{"completion", ""}, want: []string{"bash", "zsh", "fish", "powershell"}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "kinds", args: []string{"watch", "--kind", ""}, want: []string{"new", "flags", "expunged", "status", "gap", "item", "item_mail", "rule", "item_due_soon", "item_due", "item_overdue", "invite", "event_changed", "event_cancelled", "invite_reply"}, dir: cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace},
		{name: "second kind", args: []string{"watch", "--kind", "new,f"}, want: []string{"new,flags"}, dir: cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace},
		{name: "system flags", args: []string{"flags", "x", "--add", `\S`}, want: []string{`\Seen`}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "audit since", args: []string{"audit", "--since", ""}, want: []string{"1h", "24h", "168h"}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "audit client", args: []string{"audit", "--client", ""}, want: []string{"cli", "mcp", "rule", "daemon"}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "subject", args: []string{"send", "--subject", ""}, want: []string{}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "no positional args", args: []string{"version", ""}, want: []string{}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "attach", args: []string{"send", "--attach", ""}, want: []string{}, dir: cobra.ShellCompDirectiveDefault},
		{name: "state dir", args: []string{"daemon", "--state-dir", ""}, want: []string{}, dir: cobra.ShellCompDirectiveFilterDirs},
		{name: "send-raw stdin", args: []string{"send-raw", ""}, want: []string{"-"}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "search source", args: []string{"search", "--source", ""}, want: []string{"auto", "cache", "server"}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "search source prefix", args: []string{"search", "--source", "s"}, want: []string{"server"}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "cache subcommand args", args: []string{"cache", "evict", ""}, want: []string{}, dir: cobra.ShellCompDirectiveNoFileComp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cands, d := completeArgs(t, Env{}, tt.args...)
			if got := values(cands); !slices.Equal(got, tt.want) {
				t.Errorf("candidates %q, want %q", got, tt.want)
			}
			if d != tt.dir {
				t.Errorf("directive %d, want %d", d, tt.dir)
			}
		})
	}
}

func TestCompleteInlineFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"logo.png", "lorem.txt", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "lots"), 0o700); err != nil {
		t.Fatal(err)
	}
	cands, d := completeArgs(t, Env{}, "send", "--inline", "logo="+dir+"/lo")
	want := []string{"logo=" + dir + "/logo.png", "logo=" + dir + "/lorem.txt", "logo=" + dir + "/lots/"}
	if got := values(cands); !slices.Equal(got, want) {
		t.Errorf("candidates %q, want %q", got, want)
	}
	if d != cobra.ShellCompDirectiveNoFileComp|cobra.ShellCompDirectiveNoSpace {
		t.Errorf("directive %d", d)
	}
	if cands, _ := completeArgs(t, Env{}, "send", "--inline", "lo"); len(cands) != 0 {
		t.Errorf("before = got %q, want none", cands)
	}
}

func TestCompletePassEntries(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	for _, p := range []string{"mail/work.gpg", "mail/home.gpg", "google/oauth-client.gpg", ".gpg-id"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(store, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store, p), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := Env{Getenv: vars(map[string]string{"PASSWORD_STORE_DIR": store})}
	cands, _ := completeArgs(t, env, "secret", "set", "accounts/x", "--from-pass", "mail/")
	if got, want := values(cands), []string{"mail/home", "mail/work"}; !slices.Equal(got, want) {
		t.Errorf("candidates %q, want %q", got, want)
	}
}

func TestCompleteAccounts(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	env := sockEnv(m.socket)
	cands, d := completeArgs(t, env, "send", "--account", "")
	if want := []string{"gmail\tgmail me@gmail.example", "host\thostinger me@example.com"}; !slices.Equal(cands, want) {
		t.Errorf("candidates %q, want %q", cands, want)
	}
	if d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive %d", d)
	}
	if cands, _ := completeArgs(t, env, "accounts", "probe", "host", ""); !slices.Equal(values(cands), []string{"gmail"}) {
		t.Errorf("probe after host: %q, want gmail", cands)
	}
	if cands, _ := completeArgs(t, env, "accounts", "test", "host", ""); len(cands) != 0 {
		t.Errorf("test takes one account, got %q", cands)
	}
}

const oauthCompletionConfig = twoAccounts + `
[oauth_clients.g]
client_id = "client-id"

[accounts.goog]
provider = "gmail"
address = "me@gmail.example"
auth = "oauth2"
oauth_client = "g"
`

func TestCompleteAuthLoginOffersOAuthAccountsOnly(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, oauthCompletionConfig, daemontest.Options{NewStore: offlineStores})
	cands, _ := completeArgs(t, sockEnv(h.Socket), "auth", "login", "")
	if got := values(cands); !slices.Equal(got, []string{"goog"}) {
		t.Errorf("candidates %q, want goog", got)
	}
}

func TestCompleteWithTheDaemonDown(t *testing.T) {
	t.Parallel()
	cands, d := completeArgs(t, sockEnv(filepath.Join(t.TempDir(), "none.sock")), "send", "--account", "")
	if len(cands) != 0 || d != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("candidates %q directive %d, want none and NoFileComp", cands, d)
	}
}

// hangingAccounts never answers ListAccounts until the caller gives up.
type hangingAccounts struct {
	chainmailv1connect.UnimplementedAccountServiceHandler
}

func (hangingAccounts) ListAccounts(ctx context.Context, _ *connect.Request[chainmailv1.ListAccountsRequest]) (*connect.Response[chainmailv1.ListAccountsResponse], error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCompleteGivesUpOnASilentDaemon(t *testing.T) {
	t.Parallel()
	path, handler := chainmailv1connect.NewAccountServiceHandler(hangingAccounts{})
	socket := startFakeServer(t, path, handler)
	start := time.Now()
	cands, _ := completeArgs(t, sockEnv(socket), "send", "--account", "")
	if len(cands) != 0 {
		t.Errorf("candidates %q, want none", cands)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("completion took %v, want under 5s", took)
	}
}

func TestSafeValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want bool
	}{
		{in: "INBOX", want: true},
		{in: "Work Stuff", want: true},
		{in: "", want: false},
		{in: "a\tb", want: false},
		{in: "a\nb", want: false},
		{in: "\x1b[31m", want: false},
		{in: "a\u202eb", want: false},
		{in: " Reports", want: false},
		{in: "Reports ", want: false},
		{in: "\u00a0Reports", want: false},
		{in: "Reports\u3000", want: false},
	}
	for _, tt := range tests {
		if got := safeValue(tt.in); got != tt.want {
			t.Errorf("safeValue(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestCompleteInlineFilesSkipsUnsafeNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"logo.png", "lo\tgo.png", "lo\u202egnp.txt", "lo\nx", "lo trailing "} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cands, _ := completeArgs(t, Env{}, "send", "--inline", "logo="+dir+"/lo")
	if got, want := values(cands), []string{"logo=" + dir + "/logo.png"}; !slices.Equal(got, want) {
		t.Errorf("candidates %q, want %q", got, want)
	}
}

// writePassStore makes a password store holding entries, each a path
// relative to the store, and returns it.
func writePassStore(t *testing.T, entries ...string) string {
	t.Helper()
	store := t.TempDir()
	for _, p := range entries {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(store, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store, p), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestCompletePassEntriesSkipsUnsafeNames(t *testing.T) {
	t.Parallel()
	store := writePassStore(t, "mail/work.gpg", "mail/trail .gpg", "mail/a\tb.gpg", " lead.gpg")
	env := Env{Getenv: vars(map[string]string{"PASSWORD_STORE_DIR": store})}
	cands, _ := completeArgs(t, env, "secret", "set", "accounts/x", "--from-pass", "")
	if got, want := values(cands), []string{"mail/work"}; !slices.Equal(got, want) {
		t.Errorf("candidates %q, want %q", got, want)
	}
}

func TestCompletePassEntriesRelativeStore(t *testing.T) {
	t.Parallel()
	store := writePassStore(t, "mail/work.gpg")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, store)
	if err != nil {
		t.Fatal(err)
	}
	// HOME has a store too, which a relative PASSWORD_STORE_DIR must win over.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".password-store"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".password-store", "home.gpg"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := Env{Getenv: vars(map[string]string{"PASSWORD_STORE_DIR": rel, "HOME": home})}
	cands, _ := completeArgs(t, env, "secret", "set", "accounts/x", "--from-pass", "")
	if got, want := values(cands), []string{"mail/work"}; !slices.Equal(got, want) {
		t.Errorf("relative store %q: candidates %q, want %q", rel, got, want)
	}
}

func TestCompletePassEntriesSymlinkedStore(t *testing.T) {
	t.Parallel()
	store := writePassStore(t, "mail/work.gpg")
	link := filepath.Join(t.TempDir(), "store")
	if err := os.Symlink(store, link); err != nil {
		t.Fatal(err)
	}
	env := Env{Getenv: vars(map[string]string{"PASSWORD_STORE_DIR": link})}
	cands, _ := completeArgs(t, env, "secret", "set", "accounts/x", "--from-pass", "")
	if got, want := values(cands), []string{"mail/work"}; !slices.Equal(got, want) {
		t.Errorf("candidates %q, want %q", got, want)
	}
}

func TestWalkPassStore(t *testing.T) {
	t.Parallel()
	var noise []string
	for i := range 20 {
		noise = append(noise, fmt.Sprintf("aaa/n%02d.gpg", i))
	}
	store := writePassStore(t, append(noise, "mail/work.gpg", "mailbox/x.gpg", "mai/y.gpg")...)

	// Only the root, mail and mail/work.gpg can hold a match for mail/,
	// so pruning finds it within five visits while aaa alone has twenty.
	got := walkPassStore(t.Context(), store, "mail/", passLimits{visits: 5, candidates: 500})
	if want := []string{"mail/work"}; !slices.Equal(got, want) {
		t.Errorf("pruned walk: %q, want %q", got, want)
	}
	got = walkPassStore(t.Context(), store, "ma", passLimits{visits: 100, candidates: 500})
	if want := []string{"mai/y", "mail/work", "mailbox/x"}; !slices.Equal(got, want) {
		t.Errorf("prefix ma: %q, want %q", got, want)
	}
	// Directories are read in the order the filesystem gives, so the cap
	// keeps some three of aaa's entries, sorted.
	got = walkPassStore(t.Context(), store, "aaa/", passLimits{visits: 100, candidates: 3})
	if len(got) != 3 || !slices.IsSorted(got) || !strings.HasPrefix(got[0], "aaa/n") || !strings.HasPrefix(got[2], "aaa/n") {
		t.Errorf("candidate cap: %q, want three sorted aaa/ entries", got)
	}
	got = walkPassStore(t.Context(), store, "", passLimits{visits: 4, candidates: 500})
	if len(got) > 4 {
		t.Errorf("visit cap 4: %d entries", len(got))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := walkPassStore(ctx, store, "", passLimits{visits: 100, candidates: 500}); len(got) != 0 {
		t.Errorf("cancelled walk: %q, want none", got)
	}
}

// A flat directory larger than the visit cap is read only as far as the cap.
func TestWalkPassStoreFlatDirectory(t *testing.T) {
	t.Parallel()
	var files []string
	for i := range 600 {
		files = append(files, fmt.Sprintf("e%04d.gpg", i))
	}
	store := writePassStore(t, files...)
	got := walkPassStore(t.Context(), store, "", passLimits{visits: 50, candidates: 500})
	if len(got) != 50 {
		t.Errorf("flat walk with a 50-visit cap returned %d entries, want 50", len(got))
	}
	got = walkPassStore(t.Context(), store, "", passLimits{visits: 10000, candidates: 1000})
	if len(got) != 600 {
		t.Errorf("uncapped flat walk returned %d entries, want 600", len(got))
	}
}

func TestCompletionTextIsOneCleanLine(t *testing.T) {
	t.Parallel()
	got := completionText("evil\tsub\nject \x1b[31mred\u202e" + strings.Repeat("x", 100))
	if strings.ContainsAny(got, "\t\n\x1b\u202e") {
		t.Errorf("completionText left a control character: %q", got)
	}
	if n := len([]rune(got)); n > 60 {
		t.Errorf("completionText is %d runes, want at most 60", n)
	}
}
