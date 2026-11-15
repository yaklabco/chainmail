//go:build unix

package system

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadFileRefusesFIFO(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	got := make(chan error, 1)
	go func() {
		_, err := OSFiles{}.ReadFile(path)
		got <- err
	}()
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("ReadFile accepted a FIFO")
		}
	case <-time.After(2 * time.Second):
		// A writer releases the stuck open so the goroutine can finish.
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("ReadFile blocked on a FIFO with no writer")
	}
}
