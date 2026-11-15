package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// trustedDirs are the only directories bootstrap runs host commands from,
// searched in this order. PATH is never consulted: under sudo it can
// still name a directory the invoking user, or an agent of theirs, can
// write. /run/current-system/sw/bin is the NixOS system profile, a link
// into the Nix store; elsewhere it does not exist, and only root can
// create it under /run.
var trustedDirs = []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin", "/run/current-system/sw/bin"}

// commandEnv is the whole environment of a host command. Nothing is
// inherited from the caller.
var commandEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}

// hostFinder finds host commands on the real system: in trustedDirs, owned
// by root.
var hostFinder = finder{root: "/", dirs: trustedDirs, owner: 0}

// maxSymlinkHops is how many symlinks the finder follows for one
// candidate before it treats the path as a loop, matching Linux's limit.
const maxSymlinkHops = 40

// finder resolves a bare command name to the absolute path of a file only
// owner can change. root is "/" on a real host and a test tree in tests;
// dirs are host paths under it. lstat reads path metadata without
// following a final symlink; nil means os.Lstat.
type finder struct {
	root  string
	dirs  []string
	owner uint32
	lstat func(string) (fs.FileInfo, error)
}

// LookupTrusted returns the absolute path, with every symlink resolved, of
// the command name in the first of dirs that holds a trusted copy. A copy
// is trusted when every directory on the way to it, from "/" through the
// search directory, is owned by root and writable only by root; every
// symlink on the way is owned by root and sits in such a directory; and
// it ends at a regular file root owns and only root can write. The Nix
// store directory alone may also be writable by its group, while it has
// the sticky bit, as trusted explains. A missing command is an error
// wrapping exec.ErrNotFound.
func LookupTrusted(name string, dirs []string) (string, error) {
	return finder{root: "/", dirs: dirs, owner: 0}.find(name)
}

// find returns the resolved path of the first trusted copy of name.
func (f finder) find(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("command %q: not a bare command name", name)
	}
	root, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		return "", fmt.Errorf("finding %s: %w", name, err)
	}
	var refused []error
	for _, dir := range f.dirs {
		// Not filepath.Join, which would clean ".." away before the walk
		// resolves the symlinks ahead of it.
		path, err := f.resolve(root, "/"+dir+"/"+name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			refused = append(refused, err)
			continue
		}
		return path, nil
	}
	if len(refused) > 0 {
		return "", fmt.Errorf("command %s has no trusted copy: %w", name, errors.Join(refused...))
	}
	return "", fmt.Errorf("command %s not found in %s: %w", name, strings.Join(f.dirs, ", "), exec.ErrNotFound)
}

// resolve walks hostPath under root one component at a time, never
// letting the OS follow a symlink, and returns the real path of the
// regular file it ends at. Every directory it walks through, root
// included, must pass trusted before anything in it is read or followed.
// A symlink is followed one hop at a time, and only when owner owns the
// link itself; its directory has already passed. The file it ends at must
// pass trusted too. A missing component is an error wrapping
// fs.ErrNotExist, so the caller can try the next directory.
func (f finder) resolve(root, hostPath string) (string, error) {
	fi, err := f.lstatPath(root)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", root)
	}
	if err := f.trusted(root, root, fi); err != nil {
		return "", err
	}

	// cur is a directory that passed trusted and is not a symlink, so ".."
	// can be resolved against it lexically.
	cur := root
	pending := splitPath(hostPath)
	hops := 0
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		switch name {
		case ".":
			continue
		case "..":
			if cur == root {
				if root == "/" {
					continue
				}
				return "", fmt.Errorf("%s leads outside %s", hostPath, root)
			}
			cur = filepath.Dir(cur)
			continue
		}

		next := filepath.Join(cur, name)
		fi, err := f.lstatPath(next)
		if err != nil {
			return "", err
		}

		if fi.Mode()&fs.ModeSymlink != 0 {
			if err := f.ownedByOwner(next, fi); err != nil {
				return "", fmt.Errorf("symlink %w", err)
			}
			hops++
			if hops > maxSymlinkHops {
				return "", fmt.Errorf("%s: too many symlinks", hostPath)
			}
			dest, err := os.Readlink(next)
			if err != nil {
				return "", err
			}
			// The target is never cleaned: the kernel follows a symlink
			// before the ".." after it, so every component is walked, and
			// checked, in order.
			names := splitPath(dest)
			if filepath.IsAbs(dest) {
				rest, ok := belowRoot(root, names)
				if !ok {
					return "", fmt.Errorf("%s points to %s, outside %s", next, dest, root)
				}
				cur = root
				names = rest
			}
			pending = append(names, pending...)
			continue
		}

		if err := f.trusted(root, next, fi); err != nil {
			return "", err
		}
		if len(pending) > 0 && !fi.IsDir() {
			return "", fmt.Errorf("%s is not a directory", next)
		}
		cur = next
	}

	fi, err = f.lstatPath(cur)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", cur)
	}
	return cur, nil
}

// lstatPath reads path's metadata without following a final symlink.
func (f finder) lstatPath(path string) (fs.FileInfo, error) {
	if f.lstat != nil {
		return f.lstat(path)
	}
	return os.Lstat(path)
}

// splitPath splits a slash-separated path into its components, dropping
// empty ones.
func splitPath(path string) []string {
	return strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
}

// nixStore is the Nix store directory, as a host path. The system package
// cannot import setup, which names it NixStore.
const nixStore = "/nix/store"

// trusted refuses path, a directory or file under root, unless the
// finder's owner owns it and neither its group nor others can write it.
// The host directory /nix/store is the one exception.
func (f finder) trusted(root, path string, fi fs.FileInfo) error {
	if err := f.ownedByOwner(path, fi); err != nil {
		return err
	}
	// The Nix store is group-writable so the nixbld build users can add
	// to it. Its sticky bit means only an entry's owner can rename or
	// remove that entry, and the entries a command resolves through are
	// owned by the owner and read-only, so they still get the normal
	// check. Only the exact host path qualifies: no other sticky,
	// group-writable directory does.
	if hostPath(root, path) == nixStore && fi.Mode()&fs.ModeSticky != 0 && fi.Mode().Perm()&0o002 == 0 {
		return nil
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by its group or others (mode %v)", path, fi.Mode().Perm())
	}
	return nil
}

// hostPath is path, which is root or below it, as a path on the host the
// tree under root stands for. It is "" when path is outside root.
func hostPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	return filepath.Join("/", rel)
}

// ownedByOwner refuses path unless the finder's owner owns it.
func (f finder) ownedByOwner(path string, fi fs.FileInfo) error {
	uid, _, err := owner(fi)
	if err != nil {
		return err
	}
	if uid != f.owner {
		return fmt.Errorf("%s is owned by uid %d, not %d", path, uid, f.owner)
	}
	return nil
}

// belowRoot returns the components of an absolute symlink target that
// follow root, for the walk to resolve from root. The target must start
// with root's own components, name for name: a "." or ".." among them
// would reach root through its parent, which the walk cannot check, so
// such a target is refused. On a real host root is "/" and every absolute
// target qualifies.
func belowRoot(root string, names []string) ([]string, bool) {
	prefix := splitPath(root)
	if len(names) < len(prefix) || !slices.Equal(names[:len(prefix)], prefix) {
		return nil, false
	}
	return names[len(prefix):], true
}

// ExecRunner is the real Runner. It runs the trusted copy of name from
// the fixed system directories, never one found through PATH, with only
// commandEnv as its environment.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return execRunner(hostFinder.find)(ctx, name, args...)
}

// execRunner is a Runner that resolves each command with find when it is
// run.
func execRunner(find func(string) (string, error)) Runner {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		path, err := find(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", commandLine(name, args), err)
		}
		cmd := exec.CommandContext(ctx, path, args...)
		// A multi-call binary such as busybox picks its applet from argv[0].
		cmd.Args[0] = name
		cmd.Env = append([]string(nil), commandEnv...)
		// Never the caller's working directory: a relative argument, such
		// as a file a shell would read, must not resolve against a
		// directory the invoking user controls.
		cmd.Dir = "/"
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return out, commandError(name, args, err, stderr.Bytes())
		}
		return out, nil
	}
}
