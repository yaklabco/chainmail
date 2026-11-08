//go:build stave

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo is a clone with an origin it can push to, both in a temp dir.
type gitRepo struct {
	t      *testing.T
	dir    string
	origin string
}

// newGitRepo makes a bare origin with one commit on main and a clone of it.
func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	// checkReleasable runs git with this process's environment, so keep the
	// user's global and system git config out of every guard test.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	r := &gitRepo{t: t, dir: filepath.Join(root, "clone"), origin: filepath.Join(root, "origin.git")}
	r.git(root, "init", "-q", "--bare", "-b", "main", r.origin)
	r.git(root, "clone", "-q", r.origin, r.dir)
	r.git(r.dir, "checkout", "-q", "-b", "main")
	r.commit("first")
	r.git(r.dir, "push", "-q", "origin", "main")
	return r
}

func (r *gitRepo) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *gitRepo) commit(msg string) {
	r.t.Helper()
	path := filepath.Join(r.dir, "file.txt")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := f.WriteString(msg + "\n"); err != nil {
		r.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		r.t.Fatal(err)
	}
	r.git(r.dir, "add", "file.txt")
	r.git(r.dir, "commit", "-q", "-m", msg)
}

func TestCheckReleasableAcceptsCleanMainWithNewTag(t *testing.T) {
	r := newGitRepo(t)
	if err := checkReleasable(r.dir, "v0.1.0"); err != nil {
		t.Fatalf("checkReleasable: %v", err)
	}
}

func TestCheckReleasableRefusesLocalTag(t *testing.T) {
	r := newGitRepo(t)
	r.git(r.dir, "tag", "v0.1.0")
	err := checkReleasable(r.dir, "v0.1.0")
	if !errors.Is(err, errTagExists) {
		t.Fatalf("err = %v, want errTagExists", err)
	}
	if !strings.Contains(err.Error(), "v0.1.0") || !strings.Contains(err.Error(), "locally") {
		t.Errorf("error %q does not name the tag and where it exists", err)
	}
}

func TestCheckReleasableRefusesRemoteTag(t *testing.T) {
	r := newGitRepo(t)
	r.git(r.dir, "tag", "v0.1.0")
	r.git(r.dir, "push", "-q", "origin", "v0.1.0")
	r.git(r.dir, "tag", "-d", "v0.1.0")
	err := checkReleasable(r.dir, "v0.1.0")
	if !errors.Is(err, errTagExists) {
		t.Fatalf("err = %v, want errTagExists", err)
	}
	if !strings.Contains(err.Error(), "v0.1.0") || !strings.Contains(err.Error(), "origin") {
		t.Errorf("error %q does not name the tag and the remote", err)
	}
}

func TestCheckReleasableIgnoresOtherTags(t *testing.T) {
	r := newGitRepo(t)
	r.git(r.dir, "tag", "v0.1.0")
	r.git(r.dir, "push", "-q", "origin", "v0.1.0")
	r.commit("second")
	r.git(r.dir, "push", "-q", "origin", "main")
	if err := checkReleasable(r.dir, "v0.1.1"); err != nil {
		t.Fatalf("checkReleasable: %v", err)
	}
}

func TestCheckReleasableRefusesModifiedFile(t *testing.T) {
	r := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(r.dir, "file.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkReleasable(r.dir, "v0.1.0")
	if !errors.Is(err, errDirtyTree) {
		t.Fatalf("err = %v, want errDirtyTree", err)
	}
	if !strings.Contains(err.Error(), "\n M file.txt") {
		t.Errorf("error %q does not list the changed file as git status shows it", err)
	}
}

func TestCheckReleasableRefusesUntrackedFile(t *testing.T) {
	r := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(r.dir, "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkReleasable(r.dir, "v0.1.0"); !errors.Is(err, errDirtyTree) {
		t.Fatalf("err = %v, want errDirtyTree", err)
	}
}

// A config that hides untracked files from git status must not hide them
// from the release guard, since an untracked .go file is built into the
// published binary.
func TestCheckReleasableRefusesUntrackedFileWhenStatusHidesThem(t *testing.T) {
	r := newGitRepo(t)
	r.git(r.dir, "config", "status.showUntrackedFiles", "no")
	if err := os.MkdirAll(filepath.Join(r.dir, "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "pkg", "new.go"), []byte("package pkg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkReleasable(r.dir, "v0.1.0")
	if !errors.Is(err, errDirtyTree) {
		t.Fatalf("err = %v, want errDirtyTree", err)
	}
	if !strings.Contains(err.Error(), "pkg/new.go") {
		t.Errorf("error %q does not name the untracked file", err)
	}
}

func TestCheckReleasableRefusesHeadAheadOfOriginMain(t *testing.T) {
	r := newGitRepo(t)
	r.commit("unpushed")
	err := checkReleasable(r.dir, "v0.1.0")
	if !errors.Is(err, errNotOriginMain) {
		t.Fatalf("err = %v, want errNotOriginMain", err)
	}
}

func TestCheckReleasableRefusesHeadBehindOriginMain(t *testing.T) {
	r := newGitRepo(t)
	other := filepath.Join(t.TempDir(), "other")
	r.git(r.dir, "clone", "-q", r.origin, other)
	r2 := &gitRepo{t: t, dir: other, origin: r.origin}
	r2.commit("pushed elsewhere")
	r2.git(other, "push", "-q", "origin", "main")
	if err := checkReleasable(r.dir, "v0.1.0"); !errors.Is(err, errNotOriginMain) {
		t.Fatalf("err = %v, want errNotOriginMain", err)
	}
}

func TestCheckReleasableRefusesOtherBranch(t *testing.T) {
	r := newGitRepo(t)
	r.git(r.dir, "checkout", "-q", "-b", "feature")
	r.commit("feature work")
	if err := checkReleasable(r.dir, "v0.1.0"); !errors.Is(err, errNotOriginMain) {
		t.Fatalf("err = %v, want errNotOriginMain", err)
	}
}

func TestReleaseTag(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0.1.0", "v0.1.0"},
		{"v0.1.0", "v0.1.0"},
		{" 1.2.3\n", "v1.2.3"},
	} {
		if got := releaseTag(tc.in); got != tc.want {
			t.Errorf("releaseTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

const sampleChangelog = `# Changelog

All notable changes to this project are documented in this file.

## [Unreleased]

## [0.2.0] - 2026-12-01

### Added

- Second thing.

## [0.1.0] - 2026-11-06

### Added

- First thing.

[Unreleased]: https://github.com/yaklabco/chainmail/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/yaklabco/chainmail/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/yaklabco/chainmail/releases/tag/v0.1.0
`

func writeSampleChangelog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(path, []byte(sampleChangelog), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWriteReleaseNotesWritesThatVersionsSection(t *testing.T) {
	path := writeSampleChangelog(t)
	out := filepath.Join(t.TempDir(), "cache", "release-notes.md")
	if err := writeReleaseNotes(path, out, "v0.1.0"); err != nil {
		t.Fatalf("writeReleaseNotes: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "## [0.1.0] - 2026-11-06\n\n### Added\n\n- First thing.\n"
	if string(got) != want {
		t.Errorf("release notes =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteReleaseNotesRefusesMissingVersion(t *testing.T) {
	path := writeSampleChangelog(t)
	out := filepath.Join(t.TempDir(), "release-notes.md")
	err := writeReleaseNotes(path, out, "v0.3.0")
	if err == nil {
		t.Fatal("writeReleaseNotes succeeded for a version the changelog lacks")
	}
	if !strings.Contains(err.Error(), "0.3.0") || !strings.Contains(err.Error(), "CHANGELOG.md") {
		t.Errorf("error %q does not name the version and the changelog", err)
	}
}

func TestWriteReleaseNotesRefusesMissingChangelog(t *testing.T) {
	dir := t.TempDir()
	err := writeReleaseNotes(filepath.Join(dir, "CHANGELOG.md"), filepath.Join(dir, "notes.md"), "v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "CHANGELOG.md") {
		t.Fatalf("err = %v, want an error naming CHANGELOG.md", err)
	}
}

func TestWriteSnapshotNotesUsesNextVersionWhenPresent(t *testing.T) {
	path := writeSampleChangelog(t)
	out := filepath.Join(t.TempDir(), "release-notes.md")
	if err := writeSnapshotNotes(path, out, "v0.1.0"); err != nil {
		t.Fatalf("writeSnapshotNotes: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "## [0.1.0]") {
		t.Errorf("snapshot notes start %q, want the 0.1.0 section", got)
	}
}

func TestWriteSnapshotNotesFallsBackToLatestRelease(t *testing.T) {
	path := writeSampleChangelog(t)
	out := filepath.Join(t.TempDir(), "release-notes.md")
	if err := writeSnapshotNotes(path, out, "v0.2.1"); err != nil {
		t.Fatalf("writeSnapshotNotes: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "## [0.2.0]") {
		t.Errorf("snapshot notes start %q, want the latest release section, 0.2.0", got)
	}
}

func TestValidateChangelogRefusesMissingFile(t *testing.T) {
	err := validateChangelog(filepath.Join(t.TempDir(), "CHANGELOG.md"))
	if err == nil || !strings.Contains(err.Error(), "CHANGELOG.md") {
		t.Fatalf("err = %v, want an error naming CHANGELOG.md", err)
	}
}

func TestValidateChangelogAcceptsValidFile(t *testing.T) {
	if err := validateChangelog(writeSampleChangelog(t)); err != nil {
		t.Fatalf("validateChangelog: %v", err)
	}
}

func TestValidateChangelogRejectsInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(path, []byte("no title here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateChangelog(path); err == nil {
		t.Fatal("validateChangelog accepted a changelog with no title")
	}
}

func TestGoreleaserParallelism(t *testing.T) {
	t.Setenv("STAVE_NUM_PROCESSORS", "3")
	if got := goreleaserParallelism(); got != "3" {
		t.Errorf("with STAVE_NUM_PROCESSORS=3, goreleaserParallelism() = %q", got)
	}
	t.Setenv("STAVE_NUM_PROCESSORS", "")
	if got := goreleaserParallelism(); got != parallelism() {
		t.Errorf("without STAVE_NUM_PROCESSORS, goreleaserParallelism() = %q, want %q", got, parallelism())
	}
}

func TestCheckReleaseEnv(t *testing.T) {
	for _, tc := range []struct {
		name, github, tap string
		missing           []string
	}{
		{name: "both set", github: "gh-token", tap: "tap-token"},
		{name: "no GitHub token", tap: "tap-token", missing: []string{"GITHUB_TOKEN"}},
		{name: "no tap token", github: "gh-token", missing: []string{"HOMEBREW_TAP_TOKEN"}},
		{name: "neither", missing: []string{"GITHUB_TOKEN", "HOMEBREW_TAP_TOKEN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tc.github)
			t.Setenv("HOMEBREW_TAP_TOKEN", tc.tap)
			err := checkReleaseEnv()
			if len(tc.missing) == 0 {
				if err != nil {
					t.Fatalf("checkReleaseEnv: %v", err)
				}
				return
			}
			if !errors.Is(err, errMissingToken) {
				t.Fatalf("err = %v, want errMissingToken", err)
			}
			for _, name := range tc.missing {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %q does not name %s", err, name)
				}
			}
		})
	}
}

// TestCalDAVTestArgsCapParallelism: the CalDAV suite runs with the same
// -p and -parallel caps as the integration suite.
func TestCalDAVTestArgsCapParallelism(t *testing.T) {
	args := strings.Join(calDAVTestArgs(), " ")
	for _, want := range []string{"-p " + parallelism(), "-parallel " + parallelism(), "-tags caldav", "-timeout 15m"} {
		if !strings.Contains(" "+args+" ", " "+want+" ") {
			t.Errorf("go %s lacks %q", args, want)
		}
	}
}
