package setup

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
)

const daemonUID = 400

// fakeFS is a map-backed FSView. Paths not in the map do not exist. It
// records every path it is asked about.
type fakeFS struct {
	mu    sync.Mutex
	files map[string]FileInfo
	asked []string
	err   map[string]error
}

func (f *fakeFS) Lstat(path string) (FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, path)
	if err := f.err[path]; err != nil {
		return FileInfo{}, err
	}
	fi, ok := f.files[path]
	if !ok {
		return FileInfo{Exists: false}, nil
	}
	return fi, nil
}

func dir(uid uint32, perm fs.FileMode) FileInfo {
	return FileInfo{Exists: true, Mode: fs.ModeDir | perm, UID: uid}
}

func file(uid uint32, perm fs.FileMode) FileInfo {
	return FileInfo{Exists: true, Mode: perm, UID: uid}
}

func link(uid uint32, dest string) FileInfo {
	return FileInfo{Exists: true, Mode: fs.ModeSymlink | 0o777, UID: uid, LinkDest: dest}
}

// stockLinux is a root-owned tree holding the paths bootstrap uses.
func stockLinux() map[string]FileInfo {
	return map[string]FileInfo{
		"/":                          dir(0, 0o755),
		"/etc":                       dir(0, 0o755),
		"/etc/chainmail":             dir(0, 0o755),
		"/etc/chainmail/config.toml": file(0, 0o644),
		"/usr":                       dir(0, 0o755),
		"/usr/local":                 dir(0, 0o755),
		"/usr/local/libexec":         dir(0, 0o755),
		"/var":                       dir(0, 0o755),
		"/var/lib":                   dir(0, 0o755),
		"/tmp":                       dir(0, fs.ModeSticky|0o777),
	}
}

// stockDarwin has /etc and /var as root-owned symlinks into /private.
func stockDarwin() map[string]FileInfo {
	return map[string]FileInfo{
		"/":            dir(0, 0o755),
		"/etc":         link(0, "private/etc"),
		"/var":         link(0, "private/var"),
		"/private":     dir(0, 0o755),
		"/private/etc": dir(0, 0o755),
		"/private/var": dir(0, 0o755),
	}
}

func with(base map[string]FileInfo, edits map[string]FileInfo) map[string]FileInfo {
	for k, v := range edits {
		base[k] = v
	}
	return base
}

func TestCheckTrust(t *testing.T) {
	t.Parallel()
	root := []uint32{0}
	rootAndDaemon := []uint32{0, daemonUID}

	cases := []struct {
		name    string
		files   map[string]FileInfo
		path    string
		trusted []uint32
		// unsafe is the path the refusal must name, "" when the check passes.
		unsafe string
		reason string
	}{
		{
			name:    "stock linux tree",
			files:   stockLinux(),
			path:    "/etc/chainmail/config.toml",
			trusted: root,
		},
		{
			name:    "missing leaf",
			files:   stockLinux(),
			path:    "/etc/chainmail/vault-key",
			trusted: root,
		},
		{
			name:    "missing parent",
			files:   stockLinux(),
			path:    "/var/lib/chainmail/vault.age",
			trusted: root,
		},
		{
			name:    "missing parent under libexec",
			files:   stockLinux(),
			path:    "/usr/local/libexec/chainmail/chainmail",
			trusted: root,
		},
		{
			name:    "ancestor owned by a user",
			files:   with(stockLinux(), map[string]FileInfo{"/usr/local": dir(1000, 0o755)}),
			path:    "/usr/local/libexec/chainmail/chainmail",
			trusted: root,
			unsafe:  "/usr/local",
			reason:  "owned by uid 1000",
		},
		{
			name:    "group-writable ancestor",
			files:   with(stockLinux(), map[string]FileInfo{"/usr/local": dir(0, 0o775)}),
			path:    "/usr/local/libexec/chainmail/chainmail",
			trusted: root,
			unsafe:  "/usr/local",
			reason:  "writable by group or others",
		},
		{
			name:    "other-writable ancestor",
			files:   with(stockLinux(), map[string]FileInfo{"/var/lib": dir(0, 0o757)}),
			path:    "/var/lib/chainmail",
			trusted: root,
			unsafe:  "/var/lib",
			reason:  "writable by group or others",
		},
		{
			name:    "group-writable leaf",
			files:   with(stockLinux(), map[string]FileInfo{"/etc/chainmail/config.toml": file(0, 0o664)}),
			path:    "/etc/chainmail/config.toml",
			trusted: root,
			unsafe:  "/etc/chainmail/config.toml",
			reason:  "writable by group or others",
		},
		{
			name:    "root directory owned by a user",
			files:   with(stockLinux(), map[string]FileInfo{"/": dir(1000, 0o755)}),
			path:    "/etc/chainmail/config.toml",
			trusted: root,
			unsafe:  "/",
			reason:  "owned by uid 1000",
		},
		{
			name:    "final component symlink",
			files:   with(stockLinux(), map[string]FileInfo{"/etc/chainmail/config.toml": link(0, "/tmp/config.toml")}),
			path:    "/etc/chainmail/config.toml",
			trusted: root,
			unsafe:  "/etc/chainmail/config.toml",
			reason:  "is a symlink",
		},
		{
			name:    "macos etc symlink into private",
			files:   with(stockDarwin(), map[string]FileInfo{"/private/etc/chainmail": dir(0, 0o755)}),
			path:    "/etc/chainmail/config.toml",
			trusted: root,
		},
		{
			name:    "macos symlink target is checked",
			files:   with(stockDarwin(), map[string]FileInfo{"/private/etc": dir(1000, 0o755)}),
			path:    "/etc/chainmail/config.toml",
			trusted: root,
			unsafe:  "/private/etc",
			reason:  "owned by uid 1000",
		},
		{
			name:    "absolute symlink target",
			files:   with(stockDarwin(), map[string]FileInfo{"/etc": link(0, "/private/etc")}),
			path:    "/etc/chainmail/config.toml",
			trusted: root,
		},
		{
			name: "relative symlink target with dot-dot",
			files: with(stockLinux(), map[string]FileInfo{
				"/var/run": link(0, "../run"),
				"/run":     dir(0, 0o755),
			}),
			path:    "/var/run/chainmail/chainmail.sock",
			trusted: root,
		},
		{
			name: "dot-dot symlink target is checked",
			files: with(stockLinux(), map[string]FileInfo{
				"/var/run": link(0, "../run"),
				"/run":     dir(0, 0o777),
			}),
			path:    "/var/run/chainmail/chainmail.sock",
			trusted: root,
			unsafe:  "/run",
			reason:  "writable by group or others",
		},
		{
			name: "chain of root-owned symlinks",
			files: with(stockDarwin(), map[string]FileInfo{
				"/etc":                   link(0, "/alias/etc"),
				"/alias":                 link(0, "private"),
				"/private/etc/chainmail": dir(0, 0o755),
			}),
			path:    "/etc/chainmail/config.toml",
			trusted: root,
		},
		{
			name:    "symlink owned by a user",
			files:   with(stockDarwin(), map[string]FileInfo{"/etc": link(1000, "private/etc")}),
			path:    "/etc/chainmail/config.toml",
			trusted: root,
			unsafe:  "/etc",
			reason:  "is a symlink owned by uid 1000",
		},
		{
			name:    "symlink owned by the daemon user",
			files:   with(stockDarwin(), map[string]FileInfo{"/etc": link(daemonUID, "private/etc")}),
			path:    "/etc/chainmail/config.toml",
			trusted: rootAndDaemon,
			unsafe:  "/etc",
			reason:  "is a symlink owned by uid 400",
		},
		{
			name:    "symlink in an unsafe directory",
			files:   with(stockDarwin(), map[string]FileInfo{"/": dir(1000, 0o755)}),
			path:    "/etc/chainmail/config.toml",
			trusted: root,
			unsafe:  "/",
			reason:  "owned by uid 1000",
		},
		{
			name: "symlink loop",
			files: with(stockLinux(), map[string]FileInfo{
				"/a": link(0, "/b"),
				"/b": link(0, "a"),
			}),
			path:    "/a/x",
			trusted: root,
			unsafe:  "/a/x",
			reason:  "too many symlinks",
		},
		{
			name:    "daemon-owned path with daemon trusted",
			files:   with(stockLinux(), map[string]FileInfo{"/var/lib/chainmail": dir(daemonUID, 0o700)}),
			path:    "/var/lib/chainmail/vault.age",
			trusted: rootAndDaemon,
		},
		{
			name:    "daemon-owned path with daemon not trusted",
			files:   with(stockLinux(), map[string]FileInfo{"/var/lib/chainmail": dir(daemonUID, 0o700)}),
			path:    "/var/lib/chainmail/vault.age",
			trusted: root,
			unsafe:  "/var/lib/chainmail",
			reason:  "owned by uid 400",
		},
		{
			name:    "sticky world-writable directory",
			files:   stockLinux(),
			path:    "/tmp/chainmail.sock",
			trusted: root,
			unsafe:  "/tmp",
			reason:  "writable by group or others",
		},
		{
			name:    "ancestor that is not a directory",
			files:   with(stockLinux(), map[string]FileInfo{"/etc/chainmail": file(0, 0o644)}),
			path:    "/etc/chainmail/config.toml",
			trusted: root,
			unsafe:  "/etc/chainmail",
			reason:  "is not a directory",
		},
		{
			name:    "dot-dot component",
			files:   stockLinux(),
			path:    "/etc/chainmail/../passwd",
			trusted: root,
			unsafe:  "/etc/chainmail/../passwd",
			reason:  "contains ..",
		},
		{
			name:    "relative path",
			files:   stockLinux(),
			path:    "etc/chainmail",
			trusted: root,
			unsafe:  "etc/chainmail",
			reason:  "is not absolute",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsv := &fakeFS{files: tc.files}
			err := CheckTrust(fsv, tc.path, tc.trusted)
			if tc.unsafe == "" {
				if err != nil {
					t.Fatalf("CheckTrust(%q) = %v, want nil", tc.path, err)
				}
				return
			}
			var r *Refusal
			if !errors.As(err, &r) {
				t.Fatalf("CheckTrust(%q) = %v (%T), want *Refusal", tc.path, err, err)
			}
			wantMsg := "unsafe path " + tc.unsafe + ": " + tc.reason
			if r.Msg != wantMsg {
				t.Errorf("message = %q, want %q", r.Msg, wantMsg)
			}
			wantHint := "bootstrap only trusts paths that root owns; fix the ownership or mode of " +
				tc.unsafe + " and run it again"
			if r.Hint != wantHint {
				t.Errorf("hint = %q, want %q", r.Hint, wantHint)
			}
		})
	}
}

func TestCheckTrustChecksSymlinkTarget(t *testing.T) {
	t.Parallel()
	fsv := &fakeFS{files: with(stockDarwin(), map[string]FileInfo{"/private/etc/chainmail": dir(0, 0o755)})}
	if err := CheckTrust(fsv, "/etc/chainmail/config.toml", []uint32{0}); err != nil {
		t.Fatalf("CheckTrust = %v", err)
	}
	for _, want := range []string{"/", "/etc", "/private", "/private/etc", "/private/etc/chainmail", "/private/etc/chainmail/config.toml"} {
		if !slices.Contains(fsv.asked, want) {
			t.Errorf("CheckTrust never looked at %q; asked %q", want, fsv.asked)
		}
	}
}

func TestCheckTrustLstatError(t *testing.T) {
	t.Parallel()
	boom := errors.New("permission denied")
	fsv := &fakeFS{files: stockLinux(), err: map[string]error{"/etc/chainmail": boom}}
	err := CheckTrust(fsv, "/etc/chainmail/config.toml", []uint32{0})
	if !errors.Is(err, boom) {
		t.Fatalf("CheckTrust = %v, want it to wrap %v", err, boom)
	}
	var r *Refusal
	if errors.As(err, &r) {
		t.Fatalf("an Lstat failure is not a Refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "/etc/chainmail") {
		t.Errorf("error %q does not name the path", err)
	}
}

// nixStoreTree is a NixOS store path to the chainmail binary: /nix/store
// is root:nixbld 1775, the entries below it root-owned and read-only.
func nixStoreTree(edits map[string]FileInfo) map[string]FileInfo {
	store := dir(0, fs.ModeSticky|0o775)
	store.GID = 30000
	return with(map[string]FileInfo{
		"/":                                      dir(0, 0o755),
		"/nix":                                   dir(0, 0o755),
		"/nix/store":                             store,
		"/nix/store/abc-chainmail":               dir(0, 0o555),
		"/nix/store/abc-chainmail/bin":           dir(0, 0o555),
		"/nix/store/abc-chainmail/bin/chainmail": file(0, 0o555),
		"/opt":                                   dir(0, 0o755),
		"/opt/store":                             dir(0, fs.ModeSticky|0o775),
		"/opt/store/chainmail":                   file(0, 0o555),
	}, edits)
}

func TestCheckTrustNixStore(t *testing.T) {
	t.Parallel()
	const binary = "/nix/store/abc-chainmail/bin/chainmail"
	storeWith := func(uid uint32, mode fs.FileMode) FileInfo {
		fi := dir(uid, mode)
		fi.GID = 30000
		return fi
	}
	cases := []struct {
		name   string
		files  map[string]FileInfo
		path   string
		unsafe string // the path the refusal names, "" when the check passes
		reason string
	}{
		{
			name:  "root-owned sticky store",
			files: nixStoreTree(nil),
			path:  binary,
		},
		{
			name:   "store without the sticky bit",
			files:  nixStoreTree(map[string]FileInfo{"/nix/store": storeWith(0, 0o775)}),
			path:   binary,
			unsafe: "/nix/store",
			reason: "writable by group or others",
		},
		{
			name:   "store writable by others",
			files:  nixStoreTree(map[string]FileInfo{"/nix/store": storeWith(0, fs.ModeSticky|0o777)}),
			path:   binary,
			unsafe: "/nix/store",
			reason: "writable by group or others",
		},
		{
			name:   "store owned by a user",
			files:  nixStoreTree(map[string]FileInfo{"/nix/store": storeWith(1000, fs.ModeSticky|0o775)}),
			path:   binary,
			unsafe: "/nix/store",
			reason: "owned by uid 1000",
		},
		{
			name:   "group-writable store entry",
			files:  nixStoreTree(map[string]FileInfo{"/nix/store/abc-chainmail": dir(0, 0o775)}),
			path:   binary,
			unsafe: "/nix/store/abc-chainmail",
			reason: "writable by group or others",
		},
		{
			name:   "missing store entry",
			files:  nixStoreTree(nil),
			path:   "/nix/store/missing/bin/chainmail",
			unsafe: "/nix/store/missing",
			reason: "is missing from the Nix store, where a build user could create it",
		},
		{
			name:  "missing below a root-owned store entry",
			files: nixStoreTree(nil),
			path:  "/nix/store/abc-chainmail/bin/absent",
		},
		{
			name:   "store is a regular file",
			files:  nixStoreTree(map[string]FileInfo{"/nix/store": file(0, fs.ModeSticky|0o775)}),
			path:   "/nix/store",
			unsafe: "/nix/store",
			reason: "writable by group or others",
		},
		{
			name:   "sticky directory elsewhere",
			files:  nixStoreTree(nil),
			path:   "/opt/store/chainmail",
			unsafe: "/opt/store",
			reason: "writable by group or others",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckTrust(&fakeFS{files: tc.files}, tc.path, []uint32{0})
			if tc.unsafe == "" {
				if err != nil {
					t.Fatalf("CheckTrust(%q) = %v, want nil", tc.path, err)
				}
				return
			}
			var r *Refusal
			if !errors.As(err, &r) {
				t.Fatalf("CheckTrust(%q) = %v (%T), want *Refusal", tc.path, err, err)
			}
			if want := "unsafe path " + tc.unsafe + ": " + tc.reason; r.Msg != want {
				t.Errorf("message = %q, want %q", r.Msg, want)
			}
		})
	}
}
