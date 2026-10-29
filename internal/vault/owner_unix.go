//go:build unix

package vault

import (
	"io/fs"
	"os"
	"syscall"
)

// fileOwner returns the owner and group of info and the process's effective
// uid. ok is false if the platform did not report ownership.
func fileOwner(info fs.FileInfo) (uid, gid, euid uint32, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return st.Uid, st.Gid, uint32(os.Geteuid()), true
}
