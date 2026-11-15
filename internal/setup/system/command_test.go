package system

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
	want := []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin", "/run/current-system/sw/bin"}
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

// TestTrustedDirsIncludeNixOS shows the NixOS system profile is searched
// last, after the directories every other host uses, in their old order.
func TestTrustedDirsIncludeNixOS(t *testing.T) {
	t.Parallel()
	if len(trustedDirs) != 5 {
		t.Fatalf("trustedDirs = %v, want 5 entries", trustedDirs)
	}
	if got, want := trustedDirs[:4], []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"}; !slices.Equal(got, want) {
		t.Fatalf("trustedDirs[:4] = %v, want %v", got, want)
	}
	if got := trustedDirs[len(trustedDirs)-1]; got != "/run/current-system/sw/bin" {
		t.Fatalf("last trusted dir = %q, want /run/current-system/sw/bin", got)
	}
}

// TestFinderNixStore shows a command under the NixOS system profile is
// found through the group-writable /nix/store only while that directory
// has the sticky bit, is not writable by others and the finder's owner
// owns it, and that no other directory gets the same exception.
func TestFinderNixStore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		store  string      // host path of the store directory
		mode   os.FileMode // its mode
		owner  uint32      // when not 0, the uid the store is reported as owned by
		errHas string      // "" when systemctl is found
	}{
		{name: "sticky store", store: "/nix/store", mode: os.ModeSticky | 0o775},
		{name: "store without the sticky bit", store: "/nix/store", mode: 0o775, errHas: "writable by its group or others"},
		{name: "store writable by others", store: "/nix/store", mode: os.ModeSticky | 0o777, errHas: "writable by its group or others"},
		{name: "store owned by another uid", store: "/nix/store", mode: os.ModeSticky | 0o775, owner: 12345, errHas: "owned by uid 12345"},
		{name: "sticky directory elsewhere", store: "/opt/store", mode: os.ModeSticky | 0o775, errHas: "writable by its group or others"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := newTree(t)
			// run/current-system links into the store, as on NixOS.
			systemctl := tt.store + "/sys/sw/bin/systemctl"
			tr.file(systemctl, "", 0o555)
			tr.dir(tt.store, tt.mode)
			tr.dir("/run", 0o755)
			tr.symlink("/run/current-system", ".."+tt.store+"/sys")
			f := tr.finder("/run/current-system/sw/bin")
			if tt.owner != 0 {
				f.lstat = lstatOwnedBy(map[string]uint32{tr.path(tt.store): tt.owner})
			}

			got, err := f.find("systemctl")
			if tt.errHas != "" {
				if err == nil {
					t.Fatalf("find(systemctl) = %q, want an error", got)
				}
				if !strings.Contains(err.Error(), tt.errHas) {
					t.Fatalf("find(systemctl) error = %v, want it to mention %q", err, tt.errHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("find(systemctl): %v", err)
			}
			if want := tr.path(systemctl); got != want {
				t.Fatalf("find(systemctl) = %q, want %q", got, want)
			}
		})
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
		{
			name: "group-writable search directory holding a symlink to a trusted shell is refused",
			setup: func(tr *tree) {
				tr.file("/usr/bin/dash", "", 0o755)
				tr.dir("/usr/local/bin", 0o775)
				tr.symlink("/usr/local/bin/getent", "../../bin/dash")
			},
			dirs:    []string{"/usr/local/bin"},
			command: "getent",
			errHas:  "writable",
		},
		{
			name: "other-writable search directory holding a symlink to a trusted shell is refused",
			setup: func(tr *tree) {
				tr.file("/usr/bin/dash", "", 0o755)
				tr.symlink("/usr/sbin/getent", "../bin/dash")
				tr.dir("/usr/sbin", 0o777)
			},
			dirs:    []string{"/usr/sbin"},
			command: "getent",
			errHas:  "writable",
		},
		{
			name: "symlink hop through a writable directory is refused",
			setup: func(tr *tree) {
				tr.file("/usr/bin/dash", "", 0o755)
				tr.symlink("/opt/hop/getent", "../../usr/bin/dash")
				tr.dir("/opt/hop", 0o777)
				tr.symlink("/usr/bin/getent", "../../opt/hop/getent")
			},
			dirs:    []string{"/usr/bin"},
			command: "getent",
			errHas:  "writable",
		},
		{
			name: "directory symlink into a writable directory is refused",
			setup: func(tr *tree) {
				tr.file("/home/user/sbin/getent", "", 0o755)
				tr.dir("/home/user", 0o777)
				tr.symlink("/sbin", "home/user/sbin")
			},
			dirs:    []string{"/sbin"},
			command: "getent",
			errHas:  "writable",
		},
		{
			name: "chain of trusted symlinks is followed",
			setup: func(tr *tree) {
				tr.file("/usr/bin/dash", "", 0o755)
				tr.symlink("/usr/bin/sh", "dash")
				tr.symlink("/bin", "usr/bin")
			},
			dirs:    []string{"/bin"},
			command: "sh",
			want:    "/usr/bin/dash",
		},
		{
			name: "absolute symlink inside the root is followed",
			setup: func(tr *tree) {
				tr.file("/usr/sbin/groupadd", "", 0o755)
				tr.symlink("/sbin", tr.path("/usr/sbin"))
			},
			dirs:    []string{"/sbin"},
			command: "groupadd",
			want:    "/usr/sbin/groupadd",
		},
		{
			// The kernel resolves /opt/redirect before "..", so the link
			// reaches /usr/local/libexec/helper. Cleaning the target first
			// would pick /opt/libexec/helper instead.
			name: "absolute target with a symlink before .. resolves as the kernel does",
			setup: func(tr *tree) {
				tr.file("/usr/local/libexec/helper", "", 0o755)
				tr.dir("/usr/local/bin", 0o755)
				tr.file("/opt/libexec/helper", "", 0o755)
				tr.symlink("/opt/redirect", tr.path("/usr/local/bin"))
				tr.symlink("/usr/bin/helper", tr.path("/opt/redirect")+"/../libexec/helper")
			},
			dirs:    []string{"/usr/bin"},
			command: "helper",
			want:    "/usr/local/libexec/helper",
		},
		{
			name: "relative target with a symlink before .. resolves as the kernel does",
			setup: func(tr *tree) {
				tr.file("/usr/local/libexec/helper", "", 0o755)
				tr.dir("/usr/local/bin", 0o755)
				tr.file("/opt/libexec/helper", "", 0o755)
				tr.symlink("/opt/redirect", "../usr/local/bin")
				tr.symlink("/usr/bin/helper", "../../opt/redirect/../libexec/helper")
			},
			dirs:    []string{"/usr/bin"},
			command: "helper",
			want:    "/usr/local/libexec/helper",
		},
		{
			// The link really reaches /srv/drop/libexec/helper, inside a
			// world-writable directory that cleaning the target would skip.
			name: "absolute target with a symlink into a writable directory before .. is refused",
			setup: func(tr *tree) {
				tr.file("/srv/drop/libexec/helper", "", 0o755)
				tr.dir("/srv/drop/bin", 0o755)
				tr.dir("/srv/drop", 0o777)
				tr.file("/opt/libexec/helper", "", 0o755)
				tr.symlink("/opt/redirect", tr.path("/srv/drop/bin"))
				tr.symlink("/usr/bin/helper", tr.path("/opt/redirect")+"/../libexec/helper")
			},
			dirs:    []string{"/usr/bin"},
			command: "helper",
			errHas:  "writable",
		},
		{
			name: "relative target with a symlink into a writable directory before .. is refused",
			setup: func(tr *tree) {
				tr.file("/srv/drop/libexec/helper", "", 0o755)
				tr.dir("/srv/drop/bin", 0o755)
				tr.dir("/srv/drop", 0o777)
				tr.file("/opt/libexec/helper", "", 0o755)
				tr.symlink("/opt/redirect", "../srv/drop/bin")
				tr.symlink("/usr/bin/helper", "../../opt/redirect/../libexec/helper")
			},
			dirs:    []string{"/usr/bin"},
			command: "helper",
			errHas:  "writable",
		},
		{
			name: "search directory with a symlink before .. resolves as the kernel does",
			setup: func(tr *tree) {
				tr.file("/usr/local/sbin/helper", "", 0o755)
				tr.dir("/usr/local/bin", 0o755)
				tr.file("/opt/sbin/helper", "", 0o755)
				tr.symlink("/opt/redirect", "../usr/local/bin")
			},
			dirs:    []string{"/opt/redirect/../sbin"},
			command: "helper",
			want:    "/usr/local/sbin/helper",
		},
		{
			name: "absolute target climbing out of the root through .. is refused",
			setup: func(tr *tree) {
				tr.dir("/usr/bin", 0o755)
				tr.symlink("/usr/bin/sh", tr.path("/usr")+"/../../bin/sh")
			},
			dirs:    []string{"/usr/bin"},
			command: "sh",
			errHas:  "outside",
		},
		{
			name: "absolute target naming the root through .. is refused",
			setup: func(tr *tree) {
				tr.file("/usr/bin/dash", "", 0o755)
				tr.symlink("/usr/bin/sh", tr.root+"/../"+filepath.Base(tr.root)+"/usr/bin/dash")
			},
			dirs:    []string{"/usr/bin"},
			command: "sh",
			errHas:  "outside",
		},
		{
			name: "symlink loop is refused",
			setup: func(tr *tree) {
				tr.symlink("/usr/bin/a", "b")
				tr.symlink("/usr/bin/b", "a")
			},
			dirs:    []string{"/usr/bin"},
			command: "a",
			errHas:  "too many",
		},
		{
			name: "symlink climbing out of the root is refused",
			setup: func(tr *tree) {
				tr.symlink("/usr/bin/sh", "../../../../../../../../../../../../bin/sh")
			},
			dirs:    []string{"/usr/bin"},
			command: "sh",
			errHas:  "outside",
		},
		{
			name: "symlink resolving to a directory is refused",
			setup: func(tr *tree) {
				tr.dir("/usr/lib/thing", 0o755)
				tr.symlink("/usr/bin/thing", "../lib/thing/..")
			},
			dirs:    []string{"/usr/bin"},
			command: "thing",
			errHas:  "not a regular file",
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

// ownedBy reports fs.FileInfo as owned by uid, for a test that cannot
// chown.
type ownedBy struct {
	fs.FileInfo
	uid uint32
}

func (o ownedBy) Sys() any {
	st := *o.FileInfo.Sys().(*syscall.Stat_t)
	st.Uid = o.uid
	return &st
}

// lstatOwnedBy is os.Lstat, except that it reports each path in owners as
// owned by the uid given for it.
func lstatOwnedBy(owners map[string]uint32) func(string) (fs.FileInfo, error) {
	return func(path string) (fs.FileInfo, error) {
		fi, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if uid, ok := owners[path]; ok {
			return ownedBy{FileInfo: fi, uid: uid}, nil
		}
		return fi, nil
	}
}

// TestFinderUntrustedSymlinkOwner shows a symlink the finder's owner does
// not own is refused, even in a trusted directory and pointing at a
// trusted file, while the same link owned by the owner is followed.
func TestFinderUntrustedSymlinkOwner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(tr *tree)
		link    string // host path of the symlink
		dirs    []string
		command string
		want    string
	}{
		{
			name: "command symlink",
			setup: func(tr *tree) {
				tr.file("/usr/bin/dash", "", 0o755)
				tr.symlink("/usr/bin/getent", "dash")
			},
			link:    "/usr/bin/getent",
			dirs:    []string{"/usr/bin"},
			command: "getent",
			want:    "/usr/bin/dash",
		},
		{
			name: "search directory symlink",
			setup: func(tr *tree) {
				tr.file("/usr/sbin/groupadd", "", 0o755)
				tr.symlink("/sbin", "usr/sbin")
			},
			link:    "/sbin",
			dirs:    []string{"/sbin"},
			command: "groupadd",
			want:    "/usr/sbin/groupadd",
		},
		{
			// /opt/redirect is followed before "..", so its owner counts,
			// though cleaning the target would erase it.
			name: "symlink before .. in an absolute target",
			setup: func(tr *tree) {
				tr.file("/usr/local/libexec/helper", "", 0o755)
				tr.dir("/usr/local/bin", 0o755)
				tr.file("/opt/libexec/helper", "", 0o755)
				tr.symlink("/opt/redirect", tr.path("/usr/local/bin"))
				tr.symlink("/usr/bin/helper", tr.path("/opt/redirect")+"/../libexec/helper")
			},
			link:    "/opt/redirect",
			dirs:    []string{"/usr/bin"},
			command: "helper",
			want:    "/usr/local/libexec/helper",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := newTree(t)
			tt.setup(tr)
			f := tr.finder(tt.dirs...)

			f.lstat = lstatOwnedBy(map[string]uint32{tr.path(tt.link): f.owner})
			got, err := f.find(tt.command)
			if err != nil || got != tr.path(tt.want) {
				t.Fatalf("find(%q) with a trusted link = %q, %v; want %q", tt.command, got, err, tr.path(tt.want))
			}

			f.lstat = lstatOwnedBy(map[string]uint32{tr.path(tt.link): f.owner + 1})
			if got, err := f.find(tt.command); err == nil || !strings.Contains(err.Error(), "owned by") {
				t.Fatalf("find(%q) through a link another user owns = %q, %v; want an ownership error", tt.command, got, err)
			}
		})
	}
}

// TestFinderUntrustedSearchDirOwner shows a search directory the finder's
// owner does not own is refused, though the command in it is a symlink to
// a trusted file.
func TestFinderUntrustedSearchDirOwner(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.file("/usr/bin/dash", "", 0o755)
	tr.symlink("/usr/local/bin/getent", "../../bin/dash")
	f := tr.finder("/usr/local/bin")
	f.lstat = lstatOwnedBy(map[string]uint32{tr.path("/usr/local/bin"): f.owner + 1})
	if got, err := f.find("getent"); err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("find through a search directory another user owns = %q, %v; want an ownership error", got, err)
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

// TestExecRunnerWorkingDirectory shows a host command runs in "/", never
// the caller's working directory, so a relative argument such as a file
// name a shell would read cannot resolve against a directory the invoking
// user controls.
func TestExecRunnerWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	tr := newTree(t)
	tr.file("/usr/bin/show-dir", "pwd", 0o755)
	out, err := execRunner(tr.finder().find)(context.Background(), "show-dir")
	if err != nil {
		t.Fatalf("show-dir: %v", err)
	}
	if string(out) != "/\n" {
		t.Fatalf("working directory = %q, want /", out)
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
