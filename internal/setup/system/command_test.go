package system

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tree is a fake host root for the command finder: a temp directory whose
// files the test user owns, so the finder is told to trust that user.
type tree struct {
	t    *testing.T
	root string
}

func newTree(t *testing.T) *tree {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &tree{t: t, root: root}
}

// path is p (an absolute host path) inside the tree.
func (tr *tree) path(p string) string { return filepath.Join(tr.root, p) }

// dir makes host directory p with mode.
func (tr *tree) dir(p string, mode os.FileMode) {
	tr.t.Helper()
	full := tr.path(p)
	if err := os.MkdirAll(full, 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		tr.t.Fatal(err)
	}
}

// file writes host file p holding a shell script, with mode, making its
// directory 0755 if missing.
func (tr *tree) file(p, script string, mode os.FileMode) {
	tr.t.Helper()
	full := tr.path(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("#!/bin/sh\n"+script+"\n"), 0o600); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		tr.t.Fatal(err)
	}
}

// symlink makes host path p a symlink to target.
func (tr *tree) symlink(p, target string) {
	tr.t.Helper()
	full := tr.path(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		tr.t.Fatal(err)
	}
}

// finder trusts the test user over the tree, searching dirs (host paths).
func (tr *tree) finder(dirs ...string) finder {
	if len(dirs) == 0 {
		dirs = trustedDirs
	}
	uid, _ := me()
	return finder{root: tr.root, dirs: dirs, owner: uid}
}

func TestTrustedDirs(t *testing.T) {
	t.Parallel()
	want := []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"}
	if !slices.Equal(trustedDirs, want) {
		t.Fatalf("trustedDirs = %v, want %v", trustedDirs, want)
	}
	if !slices.Equal(commandEnv, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}) {
		t.Fatalf("commandEnv = %v", commandEnv)
	}
	if hostFinder.root != "/" || hostFinder.owner != 0 || !slices.Equal(hostFinder.dirs, want) {
		t.Fatalf("hostFinder = %+v, want root /, owner 0, dirs %v", hostFinder, want)
	}
}

func TestFinder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(tr *tree)
		dirs    []string
		command string
		want    string // host path the finder returns; "" means an error
		errHas  string
	}{
		{
			name:    "first trusted directory wins",
			setup:   func(tr *tree) { tr.file("/usr/sbin/useradd", "", 0o755); tr.file("/usr/bin/useradd", "", 0o755) },
			command: "useradd",
			want:    "/usr/sbin/useradd",
		},
		{
			name:    "later directory",
			setup:   func(tr *tree) { tr.dir("/usr/sbin", 0o755); tr.file("/bin/id", "", 0o755) },
			command: "id",
			want:    "/bin/id",
		},
		{
			name:    "missing everywhere",
			setup:   func(tr *tree) { tr.dir("/usr/bin", 0o755) },
			command: "systemctl",
			errHas:  "not found",
		},
		{
			name:    "group-writable file is skipped",
			setup:   func(tr *tree) { tr.file("/usr/sbin/id", "", 0o775); tr.file("/usr/bin/id", "", 0o755) },
			command: "id",
			want:    "/usr/bin/id",
		},
		{
			name:    "other-writable file is refused",
			setup:   func(tr *tree) { tr.file("/usr/bin/id", "", 0o757) },
			command: "id",
			errHas:  "writable",
		},
		{
			name:    "group-writable directory is refused",
			setup:   func(tr *tree) { tr.file("/usr/bin/id", "", 0o755); tr.dir("/usr/bin", 0o775) },
			command: "id",
			errHas:  "writable",
		},
		{
			name:    "other-writable ancestor is refused",
			setup:   func(tr *tree) { tr.file("/usr/bin/id", "", 0o755); tr.dir("/usr", 0o777) },
			command: "id",
			errHas:  "writable",
		},
		{
			name:    "a sticky world-writable ancestor is refused",
			setup:   func(tr *tree) { tr.file("/usr/bin/id", "", 0o755); tr.dir("/usr", 0o1777) },
			command: "id",
			errHas:  "writable",
		},
		{
			name:    "directory that is not a regular file",
			setup:   func(tr *tree) { tr.dir("/usr/bin/id", 0o755) },
			command: "id",
			errHas:  "not a regular file",
		},
		{
			name: "symlink to a trusted file is followed",
			setup: func(tr *tree) {
				tr.file("/usr/bin/busybox", "", 0o755)
				tr.symlink("/bin/id", "../usr/bin/busybox")
			},
			command: "id",
			want:    "/usr/bin/busybox",
		},
		{
			name: "merged /sbin symlink",
			setup: func(tr *tree) {
				tr.file("/usr/sbin/groupadd", "", 0o755)
				tr.dir("/usr/bin", 0o755)
				tr.symlink("/sbin", "usr/sbin")
			},
			dirs:    []string{"/usr/bin", "/sbin"},
			command: "groupadd",
			want:    "/usr/sbin/groupadd",
		},
		{
			name: "symlink into a writable directory is refused",
			setup: func(tr *tree) {
				tr.file("/home/user/bin/systemctl", "", 0o755)
				tr.dir("/home/user/bin", 0o777)
				tr.symlink("/usr/bin/systemctl", "../../home/user/bin/systemctl")
			},
			command: "systemctl",
			errHas:  "writable",
		},
		{
			name: "symlink out of the root is refused",
			setup: func(tr *tree) {
				tr.dir("/usr/bin", 0o755)
				if err := os.Symlink("/bin/sh", tr.path("/usr/bin/sh")); err != nil {
					t.Fatal(err)
				}
			},
			command: "sh",
			errHas:  "outside",
		},
		{
			name:    "dangling symlink",
			setup:   func(tr *tree) { tr.symlink("/usr/bin/id", "nowhere") },
			command: "id",
			errHas:  "not found",
		},
		{
			name:    "writable /usr/local/bin sudo is refused",
			setup:   func(tr *tree) { tr.file("/usr/local/bin/sudo", "", 0o755); tr.dir("/usr/local/bin", 0o775) },
			dirs:    []string{"/usr/bin", "/bin", "/usr/local/bin"},
			command: "sudo",
			errHas:  "writable",
		},
		{
			name:    "root-owned /usr/local/bin sudo is accepted",
			setup:   func(tr *tree) { tr.file("/usr/local/bin/sudo", "", 0o4755) },
			dirs:    []string{"/usr/bin", "/bin", "/usr/local/bin"},
			command: "sudo",
			want:    "/usr/local/bin/sudo",
		},
		{name: "path, not a name", command: "/usr/bin/id", errHas: "bare command name"},
		{name: "relative path", command: "bin/id", errHas: "bare command name"},
		{name: "empty name", command: "", errHas: "bare command name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := newTree(t)
			if tt.setup != nil {
				tt.setup(tr)
			}
			got, err := tr.finder(tt.dirs...).find(tt.command)
			if tt.want == "" {
				if err == nil {
					t.Fatalf("find(%q) = %q, want an error", tt.command, got)
				}
				if !strings.Contains(err.Error(), tt.errHas) {
					t.Fatalf("find(%q) error = %v, want it to mention %q", tt.command, err, tt.errHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("find(%q): %v", tt.command, err)
			}
			if want := tr.path(tt.want); got != want {
				t.Fatalf("find(%q) = %q, want %q", tt.command, got, want)
			}
		})
	}
}

// TestFinderUntrustedOwner shows a file the finder's owner does not own is
// refused, however safe its mode.
func TestFinderUntrustedOwner(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.file("/usr/bin/id", "", 0o755)
	f := tr.finder()
	f.owner++
	if got, err := f.find("id"); err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("find of a file owned by another user = %q, %v; want an ownership error", got, err)
	}
}

// TestFinderMissingWrapsNotFound keeps a missing command reporting
// exec.ErrNotFound, as exec's own lookup does.
func TestFinderMissingWrapsNotFound(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.dir("/usr/bin", 0o755)
	if _, err := tr.finder().find("nothing"); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("find of a missing command = %v, want exec.ErrNotFound", err)
	}
}

// hostilePath puts a directory first in PATH holding a fake command name
// that records being run in a marker file, and returns the marker.
func hostilePath(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script := "#!/bin/sh\ntouch '" + marker + "'\necho hostile\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func wantNotRun(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command planted first in PATH was run")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// TestExecRunnerIgnoresPath is the regression test for running host
// commands through the inherited PATH: a fake systemctl first in PATH is
// never run, by the real runner or by one over a fake trusted root.
func TestExecRunnerIgnoresPath(t *testing.T) {
	marker := hostilePath(t, "systemctl")

	// The real runner: on a host with systemctl in a trusted directory it
	// prints the real one's version; on any other it is not found.
	_, _ = ExecRunner(t.Context(), "systemctl", "--version")
	wantNotRun(t, marker)

	// A runner over a fake trusted root runs that root's systemctl.
	tr := newTree(t)
	tr.file("/usr/bin/systemctl", "echo trusted", 0o755)
	out, err := execRunner(tr.finder().find)(t.Context(), "systemctl", "--version")
	if err != nil {
		t.Fatalf("runner over the trusted root: %v", err)
	}
	if string(out) != "trusted\n" {
		t.Fatalf("runner ran %q, want the trusted systemctl", out)
	}
	wantNotRun(t, marker)
}

// TestExecRunnerUnknownCommand shows a command found only through PATH is
// not run: it is not in a trusted directory.
func TestExecRunnerUnknownCommand(t *testing.T) {
	marker := hostilePath(t, "chainmail-hostile-command")
	_, err := ExecRunner(t.Context(), "chainmail-hostile-command")
	if !errors.Is(err, exec.ErrNotFound) || exitCode(err) != -1 {
		t.Fatalf("ExecRunner of a command only in PATH = %v (exit %d), want exec.ErrNotFound", err, exitCode(err))
	}
	wantNotRun(t, marker)
}

// TestExecRunnerEnvironment shows a host command gets exactly the minimal
// environment, nothing of the caller's, and its name as argv[0].
func TestExecRunnerEnvironment(t *testing.T) {
	t.Setenv("CHAINMAIL_SOCKET", "/tmp/leak.sock")
	t.Setenv("HOME", "/home/leak")
	tr := newTree(t)
	tr.file("/usr/bin/show-env", `printf '%s|%s|%s|%s|%s\n' "$PATH" "$LANG" "$HOME" "$CHAINMAIL_SOCKET" "$0"`, 0o755)
	out, err := execRunner(tr.finder().find)(context.Background(), "show-env")
	if err != nil {
		t.Fatalf("show-env: %v", err)
	}
	want := "/usr/sbin:/usr/bin:/sbin:/bin|C|||" + tr.path("/usr/bin/show-env") + "\n"
	if string(out) != want {
		t.Fatalf("environment = %q, want %q", out, want)
	}
}

// TestRunAsIgnoresPath shows RunAs resolves a bare name through the trusted
// directories, never PATH.
func TestRunAsIgnoresPath(t *testing.T) {
	marker := hostilePath(t, "chainmail-hostile-command")
	uid, gid := me()
	err := RunAs(t.Context(), uid, gid, nil, nil, nil, "chainmail-hostile-command")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("RunAs of a command only in PATH = %v, want exec.ErrNotFound", err)
	}
	wantNotRun(t, marker)
}

// TestRunAsRefusesRelativePath shows RunAs runs only a bare name or an
// absolute path.
func TestRunAsRefusesRelativePath(t *testing.T) {
	t.Parallel()
	uid, gid := me()
	if err := RunAs(t.Context(), uid, gid, nil, nil, nil, "bin/chainmail"); err == nil {
		t.Fatal("RunAs ran a relative path")
	}
}
