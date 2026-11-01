//go:build !unix

package mcpserver

import "io/fs"

// ownedByMe reports true on platforms without Unix file owners, so the
// directory checks fall back to their mode bits.
func ownedByMe(fs.FileInfo) bool { return true }
