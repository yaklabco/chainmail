//go:build stave

package main

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/yaklabco/stave/pkg/changelog"
	"github.com/yaklabco/stave/pkg/sh"
	"github.com/yaklabco/stave/pkg/st"
)

// Default runs the full local check.
var Default = Check

// Prereq groups the prerequisite targets
type Prereq st.Namespace

// Default is the default target for the prereq namespace.
func (Prereq) Default() error {
	st.Deps(Prereq.All)

	return nil
}

// All runs all prerequisite targets.
func (Prereq) All() error {
	st.Deps(Prereq.Brew)

	return nil
}

// Brew installs tools from Brewfile via Homebrew
func (Prereq) Brew() error {
	return sh.Run("brew", "bundle", "--file=Brewfile")
}

// Check runs everything CI runs: generated code check, lint, changelog
// validation, tests and build.
func Check() {
	st.Deps(Prereq.All)
	st.SerialDeps(GenerateCheck, Lint, Changelog, Test.Unit, Test.Stavefile, Build)
}

// Generate regenerates Protobuf and Connect code into gen/.
func Generate() error {
	st.Deps(Prereq.All)

	return sh.RunV("buf", "generate")
}

// GenerateCheck fails if gen/ is out of date with proto/, or the command
// reference with the CLI's help.
func GenerateCheck() error {
	st.Deps(Prereq.All)

	// Serial: buf generate empties gen/ before it rewrites it, and the
	// chainmail build behind Docs.Commands imports gen/.
	st.SerialDeps(Generate, Docs.Commands)
	paths := []string{"gen", commandDocsDir}
	if err := sh.RunV("git", append([]string{"diff", "--exit-code", "--"}, paths...)...); err != nil {
		return err
	}
	untracked, err := sh.Output("git", append([]string{"status", "--porcelain", "--"}, paths...)...)
	if err != nil {
		return err
	}
	if untracked != "" {
		return errors.New("generated files not committed:\n" + untracked)
	}
	return nil
}

// Docs groups the manual's targets.
type Docs st.Namespace

// commandDocsDir is where the generated command reference lives.
const commandDocsDir = "website/docs/reference/commands"

// Commands writes the command reference pages from the CLI's help.
func (Docs) Commands() error {
	st.Deps(Prereq.All)

	return sh.RunV("go", "run", "./cmd/chainmail", "docs", commandDocsDir)
}

// Build builds the manual into website/build, for serving under /docs/.
func (Docs) Build() error {
	st.Deps(Prereq.All)

	return bun("website", "build")
}

// Serve runs the manual locally with live reload at http://localhost:3000/docs/.
func (Docs) Serve() error {
	st.Deps(Prereq.All)

	return bun("website", "start")
}

// Site groups the landing page's targets.
type Site st.Namespace

// Build builds the landing page into site/dist.
func (Site) Build() error {
	st.Deps(Prereq.All)

	return bun("site", "build")
}

// Serve runs the landing page locally with live reload at http://localhost:4321/.
func (Site) Serve() error {
	st.Deps(Prereq.All)

	return bun("site", "dev")
}

// Pages builds the landing page and the manual into pages-dist, as CI deploys them.
func Pages() error {
	st.Deps(Prereq.All)

	return sh.RunV("scripts/build-pages.sh")
}

// bun installs dir's locked dependencies, then runs one of its package scripts.
func bun(dir, script string) error {
	if err := sh.RunWithV(nil, dir, "bun", "install", "--frozen-lockfile"); err != nil {
		return err
	}
	return sh.RunWithV(nil, dir, "bun", "run", script)
}

// Lint runs buf lint and golangci-lint.
func Lint() error {
	st.Deps(Prereq.All)

	if err := sh.RunV("buf", "lint"); err != nil {
		return err
	}
	return sh.RunV("golangci-lint", "run", "./...")
}

// Test groups the test suites.
type Test st.Namespace

// Unit runs unit and component tests with the race detector.
func (Test) Unit() error {
	st.Deps(Prereq.All)

	return sh.RunV("go", "test", "-race", "-p", parallelism(), "./...")
}

// Stavefile runs the tests of this stavefile's own logic, such as the checks
// Release makes before it tags. The stave build tag keeps them out of
// Test.Unit.
func (Test) Stavefile() error {
	st.Deps(Prereq.All)

	return sh.RunV("go", "test", "-race", "-tags", "stave", "-count=1", ".")
}

// Live runs the smoke test against real accounts through the daemon at
// $CHAINMAIL_SOCKET. The write tests wait minutes at a time for Gmail's
// search to catch up, so a full run takes far longer than go test's default
// 10 minute timeout.
func (Test) Live() error {
	st.Deps(Prereq.All)

	return sh.RunV("go", "test", "-tags", "live", "-count=1", "-timeout", "120m", "-v", "./internal/live/")
}

// Integration runs the integration suite: Dovecot in a Docker container and
// the fake Gmail server, plus the daemon's held-send test against Dovecot and
// the MCP server run as chainmail mcp against Dovecot. Each test binary
// starts and removes its own container.
func (Test) Integration() error {
	st.Deps(Prereq.All)

	return sh.RunV("go", "test", "-race", "-tags", "integration", "-count=1", "-timeout", "15m",
		"-p", parallelism(), "-parallel", parallelism(), "./internal/mailstore/imapstore/", "./internal/daemon/", "./internal/mcpserver/")
}

// Build compiles bin/chainmail without cgo.
func Build() error {
	st.Deps(Prereq.All)

	return sh.RunWithV(map[string]string{"CGO_ENABLED": "0"}, "",
		"go", "build", "-trimpath", "-ldflags", ldflags(), "-o", "bin/chainmail", "./cmd/chainmail")
}

// parallelism caps go test -p and -parallel at a quarter of the cores, as the
// machine rules require.
func parallelism() string {
	return strconv.Itoa(max(1, runtime.NumCPU()/4))
}

func ldflags() string {
	v, err := sh.Output("git", "describe", "--tags", "--always", "--dirty")
	if err != nil || strings.TrimSpace(v) == "" {
		v = "dev"
	}
	return "-s -w -X github.com/yaklabco/chainmail/internal/version.Version=" + strings.TrimSpace(v)
}

// Release file names. .buildcache/ and dist/ are build outputs that
// .gitignore keeps out of the tree, so writing to them never makes it dirty.
const (
	changelogFile    = "CHANGELOG.md"
	releaseNotesFile = ".buildcache/release-notes.md"
)

// The reasons Release refuses to run.
var (
	errTagExists          = errors.New("release tag already exists")
	errDirtyTree          = errors.New("working tree is not clean")
	errNotOriginMain      = errors.New("HEAD is not origin/main")
	errNoChangelogSection = errors.New("changelog has no section for the version")
	errMissingToken       = errors.New("release token not set")
)

// Changelog validates CHANGELOG.md against the Keep a Changelog format.
func Changelog() error {
	st.Deps(Prereq.All)

	if err := validateChangelog(changelogFile); err != nil {
		return err
	}
	fmt.Println(changelogFile + " is valid")
	return nil
}

// Release tags the next version and publishes it with goreleaser: the GitHub
// release and the Homebrew Cask. The version comes from the conventional
// commits since the last tag and must have a section in CHANGELOG.md, which
// becomes the release notes. It needs GITHUB_TOKEN and HOMEBREW_TAP_TOKEN.
//
// Every check runs before the tag is created. A pushed tag whose release
// goreleaser cannot publish blocks every later run until it is deleted.
func Release() error {
	st.Deps(Prereq.All)

	if err := checkReleaseEnv(); err != nil {
		return err
	}
	tag, err := nextReleaseTag()
	if err != nil {
		return err
	}
	if err := checkReleasable(".", tag); err != nil {
		return err
	}
	if err := writeReleaseNotes(changelogFile, releaseNotesFile, tag); err != nil {
		return err
	}
	// This also fails when goreleaser is not on PATH.
	if err := sh.RunV("goreleaser", "check"); err != nil {
		return fmt.Errorf("goreleaser check: %w", err)
	}
	if err := sh.RunV("git", "tag", "-a", tag, "-m", "chainmail "+tag); err != nil {
		return fmt.Errorf("creating tag %s: %w", tag, err)
	}
	if err := sh.RunV("git", "push", "origin", "refs/tags/"+tag); err != nil {
		return fmt.Errorf("pushing tag %s: %w", tag, err)
	}
	// Without GORELEASER_CURRENT_TAG goreleaser picks the highest tag at
	// HEAD, which need not be the one just made.
	return sh.RunWithV(map[string]string{"GORELEASER_CURRENT_TAG": tag}, "",
		"goreleaser", "release", "--clean",
		"--release-notes", releaseNotesFile, "--parallelism", goreleaserParallelism())
}

// releaseTokens are the variables goreleaser publishes with: GITHUB_TOKEN
// for the GitHub release and HOMEBREW_TAP_TOKEN for the Cask.
var releaseTokens = []string{"GITHUB_TOKEN", "HOMEBREW_TAP_TOKEN"}

// checkReleaseEnv refuses a release when a token goreleaser needs is empty.
// goreleaser reads HOMEBREW_TAP_TOKEN only when it publishes the Cask, after
// the GitHub release exists, so the check cannot be left to goreleaser.
func checkReleaseEnv() error {
	var missing []string
	for _, name := range releaseTokens {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: set %s before releasing", errMissingToken, strings.Join(missing, " and "))
	}
	return nil
}

// Snapshot builds the release archives and the Cask into dist/ as Release
// would, without tagging, pushing or publishing anything.
func Snapshot() error {
	st.Deps(Prereq.All)

	tag, err := nextReleaseTag()
	if err != nil {
		return err
	}
	if err := writeSnapshotNotes(changelogFile, releaseNotesFile, tag); err != nil {
		return err
	}
	// .goreleaser.yaml names the snapshot after this version, so the archives
	// carry the version that the notes describe.
	env := map[string]string{"CHAINMAIL_SNAPSHOT_VERSION": strings.TrimPrefix(tag, "v")}
	return sh.RunWithV(env, "", "goreleaser", "release", "--snapshot", "--clean", "--skip=publish",
		"--release-notes", releaseNotesFile, "--parallelism", goreleaserParallelism())
}

// nextReleaseTag computes the next version from the git history, as stave's
// changelog package does, configured by .svu.yml.
func nextReleaseTag() (string, error) {
	next, err := changelog.NextTag()
	if err != nil {
		return "", fmt.Errorf("computing the next version: %w", err)
	}
	return releaseTag(next), nil
}

// releaseTag turns a version into its tag name, v followed by the version.
func releaseTag(version string) string {
	return "v" + strings.TrimPrefix(strings.TrimSpace(version), "v")
}

// checkReleasable refuses a release of tag from the repository in dir when
// the tag already exists locally or on origin, when the working tree has
// changes or untracked files, or when HEAD is not the commit at the tip of
// main on origin.
func checkReleasable(dir, tag string) error {
	// Only line ends are trimmed: git status --porcelain starts a line with
	// a space for a change that is not staged.
	git := func(args ...string) (string, error) {
		out, err := sh.Output("git", append([]string{"-C", dir}, args...)...)
		if err != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return strings.TrimRight(out, "\r\n"), nil
	}

	local, err := git("tag", "--list", tag)
	if err != nil {
		return err
	}
	if local != "" {
		return fmt.Errorf("%w: %s exists locally", errTagExists, tag)
	}
	remote, err := git("ls-remote", "--tags", "origin", "refs/tags/"+tag)
	if err != nil {
		return err
	}
	if remote != "" {
		return fmt.Errorf("%w: %s exists on origin", errTagExists, tag)
	}

	// --untracked-files=all overrides a status.showUntrackedFiles setting
	// that would hide untracked files, which the release build would include.
	status, err := git("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("%w, commit or remove these first:\n%s", errDirtyTree, status)
	}

	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	mainRef, err := git("ls-remote", "origin", "refs/heads/main")
	if err != nil {
		return err
	}
	mainSHA, _, _ := strings.Cut(mainRef, "\t")
	if mainSHA == "" {
		return fmt.Errorf("%w: origin has no main branch", errNotOriginMain)
	}
	if head != mainSHA {
		return fmt.Errorf("%w: HEAD is %s and origin/main is %s, release from an up-to-date main", errNotOriginMain, head, mainSHA)
	}
	return nil
}

// writeReleaseNotes writes the section of the changelog at path for the
// version in tag to out, and fails when the changelog has no such section.
func writeReleaseNotes(path, out, tag string) error {
	cl, err := parseChangelog(path)
	if err != nil {
		return err
	}
	version := strings.TrimPrefix(tag, "v")
	if !cl.HasVersion(version) {
		return fmt.Errorf("%w: %s has no [%s] section, add it before releasing %s", errNoChangelogSection, path, version, tag)
	}
	return extractSection(path, out, version)
}

// writeSnapshotNotes writes the release notes for a snapshot. A snapshot is
// built on branches whose next version has no changelog section yet, so it
// falls back to the latest released section.
func writeSnapshotNotes(path, out, tag string) error {
	err := writeReleaseNotes(path, out, tag)
	if !errors.Is(err, errNoChangelogSection) {
		return err
	}
	fmt.Printf("%s has no section for %s, using its latest release for the snapshot notes\n", path, tag)
	return extractSection(path, out, "")
}

func parseChangelog(path string) (*changelog.Changelog, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the changelog: %w", err)
	}
	cl, err := changelog.Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cl, nil
}

// extractSection writes one section of the changelog to out, creating its
// directory. An empty version means the latest release.
func extractSection(path, out, version string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(out), err)
	}
	if err := changelog.ExtractSection(path, out, version); err != nil {
		return fmt.Errorf("writing release notes from %s: %w", path, err)
	}
	return nil
}

// validateChangelog validates the changelog at path. A missing file is an
// error, since Release takes its notes from the changelog.
func validateChangelog(path string) error {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s not found, Release takes its notes from it", path)
	}
	if err := changelog.ValidateFile(path); err != nil {
		return fmt.Errorf("changelog validation failed: %w", err)
	}
	return nil
}

// goreleaserParallelism is STAVE_NUM_PROCESSORS when it is set, otherwise a
// quarter of the cores.
func goreleaserParallelism() string {
	return cmp.Or(os.Getenv("STAVE_NUM_PROCESSORS"), parallelism())
}
