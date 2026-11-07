package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func runDocs(t *testing.T) (string, cliResult) {
	t.Helper()
	dir := t.TempDir()
	return dir, runCLI(t, Env{}, "docs", dir)
}

func readPage(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

func TestDocsWritesAPagePerVisibleCommand(t *testing.T) {
	t.Parallel()
	dir, got := runDocs(t)
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %s", got.code, got.stderr)
	}
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

func TestDocsPageHasFrontMatterUsageAndLinks(t *testing.T) {
	t.Parallel()
	dir, _ := runDocs(t)
	page := readPage(t, dir, "chainmail-accounts.md")
	for _, want := range []string{
		"---\ntitle: chainmail accounts\n",
		"sidebar_label: accounts\n",
		"\n## Usage\n\n```text\nchainmail accounts",
		"(./chainmail-accounts-test.md)",
		"(./chainmail.md)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q:\n%s", want, page)
		}
	}
}

func TestDocsExamplesAreDedented(t *testing.T) {
	t.Parallel()
	dir, _ := runDocs(t)
	page := readPage(t, dir, "chainmail-accounts-test.md")
	if want := "## Examples\n\n```sh\nchainmail accounts test work\n```\n"; !strings.Contains(page, want) {
		t.Errorf("page lacks %q:\n%s", want, page)
	}
}

func TestDocsEscapesAngleBracketsInProse(t *testing.T) {
	t.Parallel()
	dir, _ := runDocs(t)
	// item's Long text names the search term item:<id>.
	page := readPage(t, dir, "chainmail-item.md")
	prose, _, _ := strings.Cut(page, "## Usage")
	if strings.Contains(prose, "<id>") {
		t.Errorf("raw angle brackets in prose:\n%s", prose)
	}
	if !strings.Contains(prose, "item:&lt;id&gt;") {
		t.Errorf("prose lacks the escaped item:&lt;id&gt;:\n%s", prose)
	}
}

func TestDocsTurnsIndentedHelpIntoCodeBlocks(t *testing.T) {
	t.Parallel()
	dir, _ := runDocs(t)
	// search's Long text holds the query table and an example, each
	// indented by two spaces, which CommonMark would run into a paragraph.
	page := readPage(t, dir, "chainmail-search.md")
	for _, want := range []string{
		"group, and values with spaces are quoted.\n\n```text\nword, \"a phrase\"              text anywhere",
		"                              linked to no item. Only the cache answers\n",
		"so it is not read as a flag:\n\n```text\nchainmail search -- invoice -from:acme\n```\n",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q:\n%s", want, page)
		}
	}
}

func TestDocsEscapesMarkdownInProse(t *testing.T) {
	t.Parallel()
	dir, _ := runDocs(t)
	// send's Long text shows Markdown image syntax, which must stay text.
	page := readPage(t, dir, "chainmail-send.md")
	if want := `as in !\[chart\](chart.png) with`; !strings.Contains(page, want) {
		t.Errorf("page lacks %q:\n%s", want, page)
	}
	root := readPage(t, dir, "chainmail.md")
	if want := `Star messages: add \\Flagged`; !strings.Contains(root, want) {
		t.Errorf("root page lacks %q:\n%s", want, root)
	}
}

// sidebarPosition matches a page's sidebar_position front matter line.
var sidebarPosition = regexp.MustCompile(`(?m)^sidebar_position: (\d+)$`)

func TestDocsSidebarPositionsFollowTheTree(t *testing.T) {
	t.Parallel()
	dir, _ := runDocs(t)
	pages, err := filepath.Glob(filepath.Join(dir, "chainmail*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) == 0 {
		t.Fatal("no pages written")
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
	order := []string{"chainmail.md", "chainmail-accounts.md", "chainmail-accounts-test.md", "chainmail-admin.md"}
	for i := 1; i < len(order); i++ {
		if pos[order[i-1]] >= pos[order[i]] {
			t.Errorf("%s (%d) is not before %s (%d)", order[i-1], pos[order[i-1]], order[i], pos[order[i]])
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
	if got := runCLI(t, Env{}, "docs", dir); got.code != 0 {
		t.Fatalf("exit %d, stderr %s", got.code, got.stderr)
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
	if got := runCLI(t, Env{}, "docs"); got.code == 0 {
		t.Error("docs with no directory succeeded")
	}
}
