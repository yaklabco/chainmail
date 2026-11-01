//go:build unix

package mcpserver

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedByMe reports whether st belongs to the process's effective user.
func ownedByMe(st fs.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && sys.Uid == uint32(os.Geteuid())
}
