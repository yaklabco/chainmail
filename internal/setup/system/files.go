package system

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// OSFiles is the real Files. It never follows a final symlink: Lstat
// reports one, ReadFile and Lock refuse one, EnsureDir refuses anything
// that is not a directory, and WriteFile replaces a link instead of
// writing through it.
type OSFiles struct{}

var _ Files = OSFiles{}

// Lstat reads path's metadata without following a final symlink. A missing
// path is FileInfo{Exists: false} with no error.
func (OSFiles) Lstat(path string) (FileInfo, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return FileInfo{}, nil
	}
	if err != nil {
		return FileInfo{}, fmt.Errorf("inspecting %s: %w", path, err)
	}
	uid, gid, err := owner(fi)
	if err != nil {
		return FileInfo{}, fmt.Errorf("inspecting %s: %w", path, err)
	}
	info := FileInfo{Exists: true, Mode: fi.Mode(), UID: uid, GID: gid}
	if fi.Mode()&fs.ModeSymlink != 0 {
		if info.LinkDest, err = os.Readlink(path); err != nil {
			return FileInfo{}, fmt.Errorf("reading link %s: %w", path, err)
		}
	}
	return info, nil
}

// ReadFile reads path, refusing a final symlink.
func (OSFiles) ReadFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only; nothing to flush
	data, err := readAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return data, nil
}

// EnsureDir creates path if missing, then sets owner and mode. Its parent
// must exist. An existing path that is not a directory, including a
// symlink to one, is an error.
func (OSFiles) EnsureDir(path string, uid, gid uint32, mode fs.FileMode) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", path, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", path)
	}
	if err := os.Lchown(path, int(uid), int(gid)); err != nil {
		return fmt.Errorf("setting owner of %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("setting mode of %s: %w", path, err)
	}
	return nil
}

// WriteFile replaces path atomically: it writes a temp file beside path,
// sets its owner and mode, syncs it, renames it over path and syncs the
// directory. A crash leaves the old file or the new one.
func (OSFiles) WriteFile(path string, data []byte, uid, gid uint32, mode fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name()) // best effort; the write already failed
		}
	}()
	if err = writeAndSync(tmp, data, uid, gid, mode); err != nil {
		_ = tmp.Close() // the earlier error is the one to report
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file for %s: %w", path, err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return syncDir(dir)
}

// CreateExclusive creates path, which must not exist (a symlink counts as
// existing), with the given owner and mode, then writes and syncs data. An
// existing path is an error wrapping fs.ErrExist. If any later step fails
// the new file is removed.
func (OSFiles) CreateExclusive(path string, data []byte, uid, gid uint32, mode fs.FileMode) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, mode.Perm())
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(path) // best effort; the create already failed
		}
	}()
	if err = writeAndSync(f, data, uid, gid, mode); err != nil {
		_ = f.Close() // the earlier error is the one to report
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	return syncDir(filepath.Dir(path))
}

// Lock takes an exclusive flock on path, creating it with mode 0600, and
// waits until it gets it. unlock releases the lock and closes the file.
func (OSFiles) Lock(path string) (func() error, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|noFollow, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock %s: %w", path, err)
	}
	if err := flock(f); err != nil {
		_ = f.Close() // the lock error is the one to report
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	unlock := func() error {
		uerr := funlock(f)
		cerr := f.Close()
		if uerr != nil {
			return fmt.Errorf("unlocking %s: %w", path, uerr)
		}
		if cerr != nil {
			return fmt.Errorf("closing lock %s: %w", path, cerr)
		}
		return nil
	}
	return unlock, nil
}

// writeAndSync sets f's owner and mode, writes data and syncs it. The
// owner and mode come first, so the data is never readable by anyone the
// caller did not choose.
func writeAndSync(f *os.File, data []byte, uid, gid uint32, mode fs.FileMode) error {
	if err := f.Chown(int(uid), int(gid)); err != nil {
		return fmt.Errorf("setting owner: %w", err)
	}
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("setting mode: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing: %w", err)
	}
	return nil
}

// readAll reads a regular file to the end. Anything else, such as a FIFO
// that would block, is refused.
func readAll(f *os.File) ([]byte, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspecting: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", fi.Name())
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading: %w", err)
	}
	return data, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening %s: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close() // the sync error is the one to report
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", dir, err)
	}
	return nil
}
