// Package daemon runs the chainmail daemon: the Unix socket, peer checks and
// the API handlers.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
	"time"
)

// Listen creates the daemon's Unix socket at path with mode 0660. When gid is
// not negative the socket's group is set to it. A stale socket left by a
// crashed daemon is replaced; a live one is an error.
//
// The socket briefly has the umask's mode before the chmod, so the umask must
// be 0022 or stricter. The socket's directory must be traversable by members
// of the client group: systemd's default RuntimeDirectory mode 0755 works, as
// does 0750 with the directory's group set to the client group.
func Listen(ctx context.Context, path string, gid int) (net.Listener, error) {
	if err := removeStale(ctx, path); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close() // the chmod error is the one to report
		return nil, fmt.Errorf("setting socket mode: %w", err)
	}
	if gid >= 0 {
		if err := os.Lchown(path, -1, gid); err != nil {
			_ = ln.Close() // the chown error is the one to report
			return nil, fmt.Errorf("setting socket group: %w", err)
		}
	}
	return ln, nil
}

func removeStale(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking %s: %w", path, err)
	}
	if info.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err == nil {
		_ = conn.Close() // we only wanted to know if something answers
		return fmt.Errorf("another daemon is listening on %s", path)
	}
	// Only a refused connection proves nobody is listening. Any other error,
	// such as EACCES, may hide a live daemon whose path we must not take over.
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("cannot tell whether %s is live: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing stale socket %s: %w", path, err)
	}
	return nil
}
