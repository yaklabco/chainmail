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

// Check runs everything CI runs: generated code check, lint, changelog
// validation, tests and build.
func Check() {
	st.SerialDeps(GenerateCheck, Lint, Changelog, Test.Unit, Test.Stavefile, Build)
}

// Generate regenerates Protobuf and Connect code into gen/.
func Generate() error {
	return sh.RunV("buf", "generate")
}

// GenerateCheck fails if gen/ is out of date with proto/.
func GenerateCheck() error {
	st.Deps(Generate)
	if err := sh.RunV("git", "diff", "--exit-code", "--", "gen"); err != nil {
		return err
	}
	untracked, err := sh.Output("git", "status", "--porcelain", "--", "gen")
	if err != nil {
		return err
	}
	if untracked != "" {
		return errors.New("generated files not committed:\n" + untracked)
	}
	return nil
}

// Lint runs buf lint and golangci-lint.
func Lint() error {
	if err := sh.RunV("buf", "lint"); err != nil {
		return err
	}
	return sh.RunV("golangci-lint", "run", "./...")
}

// Test groups the test suites.
type Test st.Namespace

// Unit runs unit and component tests with the race detector.
func (Test) Unit() error {
	return sh.RunV("go", "test", "-race", "-p", parallelism(), "./...")
}

// Stavefile runs the tests of this stavefile's own logic, such as the checks
// Release makes before it tags. The stave build tag keeps them out of
// Test.Unit.
func (Test) Stavefile() error {
	return sh.RunV("go", "test", "-race", "-tags", "stave", "-count=1", ".")
}

// Live runs the smoke test against real accounts through the daemon at
// $CHAINMAIL_SOCKET. The write tests wait minutes at a time for Gmail's
// search to catch up, so a full run takes far longer than go test's default
// 10 minute timeout.
func (Test) Live() error {
	return sh.RunV("go", "test", "-tags", "live", "-count=1", "-timeout", "120m", "-v", "./internal/live/")
}

// Integration runs the integration suite: Dovecot in a Docker container and
// the fake Gmail server, plus the daemon's held-send test against Dovecot and
// the MCP server run as chainmail mcp against Dovecot. Each test binary
// starts and removes its own container.
func (Test) Integration() error {
	return sh.RunV("go", "test", "-race", "-tags", "integration", "-count=1", "-timeout", "15m",
		"-p", parallelism(), "-parallel", parallelism(), "./internal/mailstore/imapstore/", "./internal/daemon/", "./internal/mcpserver/")
}

// Build compiles bin/chainmail without cgo.
func Build() error {
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
)

// Changelog validates CHANGELOG.md against the Keep a Changelog format.
func Changelog() error {
	skipped, err := validateChangelog(changelogFile)
	if err != nil {
		return err
	}
	if skipped {
		fmt.Println(changelogFile + " not found, skipping changelog validation")
		return nil
	}
	fmt.Println(changelogFile + " is valid")
	return nil
}

// Release tags the next version and publishes it with goreleaser: the GitHub
// release and the Homebrew Cask. The version comes from the conventional
// commits since the last tag and must have a section in CHANGELOG.md, which
// becomes the release notes. It needs GITHUB_TOKEN and HOMEBREW_TAP_TOKEN.
func Release() error {
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
	if err := sh.RunV("git", "tag", "-a", tag, "-m", "chainmail "+tag); err != nil {
		return fmt.Errorf("creating tag %s: %w", tag, err)
	}
	if err := sh.RunV("git", "push", "origin", "refs/tags/"+tag); err != nil {
		return fmt.Errorf("pushing tag %s: %w", tag, err)
	}
	return sh.RunV("goreleaser", "release", "--clean",
		"--release-notes", releaseNotesFile, "--parallelism", goreleaserParallelism())
}

// Snapshot builds the release archives and the Cask into dist/ as Release
// would, without tagging, pushing or publishing anything.
func Snapshot() error {
	tag, err := nextReleaseTag()
	if err != nil {
		return err
	}
	if err := writeSnapshotNotes(changelogFile, releaseNotesFile, tag); err != nil {
		return err
	}
	return sh.RunV("goreleaser", "release", "--snapshot", "--clean", "--skip=publish",
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

	status, err := git("status", "--porcelain")
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

// validateChangelog validates the changelog at path. It reports skipped,
// with no error, when there is no file at path.
func validateChangelog(path string) (skipped bool, err error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err := changelog.ValidateFile(path); err != nil {
		return false, fmt.Errorf("changelog validation failed: %w", err)
	}
	return false, nil
}

// goreleaserParallelism is STAVE_NUM_PROCESSORS when it is set, otherwise a
// quarter of the cores.
func goreleaserParallelism() string {
	return cmp.Or(os.Getenv("STAVE_NUM_PROCESSORS"), parallelism())
}
