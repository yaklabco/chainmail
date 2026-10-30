package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// clock is a settable time source that is safe to read from many goroutines.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newStore(t *testing.T, ttl time.Duration) (*Store, string, *clock) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "uploads")
	c := newClock()
	s, err := New(dir, ttl, c.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, dir, c
}

// dirNames lists the entries in dir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

var handleRE = regexp.MustCompile(`^up_[a-z2-7]{26}$`)

func TestPutOpenRoundTrip(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t, time.Hour)
	data := []byte("hello, attachment\n")
	info, err := s.Put(t.Context(), "notes.txt", "text/plain", bytes.NewReader(data), 1024)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !handleRE.MatchString(info.Handle) {
		t.Errorf("handle = %q, want up_ plus 26 lowercase base32", info.Handle)
	}
	want := Info{
		Handle:      info.Handle,
		Filename:    "notes.txt",
		ContentType: "text/plain",
		Size:        int64(len(data)),
		SHA256:      sha256.Sum256(data),
	}
	if info != want {
		t.Errorf("Put info = %+v, want %+v", info, want)
	}
	got, body, err := s.Open(info.Handle)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != want {
		t.Errorf("Open info = %+v, want %+v", got, want)
	}
	if !bytes.Equal(body, data) {
		t.Errorf("Open bytes = %q, want %q", body, data)
	}
	// A handle can be used more than once while it lives.
	if _, again, err := s.Open(info.Handle); err != nil || !bytes.Equal(again, data) {
		t.Errorf("second Open = %q, %v", again, err)
	}
}

func TestPutExactlyMaxBytes(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t, time.Hour)
	data := bytes.Repeat([]byte("x"), 64)
	info, err := s.Put(t.Context(), "a.bin", "application/octet-stream", bytes.NewReader(data), 64)
	if err != nil {
		t.Fatalf("Put at the limit: %v", err)
	}
	if info.Size != 64 {
		t.Errorf("Size = %d, want 64", info.Size)
	}
}

func TestPutEmpty(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t, time.Hour)
	info, err := s.Put(t.Context(), "empty", "", strings.NewReader(""), 10)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if info.Size != 0 || info.SHA256 != sha256.Sum256(nil) {
		t.Errorf("info = %+v", info)
	}
	if _, body, err := s.Open(info.Handle); err != nil || len(body) != 0 {
		t.Errorf("Open = %q, %v", body, err)
	}
}

func TestPermissions(t *testing.T) {
	t.Parallel()
	s, dir, _ := newStore(t, time.Hour)
	info, err := s.Put(t.Context(), "f.txt", "text/plain", strings.NewReader("secret"), 100)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 700", perm)
	}
	names := dirNames(t, dir)
	if len(names) != 1 || names[0] != info.Handle {
		t.Fatalf("dir entries = %v, want only %s", names, info.Handle)
	}
	fileInfo, err := os.Stat(filepath.Join(dir, info.Handle))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
}

func TestNewTightensExistingDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "uploads")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir, time.Hour, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 700", perm)
	}
}

func TestPutTooLargeLeavesNoFile(t *testing.T) {
	t.Parallel()
	s, dir, _ := newStore(t, time.Hour)
	data := bytes.Repeat([]byte("y"), 65)
	_, err := s.Put(t.Context(), "big.bin", "application/octet-stream", bytes.NewReader(data), 64)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Put err = %v, want ErrTooLarge", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("dir entries after refused upload = %v, want none", names)
	}
	if n := s.Sweep(); n != 0 {
		t.Errorf("Sweep = %d, want 0", n)
	}
}

// failingReader returns some bytes and then an error.
type failingReader struct{ sent bool }

var errBoom = errors.New("boom")

func (r *failingReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errBoom
	}
	r.sent = true
	return copy(p, "partial"), nil
}

func TestPutReadErrorLeavesNoFile(t *testing.T) {
	t.Parallel()
	s, dir, _ := newStore(t, time.Hour)
	_, err := s.Put(t.Context(), "f", "text/plain", &failingReader{}, 1024)
	if !errors.Is(err, errBoom) {
		t.Fatalf("Put err = %v, want errBoom", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("dir entries = %v, want none", names)
	}
}

func TestPutCanceledContext(t *testing.T) {
	t.Parallel()
	s, dir, _ := newStore(t, time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.Put(ctx, "f", "text/plain", strings.NewReader("data"), 1024)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put err = %v, want context.Canceled", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("dir entries = %v, want none", names)
	}
}

func TestOpenUnknownHandle(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t, time.Hour)
	for _, h := range []string{"", "up_aaaaaaaaaaaaaaaaaaaaaaaaaa", "../uploads", "nope"} {
		if _, _, err := s.Open(h); !errors.Is(err, ErrUnknownHandle) {
			t.Errorf("Open(%q) err = %v, want ErrUnknownHandle", h, err)
		}
	}
}

func TestOpenExpiredHandle(t *testing.T) {
	t.Parallel()
	s, dir, c := newStore(t, time.Hour)
	info, err := s.Put(t.Context(), "f.txt", "text/plain", strings.NewReader("data"), 100)
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Hour - time.Second)
	if _, _, err := s.Open(info.Handle); err != nil {
		t.Fatalf("Open before expiry: %v", err)
	}
	c.Advance(time.Second)
	if _, _, err := s.Open(info.Handle); !errors.Is(err, ErrUnknownHandle) {
		t.Fatalf("Open after expiry err = %v, want ErrUnknownHandle", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("dir entries after expired Open = %v, want none", names)
	}
}

func TestSweepCounts(t *testing.T) {
	t.Parallel()
	s, dir, c := newStore(t, time.Hour)
	put := func(name string) Info {
		t.Helper()
		info, err := s.Put(t.Context(), name, "text/plain", strings.NewReader(name), 100)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	put("old1")
	put("old2")
	c.Advance(30 * time.Minute)
	fresh := put("fresh")
	if n := s.Sweep(); n != 0 {
		t.Errorf("first Sweep = %d, want 0", n)
	}
	c.Advance(30 * time.Minute)
	if n := s.Sweep(); n != 2 {
		t.Errorf("Sweep at 1h = %d, want 2", n)
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != fresh.Handle {
		t.Errorf("dir entries = %v, want only %s", names, fresh.Handle)
	}
	if _, _, err := s.Open(fresh.Handle); err != nil {
		t.Errorf("Open fresh: %v", err)
	}
	if n := s.Sweep(); n != 0 {
		t.Errorf("repeat Sweep = %d, want 0", n)
	}
	c.Advance(30 * time.Minute)
	if n := s.Sweep(); n != 1 {
		t.Errorf("final Sweep = %d, want 1", n)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("dir entries = %v, want none", names)
	}
}

func TestNewClearsLeftovers(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "uploads")
	first, err := New(dir, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := first.Put(t.Context(), "f", "text/plain", strings.NewReader("old"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tmp-stale"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := New(dir, time.Hour, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("dir entries after New = %v, want none", names)
	}
	if _, _, err := second.Open(old.Handle); !errors.Is(err, ErrUnknownHandle) {
		t.Errorf("Open of earlier run's handle err = %v, want ErrUnknownHandle", err)
	}
}

func TestFilenameIsMetadataOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "state", "uploads")
	s, err := New(dir, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	const evil = "../../etc/passwd"
	info, err := s.Put(t.Context(), evil, "text/plain", strings.NewReader("data"), 100)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if info.Filename != evil {
		t.Errorf("Filename = %q, want %q", info.Filename, evil)
	}
	if strings.Contains(info.Handle, "passwd") || strings.ContainsAny(info.Handle, "./") {
		t.Errorf("handle %q carries the filename", info.Handle)
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != info.Handle {
		t.Errorf("dir entries = %v, want only %s", names, info.Handle)
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s/etc err = %v, want not exist", root, err)
	}
	got, _, err := s.Open(info.Handle)
	if err != nil || got.Filename != evil {
		t.Errorf("Open = %+v, %v", got, err)
	}
}

func TestConcurrentPuts(t *testing.T) {
	t.Parallel()
	s, dir, _ := newStore(t, time.Hour)
	const n = 32
	infos := make([]Info, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			body := fmt.Sprintf("payload %d", i)
			infos[i], errs[i] = s.Put(t.Context(), fmt.Sprintf("f%d.txt", i), "text/plain", strings.NewReader(body), 1024)
		})
	}
	wg.Wait()
	seen := make(map[string]bool, n)
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Put %d: %v", i, errs[i])
		}
		h := infos[i].Handle
		if seen[h] {
			t.Fatalf("duplicate handle %s", h)
		}
		seen[h] = true
		_, body, err := s.Open(h)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if want := fmt.Sprintf("payload %d", i); string(body) != want {
			t.Errorf("body %d = %q, want %q", i, body, want)
		}
	}
	if got := len(dirNames(t, dir)); got != n {
		t.Errorf("dir has %d entries, want %d", got, n)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t, time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// cancelingReader cancels its context after the first chunk.
type cancelingReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		r.cancel()
		return copy(p, "chunk"), nil
	}
	return copy(p, "more"), nil
}

func TestPutCanceledMidStream(t *testing.T) {
	t.Parallel()
	s, dir, _ := newStore(t, time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := s.Put(ctx, "f", "text/plain", &cancelingReader{cancel: cancel}, 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put err = %v, want context.Canceled", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("dir entries = %v, want none", names)
	}
}

func TestSweepRetriesFailedRemoval(t *testing.T) {
	t.Parallel()
	s, dir, c := newStore(t, time.Hour)
	info, err := s.Put(t.Context(), "a.txt", "text/plain", strings.NewReader("secret"), 100)
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Hour)
	// A read-only directory makes the removal fail.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if n := s.Sweep(); n != 0 {
		t.Errorf("Sweep with a failing removal = %d, want 0", n)
	}
	if _, _, err := s.Open(info.Handle); !errors.Is(err, ErrUnknownHandle) {
		t.Errorf("Open of an expired upload err = %v, want ErrUnknownHandle", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if n := s.Sweep(); n != 1 {
		t.Errorf("Sweep after the failure cleared = %d, want 1", n)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("dir entries = %v, want none", names)
	}
}

func TestPutMaxInt64Limit(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t, time.Hour)
	info, err := s.Put(t.Context(), "a.txt", "text/plain", strings.NewReader("hello"), math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 5 {
		t.Errorf("Size = %d, want 5", info.Size)
	}
}

func TestPutNegativeLimit(t *testing.T) {
	t.Parallel()
	s, _, _ := newStore(t, time.Hour)
	if _, err := s.Put(t.Context(), "a.txt", "text/plain", strings.NewReader("hello"), -1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Put with a negative limit err = %v, want ErrTooLarge", err)
	}
}

func TestRunSweepsEveryMinute(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "uploads")
		s, err := New(dir, time.Minute, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(t.Context(), "a.txt", "text/plain", strings.NewReader("x"), 10); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			s.Run(ctx)
			close(done)
		}()
		time.Sleep(sweepInterval + time.Second)
		synctest.Wait()
		if names := dirNames(t, dir); len(names) != 0 {
			t.Errorf("dir entries after a sweep interval = %v, want none", names)
		}
		cancel()
		<-done
	})
}

// TestStatReadsNoBytes stats a live upload whose file is gone: the info
// comes from the index alone. Unknown and expired handles are unknown.
func TestStatReadsNoBytes(t *testing.T) {
	t.Parallel()
	s, dir, c := newStore(t, time.Hour)
	info, err := s.Put(t.Context(), "f.bin", "application/octet-stream", strings.NewReader("twelve bytes"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, info.Handle)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Stat(info.Handle)
	if err != nil || got != info {
		t.Errorf("Stat = %+v, %v, want %+v", got, err, info)
	}
	if _, err := s.Stat("up_aaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrUnknownHandle) {
		t.Errorf("Stat of an unknown handle err = %v, want ErrUnknownHandle", err)
	}
	c.Advance(time.Hour)
	if _, err := s.Stat(info.Handle); !errors.Is(err, ErrUnknownHandle) {
		t.Errorf("Stat after expiry err = %v, want ErrUnknownHandle", err)
	}
}
