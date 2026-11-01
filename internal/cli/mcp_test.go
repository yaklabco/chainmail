package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func TestMCPCommandSpeaksMCPOnStdio(t *testing.T) {
	t.Parallel()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
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
