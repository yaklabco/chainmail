//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
)

func peerCredentials(net.Conn) (Peer, error) {
	return Peer{}, errors.New("peer credentials are only supported on Linux and macOS")
}
