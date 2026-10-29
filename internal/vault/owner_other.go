//go:build !unix

package vault

import "io/fs"

// fileOwner reports no ownership on platforms without Unix file owners, so
// LoadIdentity falls back to its strict mode check.
func fileOwner(fs.FileInfo) (uid, gid, euid uint32, ok bool) {
	return 0, 0, 0, false
}
