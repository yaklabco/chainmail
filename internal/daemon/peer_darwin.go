package daemon

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerCredentials reads the peer's user and group with LOCAL_PEERCRED and
// its process ID with LOCAL_PEERPID, macOS's counterparts of Linux's
// SO_PEERCRED. The first group of the xucred is the effective group.
func peerCredentials(c net.Conn) (Peer, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return Peer{}, fmt.Errorf("connection is %T, not a Unix socket", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Peer{}, fmt.Errorf("raw connection: %w", err)
	}
	var (
		cred    *unix.Xucred
		pid     int
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if credErr == nil {
			pid, credErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		}
	}); err != nil {
		return Peer{}, fmt.Errorf("reading peer credentials: %w", err)
	}
	if credErr != nil {
		return Peer{}, fmt.Errorf("reading peer credentials: %w", credErr)
	}
	if cred.Ngroups < 1 {
		return Peer{}, errors.New("reading peer credentials: the peer has no group")
	}
	return Peer{UID: cred.Uid, GID: cred.Groups[0], PID: int32(pid)}, nil
}
