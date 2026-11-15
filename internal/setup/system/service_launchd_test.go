package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// logService is the real launchd Service reading the log at path.
func logService(path string) Service {
	return NewService("darwin", label, plistPath, path, newFakeRunner().run)
}

// writeLog writes data to a new log file and returns its path.
func writeLog(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// numbered is lines "line 1" to "line n", each padded to width bytes
// before its newline.
func numbered(n, width int) []byte {
	var b bytes.Buffer
	for i := 1; i <= n; i++ {
		line := fmt.Sprintf("line %d", i)
		b.WriteString(line + strings.Repeat(".", max(0, width-len(line))) + "\n")
	}
	return b.Bytes()
}

func lines(out []byte) []string {
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

func TestLaunchdLogs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("last 20 lines", func(t *testing.T) {
		t.Parallel()
		out, err := logService(writeLog(t, numbered(30, 0))).Logs(ctx, 20)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		got := lines(out)
		if len(got) != 20 || got[0] != "line 11" || got[19] != "line 30" {
			t.Fatalf("Logs returned %d lines from %q to %q", len(got), got[0], got[len(got)-1])
		}
	})

	t.Run("short file", func(t *testing.T) {
		t.Parallel()
		out, err := logService(writeLog(t, []byte("one\ntwo"))).Logs(ctx, 20)
		if err != nil || string(out) != "one\ntwo" {
			t.Fatalf("Logs = %q, %v", out, err)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		t.Parallel()
		out, err := logService(writeLog(t, nil)).Logs(ctx, 20)
		if err != nil || len(out) != 0 {
			t.Fatalf("Logs = %q, %v, want empty", out, err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		out, err := logService(filepath.Join(t.TempDir(), "daemon.log")).Logs(ctx, 20)
		if err != nil || len(out) != 0 {
			t.Fatalf("Logs = %q, %v, want empty", out, err)
		}
	})

	t.Run("large file", func(t *testing.T) {
		t.Parallel()
		// 8 MiB of 100-byte lines: the last 20 sit in the last chunk.
		out, err := logService(writeLog(t, numbered(80_000, 99))).Logs(ctx, 20)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		got := lines(out)
		if len(got) != 20 || !strings.HasPrefix(got[0], "line 79981.") || !strings.HasPrefix(got[19], "line 80000.") {
			t.Fatalf("Logs returned %d lines from %.12q to %.12q", len(got), got[0], got[len(got)-1])
		}
	})

	t.Run("lines spanning several chunks", func(t *testing.T) {
		t.Parallel()
		// 20 lines of 3000 bytes need more than one chunk but fit the cap.
		out, err := logService(writeLog(t, numbered(100, 2999))).Logs(ctx, 20)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		got := lines(out)
		if len(got) != 20 || !strings.HasPrefix(got[0], "line 81.") || !strings.HasPrefix(got[19], "line 100.") {
			t.Fatalf("Logs returned %d lines from %.12q to %.12q", len(got), got[0], got[len(got)-1])
		}
	})

	t.Run("long lines are capped", func(t *testing.T) {
		t.Parallel()
		// 20 lines of 10 KiB do not fit the cap: only whole lines from the
		// last 64 KiB come back.
		out, err := logService(writeLog(t, numbered(40, 10239))).Logs(ctx, 20)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		if len(out) > maxLogTail {
			t.Fatalf("Logs returned %d bytes, more than %d", len(out), maxLogTail)
		}
		got := lines(out)
		if len(got) != 6 || !strings.HasPrefix(got[0], "line 35.") || !strings.HasPrefix(got[5], "line 40.") {
			t.Fatalf("Logs returned %d lines from %.12q to %.12q", len(got), got[0], got[len(got)-1])
		}
	})

	t.Run("one huge line", func(t *testing.T) {
		t.Parallel()
		out, err := logService(writeLog(t, bytes.Repeat([]byte("x"), 1<<20))).Logs(ctx, 20)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		if len(out) != maxLogTail {
			t.Fatalf("Logs returned %d bytes, want the last %d", len(out), maxLogTail)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		out, err := logService(writeLog(t, numbered(30, 0))).Logs(cancelled, 20)
		if !errors.Is(err, context.Canceled) || out != nil {
			t.Fatalf("Logs with a cancelled context = %q, %v, want context.Canceled", out, err)
		}
	})

	t.Run("final symlink", func(t *testing.T) {
		t.Parallel()
		target := writeLog(t, []byte("secret\n"))
		link := filepath.Join(t.TempDir(), "daemon.log")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if out, err := logService(link).Logs(ctx, 20); err == nil {
			t.Fatalf("Logs followed a final symlink and returned %q", out)
		}
	})

	t.Run("FIFO", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.log")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := logService(path).Logs(ctx, 20)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("Logs read a FIFO")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Logs blocked on a FIFO")
		}
	})
}

// countingReader counts the bytes read through it and runs after once,
// after the first read.
type countingReader struct {
	r     io.ReaderAt
	mu    sync.Mutex
	read  int
	calls int
	after func()
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.read += n
	c.calls++
	if c.calls == 1 && c.after != nil {
		c.after()
	}
	return n, err
}

func TestTailLogReadsAtMostTheCap(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("y"), 4<<20)
	r := &countingReader{r: bytes.NewReader(data)}
	out, err := tailLog(context.Background(), r, int64(len(data)), 20)
	if err != nil {
		t.Fatalf("tailLog: %v", err)
	}
	if r.read > maxLogTail || len(out) > maxLogTail {
		t.Fatalf("tailLog read %d bytes and returned %d, want at most %d", r.read, len(out), maxLogTail)
	}
	if r.calls < 2 {
		t.Fatalf("tailLog read in %d calls, want bounded chunks", r.calls)
	}
}

func TestTailLogStopsWhenCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data := bytes.Repeat([]byte("z"), 4<<20)
	r := &countingReader{r: bytes.NewReader(data), after: cancel}
	out, err := tailLog(ctx, r, int64(len(data)), 20)
	if !errors.Is(err, context.Canceled) || out != nil {
		t.Fatalf("tailLog cancelled after one chunk = %d bytes, %v, want context.Canceled", len(out), err)
	}
	if r.calls != 1 {
		t.Fatalf("tailLog read %d chunks after it was cancelled, want 1", r.calls)
	}
}
