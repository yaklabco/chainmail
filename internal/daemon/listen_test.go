package daemon

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// socketDir returns a short temp dir. Unix socket paths are limited to 108 bytes,
// and t.TempDir paths can be longer.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestListenSetsMode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(socketDir(t), "s.sock")
	ln, err := Listen(t.Context(), path, -1)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() }) // a close error at test end changes nothing
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Type() != fs.ModeSocket {
		t.Errorf("type = %v, want socket", info.Mode().Type())
	}
	if perm := info.Mode().Perm(); perm != 0o660 {
		t.Errorf("mode = %v, want 0660", perm)
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(socketDir(t), "s.sock")
	old, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ul, ok := old.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener is %T", old)
	}
	ul.SetUnlinkOnClose(false)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(t.Context(), path, -1)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestListenRefusesLiveSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(socketDir(t), "s.sock")
	ln, err := Listen(t.Context(), path, -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() }) // a close error at test end changes nothing
	if _, err := Listen(t.Context(), path, -1); err == nil || !strings.Contains(err.Error(), "another daemon") {
		t.Errorf("second Listen err = %v, want another daemon", err)
	}
}

func TestListenRefusesRegularFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(socketDir(t), "s.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(t.Context(), path, -1); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("err = %v, want not a socket", err)
	}
}
