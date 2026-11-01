package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMCPCommandSpeaksMCPOnStdio(t *testing.T) {
	t.Parallel()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr syncBuffer
	home := t.TempDir()
	env := Env{
		Stdin: inR, Stdout: outW, Stderr: &stderr,
		Getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
	}
	done := make(chan int, 1)
	go func() {
		done <- Execute(t.Context(), []string{"--socket", "/nonexistent/chainmail.sock", "mcp"}, env)
		_ = outW.Close() // ends the reader below if the command exits early
		_ = inR.Close()  // and fails the write below instead of blocking it
	}()
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n"
	if _, err := io.WriteString(inW, req); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(outR).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the response: %v (stderr %s)", err, stderr.String())
	}
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			ProtocolVersion string                     `json:"protocolVersion"`
			ServerInfo      struct{ Name string }      `json:"serverInfo"`
			Capabilities    map[string]json.RawMessage `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("stdout line is not JSON-RPC: %q", line)
	}
	if resp.ID != 1 || resp.Result.ProtocolVersion != "2025-11-25" || resp.Result.ServerInfo.Name != "chainmail" {
		t.Errorf("initialize result = %+v", resp)
	}
	if _, ok := resp.Result.Capabilities["logging"]; ok {
		t.Error("the logging capability is advertised")
	}
	_ = inW.Close() // stdin EOF ends the session
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code %d, stderr %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("chainmail mcp did not exit on stdin EOF")
	}
}

func TestMCPCommandClosesStdinWhenCancelled(t *testing.T) {
	t.Parallel()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr syncBuffer
	home := t.TempDir()
	env := Env{
		Stdin: inR, Stdout: outW, Stderr: &stderr,
		Getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- Execute(ctx, []string{"--socket", "/nonexistent/chainmail.sock", "mcp"}, env)
		_ = outW.Close() // ends the reader below if the command exits early
	}()
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n"
	if _, err := io.WriteString(inW, req); err != nil {
		t.Fatal(err)
	}
	// stdin stays open: only the context ends the session.
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code %d, stderr %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("chainmail mcp did not exit when its context ended")
	}
	// With stdin closed, a write fails at once. Were it still open, the
	// SDK's reader goroutine would still be blocked reading it and would
	// take the write.
	wrote := make(chan error, 1)
	go func() {
		_, err := io.WriteString(inW, "\n")
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("writing to stdin after exit: %v, want io.ErrClosedPipe (stdin not closed)", err)
		}
	case <-time.After(10 * time.Second):
		_ = inR.Close() // unblocks the write
		t.Fatal("stdin is neither read nor closed after exit")
	}
}

// TestMCPCommandKeepsClientTextOutOfStderr: a session that fails on a frame
// the client sent must not echo that frame's text to stderr, through the
// server's log or through the CLI's own error report, and still fails.
func TestMCPCommandKeepsClientTextOutOfStderr(t *testing.T) {
	t.Parallel()
	const secret = "SENTINEL-hunter2-Bearer-ya29"
	var stdout bytes.Buffer
	var stderr syncBuffer
	home := t.TempDir()
	env := Env{
		Stdin:  io.NopCloser(strings.NewReader(`{"jsonrpc":"` + secret + `","id":1,"method":"tools/list"}` + "\n")),
		Stdout: &stdout, Stderr: &stderr,
		Getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
	}
	code := Execute(t.Context(), []string{"--socket", "/nonexistent/chainmail.sock", "mcp"}, env)
	if strings.Contains(stderr.String(), secret) || strings.Contains(stdout.String(), secret) {
		t.Errorf("client-controlled text reached the output: stderr %q, stdout %q", stderr.String(), stdout.String())
	}
	if code == 0 {
		t.Errorf("exit code 0 for a failed session (stderr %q)", stderr.String())
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use, for a stderr the
// command may still be writing while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// cancelOnLine is a stderr that cancels a context, with cause, once a line
// holding line is written to it.
type cancelOnLine struct {
	syncBuffer
	line   string
	cancel func()
}

func (w *cancelOnLine) Write(p []byte) (int, error) {
	n, err := w.syncBuffer.Write(p)
	if strings.Contains(string(p), w.line) {
		w.cancel()
	}
	return n, err
}

// TestMCPCommandLogsTheSignalAndARacingError: a session that fails just as
// a signal ends the command exits 0, as a signal does. Its stderr names
// the signal from the context's cause, and the session's error by kind
// (logged by the server), never by its text.
func TestMCPCommandLogsTheSignalAndARacingError(t *testing.T) {
	t.Parallel()
	const secret = "SENTINEL-hunter2-Bearer-ya29"
	const signal = "terminated signal received"
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	// The session fails on the frame below, and the signal comes as the
	// server logs that failure, so ctx has ended when Run returns its error.
	stderr := &cancelOnLine{line: "the mcp session ended", cancel: func() { cancel(errors.New(signal)) }}
	var stdout bytes.Buffer
	home := t.TempDir()
	env := Env{
		Stdin:  io.NopCloser(strings.NewReader(`{"jsonrpc":"` + secret + `","id":1,"method":"tools/list"}` + "\n")),
		Stdout: &stdout, Stderr: stderr,
		Getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
	}
	code := Execute(ctx, []string{"--socket", "/nonexistent/chainmail.sock", "mcp"}, env)
	got := stderr.String()
	if code != 0 {
		t.Errorf("exit code %d, want 0 for a session a signal ended (stderr %q)", code, got)
	}
	if !strings.Contains(got, "session ended by a signal") || !strings.Contains(got, signal) {
		t.Errorf("stderr does not name the signal from the context's cause: %q", got)
	}
	if !strings.Contains(got, "the mcp session ended") || !strings.Contains(got, "kind=") {
		t.Errorf("stderr does not give the session error's kind: %q", got)
	}
	if strings.Contains(got, secret) || strings.Contains(stdout.String(), secret) {
		t.Errorf("client-controlled text reached the output: stderr %q, stdout %q", got, stdout.String())
	}
}
