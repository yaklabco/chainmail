package cli

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/version"
)

// Only --dev, or CHAINMAIL_DEV set to a true value, makes a dev daemon. An
// unset, empty or false variable leaves it a production one.
func TestDaemonResolveModes(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(twoAccounts), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		env  map[string]string
		dev  string // the --dev argument, "" when not given
		want bool
	}{
		{name: "unset", want: false},
		{name: "empty", env: map[string]string{"CHAINMAIL_DEV": ""}, want: false},
		{name: "zero", env: map[string]string{"CHAINMAIL_DEV": "0"}, want: false},
		{name: "false", env: map[string]string{"CHAINMAIL_DEV": "false"}, want: false},
		{name: "one", env: map[string]string{"CHAINMAIL_DEV": "1"}, want: true},
		{name: "true", env: map[string]string{"CHAINMAIL_DEV": "true"}, want: true},
		{name: "flag", dev: "--dev", want: true},
		{name: "flag over false variable", env: map[string]string{"CHAINMAIL_DEV": "false"}, dev: "--dev", want: true},
		{name: "flag false over true variable", env: map[string]string{"CHAINMAIL_DEV": "true"}, dev: "--dev=false", want: false},
		{name: "flag false", dev: "--dev=false", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := Env{Getenv: vars(tt.env)}
			f := &daemonFlags{}
			flags := pflag.NewFlagSet("daemon", pflag.ContinueOnError)
			f.bind(flags)
			args := []string{"--config", cfgPath, "--state-dir", t.TempDir(), "--vault-key-file", "/nonexistent/vault-key"}
			if tt.dev != "" {
				args = append(args, tt.dev)
			}
			if err := flags.Parse(args); err != nil {
				t.Fatal(err)
			}
			if err := f.fromSettings(env, flags); err != nil {
				t.Fatalf("fromSettings: %v", err)
			}
			g := &globals{socketFlag: filepath.Join(t.TempDir(), "dev.sock")}
			p, err := f.resolve(env, g)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if p.dev != tt.want {
				t.Errorf("dev = %t, want %t", p.dev, tt.want)
			}
		})
	}
}

// A dev daemon says so on stderr in plain text and in its JSON log, and a
// production daemon says nothing.
func TestDevWarningPrinted(t *testing.T) {
	t.Parallel()
	t.Run("dev", func(t *testing.T) {
		t.Parallel()
		var stderr, logs bytes.Buffer
		announceMode(&stderr, slog.New(slog.NewJSONHandler(&logs, nil)), true)
		if got, want := stderr.String(), devWarning+"\n"; got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		if len(lines) != 1 {
			t.Fatalf("got %d log records, want 1:\n%s", len(lines), logs.String())
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
			t.Fatalf("log record %q: %v", lines[0], err)
		}
		if rec["level"] != "WARN" || rec["msg"] != devWarning || rec["dev"] != true {
			t.Errorf("log record = %v, want level WARN, msg devWarning and dev true", rec)
		}
	})
	t.Run("production", func(t *testing.T) {
		t.Parallel()
		var stderr, logs bytes.Buffer
		announceMode(&stderr, slog.New(slog.NewJSONHandler(&logs, nil)), false)
		if stderr.Len() != 0 || logs.Len() != 0 {
			t.Errorf("stderr = %q, logs = %q, want both empty", stderr.String(), logs.String())
		}
	})
}

// admin status warns about a dev daemon and about a daemon whose version is
// not the CLI's, and says nothing extra otherwise.
func TestStatusTableWarnings(t *testing.T) {
	t.Parallel()
	versionWarning := "the chainmail CLI is " + version.Version + ", the daemon is 9.9.9-other; " +
		"run bootstrap again or restart the daemon after upgrading"
	tests := []struct {
		name string
		resp *chainmailv1.GetStatusResponse
		want []string
	}{
		{name: "production, same version", resp: &chainmailv1.GetStatusResponse{Version: version.Version}},
		{name: "dev", resp: &chainmailv1.GetStatusResponse{Version: version.Version, DevMode: true}, want: []string{devWarning}},
		{name: "other version", resp: &chainmailv1.GetStatusResponse{Version: "9.9.9-other"}, want: []string{versionWarning}},
		{
			name: "dev and other version",
			resp: &chainmailv1.GetStatusResponse{Version: "9.9.9-other", DevMode: true},
			want: []string{devWarning, versionWarning},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.resp.StartedAt = statusAt(8, 0)
			var b strings.Builder
			if err := statusTable(&b, time.UTC, tt.resp); err != nil {
				t.Fatal(err)
			}
			var got []string
			for line := range strings.SplitSeq(b.String(), "\n") {
				if rest, ok := strings.CutPrefix(line, "warning "); ok {
					got = append(got, strings.TrimLeft(rest, " "))
				}
			}
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("warning rows = %q, want %q\nfull table:\n%s", got, tt.want, b.String())
			}
			// The dev mode row stays, whatever the warnings.
			if !strings.Contains(b.String(), "dev mode  ") {
				t.Errorf("table lost its dev mode row:\n%s", b.String())
			}
		})
	}
}

func TestDevFlagHelp(t *testing.T) {
	t.Parallel()
	cmd := newDaemonCmd(t.Context(), Env{Getenv: func(string) string { return "" }}, &globals{})
	f := cmd.Flags().Lookup("dev")
	if f == nil {
		t.Fatal("daemon has no --dev flag")
	}
	if !strings.Contains(f.Usage, "never with real accounts") {
		t.Errorf("--dev usage = %q, want it to say never with real accounts", f.Usage)
	}
}

// The daemon's example leads with bootstrap, and shows --dev only under a
// comment that says it is for contributors.
func TestHelptextNoDevStart(t *testing.T) {
	t.Parallel()
	const contributors = "# contributors only: a dev daemon runs as you and is unsafe with real accounts"
	ex, ok := examples["chainmail daemon"]
	if !ok {
		t.Fatal("no example for chainmail daemon")
	}
	lines := strings.Split(ex, "\n")
	var commands []string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		commands = append(commands, trimmed)
		if strings.Contains(trimmed, "--dev") {
			if i == 0 || strings.TrimSpace(lines[i-1]) != contributors {
				t.Errorf("line %q is not preceded by %q", trimmed, contributors)
			}
		}
	}
	if len(commands) == 0 || !strings.HasPrefix(commands[0], "chainmail bootstrap ") {
		t.Errorf("first command = %q, want chainmail bootstrap\n%s", commands, ex)
	}
}
