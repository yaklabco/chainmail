package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runDocs runs chainmail docs into a new directory and returns it, failing
// the test with the command's stderr when it does not succeed.
func runDocs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if r := runCLI(t, Env{}, "docs", dir); r.code != 0 {
		t.Fatalf("exit %d, stderr %s", r.code, r.stderr)
	}
	return dir
}

func readPage(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// fixtureTree is a small command tree whose help text the page tests
// control, so a wording change in the real commands does not break them.
func fixtureTree() *cobra.Command {
	root := &cobra.Command{Use: "chainmail", Short: "Mail for agents"}
	root.PersistentFlags().String("socket", "", "daemon socket")
	send := &cobra.Command{
		Use:   "send <to>",
		Short: "Send a message",
		Long: "Send a message to <to>.\n\nThe body is Markdown:\n\n" +
			"  # heading\n\n  - item\n\nIt is sent at once.",
		Example: "  chainmail send bob@example.com\n  chainmail send --dry-run bob@example.com",
		RunE:    func(*cobra.Command, []string) error { return nil },
	}
	send.Flags().Bool("dry-run", false, "print the message instead")
	draft := &cobra.Command{Use: "draft", Short: "Work with drafts"}
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a draft",
		Long:  "Create a draft with ```fenced``` text.\n\n  ```sh\n  echo hi\n  ```",
		RunE:  func(*cobra.Command, []string) error { return nil },
	}
	hidden := &cobra.Command{Use: "man", Short: "Man pages", Hidden: true, RunE: func(*cobra.Command, []string) error { return nil }}
	draft.AddCommand(create)
	root.AddCommand(send, draft, hidden)
	return root
}

func writeFixtureDocs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := writeCommandDocs(fixtureTree(), dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDocsWritesAPagePerVisibleCommand(t *testing.T) {
	t.Parallel()
	dir := runDocs(t)
	for _, name := range []string{"chainmail.md", "chainmail-search.md", "chainmail-accounts-test.md", "chainmail-item-create.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("no page %s: %v", name, err)
		}
	}
	for _, name := range []string{"chainmail-man.md", "chainmail-docs.md", "chainmail-help.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("hidden command has a page: %s", name)
		}
	}
}

func TestDocsFixturePages(t *testing.T) {
	t.Parallel()
	dir := writeFixtureDocs(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := "chainmail-draft-create.md chainmail-draft.md chainmail-send.md chainmail.md"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("pages = %s, want %s", got, want)
	}
}

func TestDocsPageHasFrontMatterUsageAndLinks(t *testing.T) {
	t.Parallel()
	dir := writeFixtureDocs(t)
	page := readPage(t, dir, "chainmail-draft.md")
	for _, want := range []string{
		"---\ntitle: \"chainmail draft\"\n",
		"sidebar_label: \"draft\"\n",
		"description: \"Work with drafts\"\n",
		"\n## Usage\n\n```text\nchainmail draft",
		"- [chainmail draft create](./chainmail-draft-create.md): Create a draft\n",
		"## See also\n\n- [chainmail](./chainmail.md)\n",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q:\n%s", want, page)
		}
	}
}

func TestDocsSendPage(t *testing.T) {
	t.Parallel()
	dir := writeFixtureDocs(t)
	page := readPage(t, dir, "chainmail-send.md")
	for _, want := range []string{
		"Send a message to &lt;to&gt;.\n",
		"The body is Markdown:\n\n```text\n# heading\n\n- item\n```\n\nIt is sent at once.\n",
		"## Examples\n\n```sh\nchainmail send bob@example.com\nchainmail send --dry-run bob@example.com\n```\n",
		"## Flags\n\n```text\n      --dry-run",
		"## Global flags\n\n```text\n      --socket",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q:\n%s", want, page)
		}
	}
}

func TestDocsFenceOutgrowsBackticksInTheContent(t *testing.T) {
	t.Parallel()
	dir := writeFixtureDocs(t)
	page := readPage(t, dir, "chainmail-draft-create.md")
	if want := "\n````text\n```sh\necho hi\n```\n````\n"; !strings.Contains(page, want) {
		t.Errorf("page lacks %q:\n%s", want, page)
	}
}

func TestDocsRejectsPagesWithTheSameName(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "chainmail"}
	run := func(*cobra.Command, []string) error { return nil }
	send := &cobra.Command{Use: "send", RunE: run}
	send.AddCommand(&cobra.Command{Use: "raw", RunE: run})
	root.AddCommand(send, &cobra.Command{Use: "send-raw", RunE: run})
	dir := t.TempDir()
	old := filepath.Join(dir, "chainmail-old.md")
	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeCommandDocs(root, dir)
	if err == nil || !strings.Contains(err.Error(), "chainmail-send-raw.md") {
		t.Fatalf("err = %v, want one naming chainmail-send-raw.md", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("a failed run removed the existing pages: %v", err)
	}
}

func TestProse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
	}{
		{"plain", "Read a message.", "Read a message."},
		{"html and emphasis", "a & b <id> *x* _y_ `z` [l]", `a &amp; b &lt;id&gt; \*x\* \_y\_ \` + "`z\\`" + ` \[l\]`},
		{"backslash", `add \Flagged`, `add \\Flagged`},
		{"tilde pipe and braces", "a ~b~ | c {d}", `a \~b\~ \| c \{d\}`},
		{"heading", "# not a heading", `\# not a heading`},
		{"plus list", "+ not a list", `\+ not a list`},
		{"dash list", "- not a list", `\- not a list`},
		{"ordered list", "1. not a list", `1\. not a list`},
		{"ordered list paren", "12) not a list", `12\) not a list`},
		{"setext dashes", "Title\n---", "Title\n\\---"},
		{"setext equals", "Title\n===", "Title\n\\==="},
		{"admonition", ":::note", `\:::note`},
		{"number in text", "10 messages", "10 messages"},
		{"indented at line 0", "  a b\n  c\nafter", "```text\na b\nc\n```\n\nafter"},
		{"indented at end", "before\n  a\n  b", "before\n\n```text\na\nb\n```"},
		{"blank line inside a run", "x\n  a\n\n  b\ny", "x\n\n```text\na\n\nb\n```\n\ny"},
		{"blank line ends a run", "x\n  a\n\ny", "x\n\n```text\na\n```\n\ny"},
		{"code is not escaped", "  <id> & *x*", "```text\n<id> & *x*\n```"},
		{"fence in code", "  ```\n  x", "````text\n```\nx\n````"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := prose(tt.in); got != tt.want {
				t.Errorf("prose(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDedent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
	}{
		{"none", "a\n b", "a\n b"},
		{"spaces", "  a\n    b", "a\n  b"},
		{"tabs", "\ta\n\t\tb", "a\n\tb"},
		{"blank lines kept", "  a\n\n  b", "a\n\nb"},
		{"short blank line", "    a\n  \n    b", "a\n\nb"},
		{"mixed tabs and spaces share no indent", "\ta\n  b", "\ta\n  b"},
		{"mixed with a common prefix", "  \ta\n  b", "\ta\nb"},
		{"trailing newline dropped", "  a\n", "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := dedent(tt.in); got != tt.want {
				t.Errorf("dedent(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDocsRealTreeEscapesProse(t *testing.T) {
	t.Parallel()
	dir := runDocs(t)
	// item's Long text names the search term item:<id>.
	page := readPage(t, dir, "chainmail-item.md")
	prose, _, _ := strings.Cut(page, "## Usage")
	if strings.Contains(prose, "<id>") {
		t.Errorf("raw angle brackets in prose:\n%s", prose)
	}
}

// sidebarPosition matches a page's sidebar_position front matter line.
var sidebarPosition = regexp.MustCompile(`(?m)^sidebar_position: (\d+)$`)

func TestDocsSidebarPositionsFollowTheTree(t *testing.T) {
	t.Parallel()
	dir := writeFixtureDocs(t)
	pages, err := filepath.Glob(filepath.Join(dir, "chainmail*.md"))
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	seen := map[int]string{}
	for _, p := range pages {
		name := filepath.Base(p)
		m := sidebarPosition.FindStringSubmatch(readPage(t, dir, name))
		if m == nil {
			t.Fatalf("%s has no sidebar_position", name)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatal(err)
		}
		if other, ok := seen[n]; ok {
			t.Errorf("%s and %s share sidebar_position %d", name, other, n)
		}
		seen[n] = name
		pos[name] = n
	}
	// A parent comes first, then its subcommands, then its next sibling.
	order := []string{"chainmail.md", "chainmail-draft.md", "chainmail-draft-create.md", "chainmail-send.md"}
	for i, name := range order {
		if pos[name] != i {
			t.Errorf("%s is at %d, want %d", name, pos[name], i)
		}
	}
}

func TestDocsRemovesStalePages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stale := filepath.Join(dir, "chainmail-gone.md")
	keep := filepath.Join(dir, "_category_.json")
	for _, p := range []string{stale, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCommandDocs(fixtureTree(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("a page for a command that no longer exists was kept")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("_category_.json was removed: %v", err)
	}
}

func TestDocsIsHiddenAndTakesOneDir(t *testing.T) {
	t.Parallel()
	if got := runCLI(t, Env{}, "--help"); strings.Contains(got.stdout, "\n  docs ") {
		t.Errorf("help lists docs:\n%s", got.stdout)
	}
	if got := runCLI(t, Env{}, "docs"); got.code != 2 {
		t.Errorf("docs with no directory: exit %d, want 2 (stderr %s)", got.code, got.stderr)
	}
}
