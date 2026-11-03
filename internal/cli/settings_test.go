package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/jamesainslie/chainmail/internal/config"
)

// resolveWith parses args as global flags on a throwaway tree and resolves them.
func resolveWith(t *testing.T, env Env, args ...string) (*globals, error) {
	t.Helper()
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	g := &globals{}
	root := &cobra.Command{Use: "chainmail"}
	addGlobalFlags(root, g)
	sub := &cobra.Command{Use: "x", Run: func(*cobra.Command, []string) {}}
	root.AddCommand(sub)
	if err := sub.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return g, g.resolve(env, sub)
}

// vars returns a Getenv over m.
func vars(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// writeCLIConfig writes cli.toml under a new XDG_CONFIG_HOME and returns that directory.
func writeCLIConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "chainmail"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chainmail", "cli.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSettingsPrecedence(t *testing.T) {
	t.Parallel()
	xdg := writeCLIConfig(t, "socket = \"/file.sock\"\noutput = \"table\"\n")
	tests := []struct {
		name                string
		env                 Env
		args                []string
		wantSocket, wantOut string
	}{
		{name: "defaults", env: Env{}, wantSocket: config.DefaultSocket, wantOut: "json"},
		{name: "terminal default", env: Env{StdoutTerminal: true}, wantSocket: config.DefaultSocket, wantOut: "table"},
		{name: "file", env: Env{Getenv: vars(map[string]string{"XDG_CONFIG_HOME": xdg})}, wantSocket: "/file.sock", wantOut: "table"},
		{
			name:       "env over file",
			env:        Env{Getenv: vars(map[string]string{"XDG_CONFIG_HOME": xdg, "CHAINMAIL_SOCKET": "/env.sock", "CHAINMAIL_OUTPUT": "json"})},
			wantSocket: "/env.sock", wantOut: "json",
		},
		{
			name:       "flag over env",
			env:        Env{Getenv: vars(map[string]string{"XDG_CONFIG_HOME": xdg, "CHAINMAIL_SOCKET": "/env.sock"})},
			args:       []string{"--socket", "/flag.sock", "--output", "json"},
			wantSocket: "/flag.sock", wantOut: "json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g, err := resolveWith(t, tt.env, tt.args...)
			if err != nil {
				t.Fatal(err)
			}
			if g.socket != tt.wantSocket || g.output != tt.wantOut {
				t.Errorf("socket %q output %q, want %q %q", g.socket, g.output, tt.wantSocket, tt.wantOut)
			}
		})
	}
}

func TestSettingsHomeFallback(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "chainmail")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cli.toml"), []byte(`socket = "/home.sock"`), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := resolveWith(t, Env{Getenv: vars(map[string]string{"HOME": home})})
	if err != nil || g.socket != "/home.sock" {
		t.Errorf("socket %q, err %v, want /home.sock", g.socket, err)
	}
}

func TestSettingsErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		file string
		env  map[string]string
		args []string
		want string
	}{
		{name: "unknown key", file: `colour = "red"`, want: `unknown key "colour"`},
		{name: "table", file: "[output]\nx = 1", want: "output must be a string"},
		{name: "bad toml", file: "socket = ", want: "parsing"},
		{name: "not a string", file: "output = 3", want: "output must be a string"},
		{name: "bad output in file", file: `output = "yaml"`, want: `got "yaml" from `},
		{name: "bad output in env", env: map[string]string{"CHAINMAIL_OUTPUT": "yaml"}, want: `got "yaml" from CHAINMAIL_OUTPUT`},
		{name: "empty output flag", args: []string{"--output", ""}, want: `got "" from --output`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{}
			for k, v := range tt.env {
				env[k] = v
			}
			if tt.file != "" {
				env["XDG_CONFIG_HOME"] = writeCLIConfig(t, tt.file)
			}
			_, err := resolveWith(t, Env{Getenv: vars(env)}, tt.args...)
			var ue usageError
			if !errors.As(err, &ue) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want a usage error containing %q", err, tt.want)
			}
		})
	}
}

func TestDaemonFlagsFromEnvironment(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(twoAccounts), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{
			name: "state dir, config and vault key from env",
			env:  map[string]string{"CHAINMAIL_CONFIG": cfgPath, "CHAINMAIL_STATE_DIR": "/var/lib/chainmail", "CHAINMAIL_VAULT_KEY_FILE": "/x"},
			args: []string{"daemon", "--dev"},
			want: "--state-dir other than",
		},
		{
			name: "dev from env",
			env:  map[string]string{"CHAINMAIL_DEV": "true"},
			args: []string{"daemon", "--config", cfgPath, "--state-dir", "/var/lib/chainmail", "--vault-key-file", "/x"},
			want: "--state-dir other than",
		},
		{
			// The daemon reads only --socket for its own socket, so a client's
			// cli.toml cannot move it, and a dev daemon still needs one of its own.
			name: "cli.toml socket does not reach the daemon",
			env:  map[string]string{"XDG_CONFIG_HOME": writeCLIConfig(t, `socket = "/file.sock"`)},
			args: []string{"daemon", "--dev", "--config", cfgPath, "--state-dir", t.TempDir(), "--vault-key-file", "/x"},
			want: "--dev needs --socket, CHAINMAIL_SOCKET or XDG_RUNTIME_DIR",
		},
		{
			name: "bad dev value",
			env:  map[string]string{"CHAINMAIL_DEV": "maybe"},
			args: []string{"daemon"},
			want: "CHAINMAIL_DEV must be true or false",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{Getenv: vars(tt.env)}, tt.args...)
			if got.code != 2 || errorReason(t, got.stderr) != "USAGE" || !strings.Contains(got.stderr, tt.want) {
				t.Errorf("got %+v, want exit 2 with %q", got, tt.want)
			}
		})
	}
}

// The daemon uses neither client setting, so a bad one must not stop it.
func TestDaemonIgnoresClientSettings(t *testing.T) {
	t.Parallel()
	xdg := writeCLIConfig(t, "colour = \"red\"")
	env := Env{Getenv: vars(map[string]string{"CHAINMAIL_OUTPUT": "yaml", "XDG_CONFIG_HOME": xdg})}
	got := runCLI(t, env, "daemon", "--dev", "--config", "/nonexistent/config.toml", "--state-dir", t.TempDir(), "--vault-key-file", "/x")
	if strings.Contains(got.stderr, "output") || strings.Contains(got.stderr, "cli.toml") {
		t.Errorf("daemon failed on client settings: %s", got.stderr)
	}
}
