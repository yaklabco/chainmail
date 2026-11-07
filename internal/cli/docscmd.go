package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// newDocsCmd writes the command reference of the docs site: one Markdown
// page per visible command. It is hidden: the docs build runs it.
func newDocsCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "docs <dir>",
		Short:  "Write the command reference pages into dir",
		Hidden: true,
		Args:   exactArgs(1),
		// The completion specs in complete.go cover the visible commands.
		// This hidden one only needs a directory.
		ValidArgsFunction: dirs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeCommandDocs(cmd.Root(), args[0])
		},
	}
}

// writeCommandDocs replaces the chainmail*.md pages in dir with one page per
// visible command under root, leaving other files such as _category_.json.
// Pages are numbered in tree order, parents before their subcommands and
// siblings in cobra's alphabetical order, which sets their place in the
// docs sidebar. Every page is rendered before any file changes, so a
// clash of page names leaves dir as it was.
func writeCommandDocs(root *cobra.Command, dir string) error {
	pages := map[string]string{}
	var paths []string
	var render func(c *cobra.Command) error
	render = func(c *cobra.Command) error {
		name := pageName(c)
		if _, ok := pages[name]; ok {
			return fmt.Errorf("two commands have the page %s, one is %s", name, c.CommandPath())
		}
		pages[name] = commandPage(c, len(paths))
		paths = append(paths, name)
		for _, sub := range visible(c) {
			if err := render(sub); err != nil {
				return err
			}
		}
		return nil
	}
	if err := render(root); err != nil {
		return err
	}

	// The pages are public docs that a site build may read as another user.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	old, err := filepath.Glob(filepath.Join(dir, "chainmail*.md"))
	if err != nil {
		return fmt.Errorf("listing the pages in %s: %w", dir, err)
	}
	for _, p := range old {
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("removing %s: %w", p, err)
		}
	}
	for _, name := range paths {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(pages[name]), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}
	return nil
}

// visible is c's subcommands that help lists. IsAvailableCommand is false
// for hidden commands and for the help command.
func visible(c *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, sub := range c.Commands() {
		if sub.IsAvailableCommand() {
			out = append(out, sub)
		}
	}
	return out
}

// pageName is the file name of c's page: its command path with hyphens.
func pageName(c *cobra.Command) string {
	return strings.ReplaceAll(c.CommandPath(), " ", "-") + ".md"
}

// The pages are CommonMark, not MDX: the site's docusaurus.config.ts sets
// markdown.format to 'detect', which parses .md files as CommonMark. MDX
// would reject the HTML comment in each page's header and read braces as
// expressions. Braces are escaped anyway, so that only the header would
// need to change.

// proseEscaper escapes the characters CommonMark, or the GFM extensions the
// site enables, would read as HTML, emphasis, code, a link, a table or a
// strikethrough, so help text renders as the terminal shows it.
var proseEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;",
	`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`,
	"~", `\~`, "|", `\|`, "{", `\{`, "}", `\}`,
)

// blockStart matches the start of a line CommonMark would read as a
// heading, a list item, a setext underline or, in Docusaurus, an
// admonition. Group 1 is a list number, group 2 the character to escape.
var blockStart = regexp.MustCompile(`^(\d*)([#+=:\-]|[.)])`)

// escapeLine escapes a line of prose, including the block syntax its first
// characters would start.
func escapeLine(line string) string {
	m := blockStart.FindStringSubmatch(line)
	if m == nil || (m[1] == "") != strings.ContainsAny(m[2], "#+=:-") {
		return proseEscaper.Replace(line)
	}
	return m[1] + `\` + m[2] + proseEscaper.Replace(line[len(m[0]):])
}

// prose renders help text for a CommonMark page. Each run of indented
// lines, such as a table or an example, becomes a text code block, since
// CommonMark would join lines indented by fewer than four spaces into the
// paragraph. A blank line followed by another indented line stays in the
// run. Every other line is escaped.
func prose(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var b strings.Builder
	for i := 0; i < len(lines); {
		if !indented(lines[i]) {
			b.WriteString(escapeLine(lines[i]) + "\n")
			i++
			continue
		}
		j := i + 1
		for k := j; k < len(lines); k++ {
			if indented(lines[k]) {
				j = k + 1
			} else if strings.TrimSpace(lines[k]) != "" {
				break
			}
		}
		if i > 0 && strings.TrimSpace(lines[i-1]) != "" {
			b.WriteString("\n")
		}
		b.WriteString(codeBlock("text", dedent(strings.Join(lines[i:j], "\n"))))
		if j < len(lines) && strings.TrimSpace(lines[j]) != "" {
			b.WriteString("\n")
		}
		i = j
	}
	return strings.TrimRight(b.String(), "\n")
}

// codeBlock fences code with info as its info string. The fence is one
// backtick longer than the longest run of backticks in code, and at least
// three, so code that holds a fence cannot close the block.
func codeBlock(info, code string) string {
	longest, run := 0, 0
	for _, r := range code {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + info + "\n" + strings.TrimRight(code, "\n") + "\n" + fence + "\n"
}

// indented reports whether line is text that starts with a space or tab.
func indented(line string) bool {
	return strings.TrimSpace(line) != "" && (line[0] == ' ' || line[0] == '\t')
}

// dedent removes the leading whitespace every non-blank line of s shares,
// compared character by character so a tab never matches spaces. Blank
// lines become empty.
func dedent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	prefix, first := "", true
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		ws := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		if first {
			prefix, first = ws, false
			continue
		}
		n := 0
		for n < len(prefix) && n < len(ws) && prefix[n] == ws[n] {
			n++
		}
		prefix = prefix[:n]
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ""
		} else {
			lines[i] = l[len(prefix):]
		}
	}
	return strings.Join(lines, "\n")
}

// commandPage renders c's page, with pos as its place in the sidebar. Go's
// %q escapes are a subset of YAML's double-quoted ones.
func commandPage(c *cobra.Command, pos int) string {
	var b strings.Builder
	label := strings.TrimPrefix(c.CommandPath(), c.Root().Name()+" ")
	fmt.Fprintf(&b, "---\ntitle: %q\nsidebar_label: %q\nsidebar_position: %d\ndescription: %q\n---\n\n",
		c.CommandPath(), label, pos, c.Short)
	b.WriteString("<!-- Generated by `chainmail docs`. Do not edit: change the command's help and run `go tool stave docs:commands`. -->\n\n")
	if c.Long != "" {
		b.WriteString(prose(c.Long) + "\n\n")
	} else {
		b.WriteString(prose(c.Short) + "\n\n")
	}
	b.WriteString("## Usage\n\n" + codeBlock("text", c.UseLine()) + "\n")
	if c.Example != "" {
		b.WriteString("## Examples\n\n" + codeBlock("sh", dedent(c.Example)) + "\n")
	}
	if f := c.NonInheritedFlags().FlagUsages(); f != "" {
		b.WriteString("## Flags\n\n" + codeBlock("text", f) + "\n")
	}
	if f := c.InheritedFlags().FlagUsages(); f != "" {
		b.WriteString("## Global flags\n\n" + codeBlock("text", f) + "\n")
	}
	if subs := visible(c); len(subs) > 0 {
		b.WriteString("## Commands\n\n")
		for _, sub := range subs {
			fmt.Fprintf(&b, "- [%s](./%s): %s\n", sub.CommandPath(), pageName(sub), prose(sub.Short))
		}
		b.WriteString("\n")
	}
	if p := c.Parent(); p != nil {
		fmt.Fprintf(&b, "## See also\n\n- [%s](./%s)\n", p.CommandPath(), pageName(p))
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}
