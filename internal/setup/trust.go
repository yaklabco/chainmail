package setup

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
)

// maxSymlinkHops is how many symlinks CheckTrust follows before it treats
// the path as a loop, matching Linux's limit.
const maxSymlinkHops = 40

// NixStore is the Nix store directory. CheckTrust accepts it, and only it,
// while it is owned by root, has the sticky bit and is not writable by
// others, although its group (nixbld) may write it.
const NixStore = "/nix/store"

// FileInfo is what the trust check needs to know about a path, read
// without following a final symlink.
type FileInfo struct {
	Exists   bool
	Mode     fs.FileMode // includes fs.ModeSymlink and fs.ModeDir
	UID, GID uint32
	LinkDest string // set when Mode has fs.ModeSymlink: the raw readlink value
}

// FSView reads path metadata. A missing path is FileInfo{Exists: false}, nil.
type FSView interface {
	Lstat(path string) (FileInfo, error)
}

// CheckTrust checks path and every ancestor up to "/". trusted holds the
// uids that may own them (root, and the daemon user once it exists). It
// returns a *Refusal naming the first unsafe path, or nil.
//
// Each existing component must be owned by a trusted uid and must not be
// writable by group or others. NixStore is the one exception, see
// checkOwnerMode. The final component must not be a symlink.
// An ancestor that is a symlink is accepted only when root owns the link
// and its directory passed the check, and then the check continues at the
// link's target. A missing component passes, with everything below it,
// because root will create it.
func CheckTrust(fsv FSView, path string, trusted []uint32) error {
	if !filepath.IsAbs(path) {
		return unsafePath(path, "is not absolute")
	}
	if hasDotDot(path) {
		return unsafePath(path, "contains ..")
	}

	root, err := lstat(fsv, "/")
	if err != nil {
		return err
	}
	if !root.Exists {
		return nil
	}
	if err := checkOwnerMode("/", root, trusted); err != nil {
		return err
	}

	// cur is a directory already checked and known not to be a symlink, so
	// ".." in a link target can be resolved against it lexically. pending
	// holds the components still to walk; its last element is always the
	// final component of the original path, because only ancestors are
	// expanded.
	cur := "/"
	pending := splitPath(filepath.Clean(path))
	hops := 0
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}

		next := filepath.Join(cur, name)
		info, err := lstat(fsv, next)
		if err != nil {
			return err
		}
		if !info.Exists {
			return nil
		}
		leaf := len(pending) == 0

		if info.Mode&fs.ModeSymlink != 0 {
			if leaf {
				return unsafePath(next, "is a symlink")
			}
			if info.UID != 0 {
				return unsafePath(next, fmt.Sprintf("is a symlink owned by uid %d", info.UID))
			}
			hops++
			if hops > maxSymlinkHops {
				return unsafePath(path, "too many symlinks")
			}
			if filepath.IsAbs(info.LinkDest) {
				cur = "/"
			}
			pending = append(splitPath(info.LinkDest), pending...)
			continue
		}

		if err := checkOwnerMode(next, info, trusted); err != nil {
			return err
		}
		if !leaf && !info.Mode.IsDir() {
			return unsafePath(next, "is not a directory")
		}
		cur = next
	}
	return nil
}

// checkOwnerMode refuses a path owned by an untrusted uid or writable by
// group or others, except for NixStore.
func checkOwnerMode(path string, info FileInfo, trusted []uint32) error {
	// The Nix store is group-writable so the nixbld build users can add
	// to it. Its sticky bit means only an entry's owner can rename or
	// remove that entry, and the entries bootstrap resolves are root-owned
	// and read-only, so they still get the normal check. Only the exact
	// path qualifies: no other sticky, group-writable directory does.
	if path == NixStore && info.UID == 0 && info.Mode&fs.ModeSticky != 0 && info.Mode.Perm()&0o002 == 0 {
		return nil
	}
	if !slices.Contains(trusted, info.UID) {
		return unsafePath(path, fmt.Sprintf("owned by uid %d", info.UID))
	}
	if info.Mode.Perm()&0o022 != 0 {
		return unsafePath(path, "writable by group or others")
	}
	return nil
}

func lstat(fsv FSView, path string) (FileInfo, error) {
	info, err := fsv.Lstat(path)
	if err != nil {
		return FileInfo{}, fmt.Errorf("check trust of %s: %w", path, err)
	}
	return info, nil
}

// splitPath splits a slash-separated path into its components, dropping
// empty ones.
func splitPath(path string) []string {
	return strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' })
}

func unsafePath(path, reason string) error {
	return refuse(
		"bootstrap only trusts paths that root owns; fix the ownership or mode of "+path+" and run it again",
		"unsafe path %s: %s", path, reason,
	)
}
