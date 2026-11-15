// Package setuptest holds fakes of the operating system for testing
// chainmail bootstrap: files with owners and modes, users and groups, the
// service manager, the daemon run as its own user, a clock and a terminal.
// None of them touch the real system.
//
// It imports only setup/system, so the setup package's own tests can use
// it too.
package setuptest

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/yaklabco/chainmail/internal/setup/system"
)

// maxSymlinkHops matches Linux's limit on symlinks followed in one path.
const maxSymlinkHops = 40

// node is one path in Files.
type node struct {
	info system.FileInfo
	data []byte
}

// Files is a map-backed system.Files. Paths not in the map do not exist.
// Ancestor symlinks are followed the way the kernel follows them, so
// /etc/chainmail on a macOS tree is /private/etc/chainmail; a final
// symlink never is. The setters (Dir, File, Symlink, Remove) do not check
// parents, so a test can build any tree.
type Files struct {
	// Journal, when set, gets one entry per change: "mkdir <path>",
	// "write <path>", "create <path>", "lock <path>" and "unlock <path>".
	Journal *Journal

	mu        sync.Mutex
	nodes     map[string]node
	errs      map[string]error
	writeErrs map[string]error
	writes    []string
	locked    map[string]bool
}

var _ system.Files = (*Files)(nil)

// NewFiles returns an empty tree.
func NewFiles() *Files {
	return &Files{
		nodes:     map[string]node{},
		errs:      map[string]error{},
		writeErrs: map[string]error{},
		locked:    map[string]bool{},
	}
}

// resolve follows symlinks in the ancestors of path. The caller holds mu,
// or owns f.
func (f *Files) resolve(path string) string {
	parts := splitPath(filepath.Clean(path))
	cur := "/"
	for hops := 0; len(parts) > 0 && hops < maxSymlinkHops; {
		name := parts[0]
		parts = parts[1:]
		if name == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		n, ok := f.nodes[next]
		if ok && len(parts) > 0 && n.info.Mode&fs.ModeSymlink != 0 {
			hops++
			if filepath.IsAbs(n.info.LinkDest) {
				cur = "/"
			}
			parts = append(splitPath(n.info.LinkDest), parts...)
			continue
		}
		cur = next
	}
	return cur
}

// splitPath splits a path into its components, dropping empty ones.
func splitPath(path string) []string {
	return strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' })
}

// Dir adds a directory.
func (f *Files) Dir(path string, uid, gid uint32, perm fs.FileMode) *Files {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[f.resolve(path)] = node{info: system.FileInfo{Exists: true, Mode: fs.ModeDir | perm, UID: uid, GID: gid}}
	return f
}

// File adds a regular file holding data. perm may carry type bits such as
// fs.ModeSocket.
func (f *Files) File(path string, uid, gid uint32, perm fs.FileMode, data []byte) *Files {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[f.resolve(path)] = node{info: system.FileInfo{Exists: true, Mode: perm, UID: uid, GID: gid}, data: slices.Clone(data)}
	return f
}

// Symlink adds a symlink to dest owned by uid.
func (f *Files) Symlink(path string, uid uint32, dest string) *Files {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[f.resolve(path)] = node{info: system.FileInfo{Exists: true, Mode: fs.ModeSymlink | 0o777, UID: uid, LinkDest: dest}}
	return f
}

// Remove deletes path from the tree.
func (f *Files) Remove(path string) *Files {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nodes, f.resolve(path))
	return f
}

// FailRead makes Lstat and ReadFile of path fail with err.
func (f *Files) FailRead(path string, err error) *Files {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[f.resolve(path)] = err
	return f
}

// FailWrite makes EnsureDir, WriteFile and CreateExclusive of path fail
// with err, wrapped as the real Files wraps it.
func (f *Files) FailWrite(path string, err error) *Files {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeErrs[f.resolve(path)] = err
	return f
}

// Writes lists the resolved paths changed through the system.Files
// methods, in order.
func (f *Files) Writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// Content returns the data and metadata at path, whatever its mode, and
// false when it does not exist.
func (f *Files) Content(path string) ([]byte, system.FileInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[f.resolve(path)]
	return slices.Clone(n.data), n.info, ok
}

// Locked reports whether path is locked now.
func (f *Files) Locked(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.locked[f.resolve(path)]
}

// Lstat implements system.Files.
func (f *Files) Lstat(path string) (system.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if err := f.errs[path]; err != nil {
		return system.FileInfo{}, fmt.Errorf("inspecting %s: %w", path, err)
	}
	n, ok := f.nodes[path]
	if !ok {
		return system.FileInfo{}, nil
	}
	return n.info, nil
}

// ReadFile implements system.Files. Like the real one, it refuses
// anything that is not a regular file, including a final symlink.
func (f *Files) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if err := f.errs[path]; err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	n, ok := f.nodes[path]
	switch {
	case !ok:
		return nil, fmt.Errorf("opening %s: %w", path, fs.ErrNotExist)
	case !n.info.Mode.IsRegular():
		return nil, fmt.Errorf("opening %s: not a regular file", path)
	}
	return slices.Clone(n.data), nil
}

// EnsureDir implements system.Files. The parent must be a directory.
func (f *Files) EnsureDir(path string, uid, gid uint32, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if err := f.writable(path, "creating"); err != nil {
		return err
	}
	if n, ok := f.nodes[path]; ok && !n.info.Mode.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", path)
	}
	f.nodes[path] = node{info: system.FileInfo{Exists: true, Mode: fs.ModeDir | mode, UID: uid, GID: gid}}
	f.record("mkdir", path)
	return nil
}

// WriteFile implements system.Files. The parent must be a directory.
func (f *Files) WriteFile(path string, data []byte, uid, gid uint32, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if err := f.writable(path, "writing"); err != nil {
		return err
	}
	f.nodes[path] = node{info: system.FileInfo{Exists: true, Mode: mode, UID: uid, GID: gid}, data: slices.Clone(data)}
	f.record("write", path)
	return nil
}

// CreateExclusive implements system.Files. An existing path is an error
// wrapping fs.ErrExist.
func (f *Files) CreateExclusive(path string, data []byte, uid, gid uint32, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if err := f.writable(path, "creating"); err != nil {
		return err
	}
	if _, ok := f.nodes[path]; ok {
		return fmt.Errorf("creating %s: %w", path, fs.ErrExist)
	}
	f.nodes[path] = node{info: system.FileInfo{Exists: true, Mode: mode, UID: uid, GID: gid}, data: slices.Clone(data)}
	f.record("create", path)
	return nil
}

// Lock implements system.Files. Locking a path that is already locked is
// an error rather than a wait, so a test that forgets to unlock fails.
func (f *Files) Lock(path string) (func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if f.locked[path] {
		return nil, fmt.Errorf("locking %s: already locked", path)
	}
	if err := f.parentDir(path); err != nil {
		return nil, fmt.Errorf("opening lock %s: %w", path, err)
	}
	if _, ok := f.nodes[path]; !ok {
		f.nodes[path] = node{info: system.FileInfo{Exists: true, Mode: 0o600}}
	}
	f.locked[path] = true
	f.Journal.Add("lock " + path)
	return func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.locked, path)
		f.Journal.Add("unlock " + path)
		return nil
	}, nil
}

// writable fails when a write to path was told to fail or its parent is
// not a directory. The caller holds mu.
func (f *Files) writable(path, verb string) error {
	if err := f.writeErrs[path]; err != nil {
		return fmt.Errorf("%s %s: %w", verb, path, err)
	}
	return f.parentDir(path)
}

// parentDir fails unless path's parent is a directory. The caller holds mu.
func (f *Files) parentDir(path string) error {
	parent := filepath.Dir(path)
	if n, ok := f.nodes[parent]; !ok || !n.info.Mode.IsDir() {
		return fmt.Errorf("parent of %s: %w", path, fs.ErrNotExist)
	}
	return nil
}

// record notes a change. The caller holds mu.
func (f *Files) record(verb, path string) {
	f.writes = append(f.writes, path)
	f.Journal.Add(verb + " " + path)
}
