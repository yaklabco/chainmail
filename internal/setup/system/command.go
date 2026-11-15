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
	"strings"
)

// trustedDirs are the only directories bootstrap runs host commands from,
// searched in this order. PATH is never consulted: under sudo it can
// still name a directory the invoking user, or an agent of theirs, can
// write.
var trustedDirs = []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// commandEnv is the whole environment of a host command. Nothing is
// inherited from the caller.
var commandEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}

// hostFinder finds host commands on the real system: in trustedDirs, owned
// by root.
var hostFinder = finder{root: "/", dirs: trustedDirs, owner: 0}

// finder resolves a bare command name to the absolute path of a file only
// owner can change. root is "/" on a real host and a test tree in tests;
// dirs are host paths under it.
type finder struct {
	root  string
	dirs  []string
	owner uint32
}

// LookupTrusted returns the absolute path, with every symlink resolved, of
// the command name in the first of dirs that holds a trusted copy. A copy
// is trusted when it resolves to a regular file that root owns and only
// root can write, in directories that root owns and only root can write,
// all the way up to "/". A missing command is an error wrapping
// exec.ErrNotFound.
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
		path, err := f.check(root, filepath.Join(root, dir, name))
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

// check resolves candidate and returns the resolved path when it is a
// regular file that it, and every directory above it up to root, is
// trusted.
func (f finder) check(root, candidate string) (string, error) {
	path, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	if !within(root, path) {
		return "", fmt.Errorf("%s resolves to %s, outside %s", candidate, path, root)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if err := f.trusted(path, fi); err != nil {
		return "", err
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		fi, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%s is not a directory", dir)
		}
		if err := f.trusted(dir, fi); err != nil {
			return "", err
		}
		if dir == root || dir == filepath.Dir(dir) {
			return path, nil
		}
	}
}

// trusted refuses path unless the finder's owner owns it and neither its
// group nor others can write it.
func (f finder) trusted(path string, fi fs.FileInfo) error {
	uid, _, err := owner(fi)
	if err != nil {
		return err
	}
	if uid != f.owner {
		return fmt.Errorf("%s is owned by uid %d, not %d", path, uid, f.owner)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by its group or others (mode %v)", path, fi.Mode().Perm())
	}
	return nil
}

// within reports whether path is root or below it.
func within(root, path string) bool {
	if root == "/" {
		return filepath.IsAbs(path)
	}
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
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
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return out, commandError(name, args, err, stderr.Bytes())
		}
		return out, nil
	}
}
