//go:build unix

package system

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// noFollow makes open fail on a final symlink instead of following it.
const noFollow = unix.O_NOFOLLOW

// nonBlock makes open return at once on a FIFO with no writer, so the
// file type can be checked before anything blocks. A regular file ignores
// it.
const nonBlock = unix.O_NONBLOCK

// owner returns the uid and gid of fi.
func owner(fi fs.FileInfo) (uid, gid uint32, err error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("no owner for %s", fi.Name())
	}
	return st.Uid, st.Gid, nil
}

// flock takes an exclusive lock on f, waiting for it.
func flock(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// funlock releases a lock taken by flock.
func funlock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
