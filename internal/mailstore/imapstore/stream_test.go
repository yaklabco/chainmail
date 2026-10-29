package imapstore

import (
	"errors"
	"io"
	"io/fs"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// readAllWithin reads r to the end and fails the test if that takes longer
// than limit.
func readAllWithin(t *testing.T, r io.Reader, limit time.Duration) ([]byte, error) {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	got := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r)
		got <- result{data, err}
	}()
	select {
	case res := <-got:
		return res.data, res.err
	case <-time.After(limit):
		t.Fatalf("reading the stream still running after %v", limit)
		return nil, nil
	}
}

func TestStreamPartIsNotRetriedOnceItStarted(t *testing.T) {
	t.Parallel()
	srv := startScriptServer(t, &scriptServer{n: 1, dropAfter: 1})
	s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
	s.chunk = 8
	_, rc, err := s.StreamPart(t.Context(), scriptRefs(1)[0], "")
	if err != nil {
		t.Fatalf("StreamPart: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	data, err := readAllWithin(t, rc, 5*time.Second)
	if !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("read = %v, want ErrUnavailable once the connection drops mid-stream", err)
	}
	if want := (scriptHeader(1) + scriptBody(1))[:8]; string(data) != want {
		t.Errorf("read %q before the drop, want the first chunk %q", data, want)
	}
	if n := srv.conns.Load(); n != 1 {
		t.Errorf("%d connections, want 1: bytes already went out, so the stream is not retried", n)
	}
}

func TestStreamPartStopsAtThePartSize(t *testing.T) {
	t.Parallel()
	srv := startScriptServer(t, &scriptServer{n: 1, endless: true})
	s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
	s.chunk = 16
	info, rc, err := s.StreamPart(t.Context(), scriptRefs(1)[0], "")
	if err != nil {
		t.Fatalf("StreamPart: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	data, err := readAllWithin(t, rc, 5*time.Second)
	if !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("read = %v, want ErrUnavailable from a server that sends more than the part holds", err)
	}
	if limit := info.Size + s.chunk; int64(len(data)) > limit {
		t.Errorf("read %d bytes of a %d-byte message, want at most %d", len(data), info.Size, limit)
	}
}

func TestPartReaderRefusesReadsAfterClose(t *testing.T) {
	t.Parallel()
	srv := startScriptServer(t, &scriptServer{n: 1})
	s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
	s.chunk = 8
	_, rc, err := s.StreamPart(t.Context(), scriptRefs(1)[0], "")
	if err != nil {
		t.Fatalf("StreamPart: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := rc.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	fetches := srv.fetches.Load()
	// The connection is back in the pool, so a read now must not touch it.
	for range 3 {
		if _, err := rc.Read(buf); !errors.Is(err, fs.ErrClosed) {
			t.Fatalf("Read after Close = %v, want fs.ErrClosed", err)
		}
	}
	if n := srv.fetches.Load(); n != fetches {
		t.Errorf("Read after Close sent %d FETCHes on a pooled connection", n-fetches)
	}
}

func TestPartReaderCloseDuringRead(t *testing.T) {
	t.Parallel()
	srv := startScriptServer(t, &scriptServer{n: 1, mode: slowly, delay: 20 * time.Millisecond})
	s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
	s.chunk = 2
	_, rc, err := s.StreamPart(t.Context(), scriptRefs(1)[0], "")
	if err != nil {
		t.Fatalf("StreamPart: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, rc)
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	if err := rc.Close(); err != nil {
		t.Errorf("Close during a Read: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, fs.ErrClosed) {
			t.Errorf("Read after a concurrent Close = %v, want fs.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reader kept going after Close")
	}
}
