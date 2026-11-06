package cli

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/config"
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
			env := maps.Clone(tt.env)
			if env == nil {
				env = map[string]string{}
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
	// It got past the settings and failed on its own config file instead.
	if got.code != api.ExitInternal || !strings.Contains(got.stderr, "reading config") {
		t.Errorf("got %+v, want exit %d reading the config file", got, api.ExitInternal)
	}
}

// Viper drops empty tables, so cli.toml is checked as written.
func TestSettingsRefuseEmptyTables(t *testing.T) {
	t.Parallel()
	for name, file := range map[string]string{
		"empty table for output": "output = {}",
		"empty unknown table":    "[unexpected]",
		"empty inline unknown":   "extra = {}",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveWith(t, Env{Getenv: vars(map[string]string{"XDG_CONFIG_HOME": writeCLIConfig(t, file)})})
			var ue usageError
			if !errors.As(err, &ue) {
				t.Errorf("err = %v, want a usage error for %q", err, file)
			}
		})
	}
}

// An explicit --dev wins over CHAINMAIL_DEV, so a bad value there does not matter.
func TestDaemonDevFlagBeatsABadEnvironment(t *testing.T) {
	t.Parallel()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(twoAccounts), 0o600); err != nil {
		t.Fatal(err)
	}
	// Each flag value fails later in its own way, which shows it was used.
	tests := []struct {
		dev, stateDir, want string
		code                int
	}{
		// Not dev: the production state directory is allowed, and the
		// missing vault key is what stops it.
		{dev: "--dev=false", stateDir: t.TempDir(), want: "vault key", code: api.ExitInternal},
		{dev: "--dev=true", stateDir: "/var/lib/chainmail", want: "--state-dir other than", code: api.ExitUsage},
	}
	for _, tt := range tests {
		t.Run(tt.dev, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{Getenv: vars(map[string]string{"CHAINMAIL_DEV": "maybe"})},
				"daemon", tt.dev, "--config", cfgPath, "--state-dir", tt.stateDir, "--vault-key-file", "/nonexistent/vault-key")
			if strings.Contains(got.stderr, "CHAINMAIL_DEV") {
				t.Errorf("%s still failed on CHAINMAIL_DEV: %s", tt.dev, got.stderr)
			}
			if got.code != tt.code || !strings.Contains(got.stderr, tt.want) {
				t.Errorf("got %+v, want exit %d with %q", got, tt.code, tt.want)
			}
		})
	}
}

// cliTOMLPath returns where cli.toml lives under a new XDG_CONFIG_HOME,
// with its directory made, and that XDG_CONFIG_HOME.
func cliTOMLPath(t *testing.T) (path, xdg string) {
	t.Helper()
	xdg = t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdg, "chainmail"), 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(xdg, "chainmail", "cli.toml"), xdg
}

// A cli.toml that is a FIFO with no writer would block a plain read
// forever, hanging every command. It must be refused at once.
func TestSettingsRefuseAFIFO(t *testing.T) {
	t.Parallel()
	path, xdg := cliTOMLPath(t)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan cliResult, 1)
	go func() { done <- runCLI(t, Env{Getenv: vars(map[string]string{"XDG_CONFIG_HOME": xdg})}, "version") }()
	select {
	case got := <-done:
		if got.code != api.ExitUsage || !strings.Contains(got.stderr, "not a regular file") || !strings.Contains(got.stderr, path) {
			t.Errorf("got %+v, want a usage error that %s is not a regular file", got, path)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading a FIFO cli.toml hung")
	}
}

func TestSettingsRefuseNonRegularAndLargeFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		make  func(t *testing.T, path string)
		wants []string
	}{
		{
			name:  "directory",
			make:  func(t *testing.T, path string) { t.Helper(); mustMkdir(t, path) },
			wants: []string{"not a regular file"},
		},
		{
			name: "over 64 KiB",
			make: func(t *testing.T, path string) {
				t.Helper()
				body := "# " + strings.Repeat("x", 64<<10) + "\n"
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wants: []string{"larger than 64 KiB"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, xdg := cliTOMLPath(t)
			tt.make(t, path)
			_, err := resolveWith(t, Env{Getenv: vars(map[string]string{"XDG_CONFIG_HOME": xdg})})
			var ue usageError
			if !errors.As(err, &ue) || !strings.Contains(err.Error(), path) {
				t.Fatalf("err = %v, want a usage error naming %s", err, path)
			}
			for _, want := range tt.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want %q", err, want)
				}
			}
		})
	}
}

// A cli.toml of exactly 64 KiB is still read.
func TestSettingsReadAFileAtTheLimit(t *testing.T) {
	t.Parallel()
	path, xdg := cliTOMLPath(t)
	line := "socket = \"/limit.sock\"\n"
	body := line + "#" + strings.Repeat("x", 64<<10-len(line)-2) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := resolveWith(t, Env{Getenv: vars(map[string]string{"XDG_CONFIG_HOME": xdg})})
	if err != nil || g.socket != "/limit.sock" {
		t.Errorf("socket %q, err %v, want /limit.sock", g.socket, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

// help, completion, man and daemon use neither client setting, so a bad
// cli.toml must not stop them. A client command still refuses it.
func TestSettingsSkippedByCommandsThatDoNotUseThem(t *testing.T) {
	t.Parallel()
	xdg := writeCLIConfig(t, `colour = "red"`)
	env := func() Env { return Env{Getenv: vars(map[string]string{"XDG_CONFIG_HOME": xdg})} }
	for _, args := range [][]string{{"help"}, {"help", "accounts"}, {"completion", "bash"}, {"man"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, env(), args...)
			if got.code != 0 || got.stdout == "" {
				t.Errorf("got %+v, want exit 0 with output", got)
			}
		})
	}
	t.Run("daemon", func(t *testing.T) {
		t.Parallel()
		got := runCLI(t, env(), "daemon", "--dev", "--state-dir", "/var/lib/chainmail")
		if got.code != api.ExitUsage || !strings.Contains(got.stderr, "--state-dir other than") {
			t.Errorf("got %+v, want the daemon's own usage error", got)
		}
	})
	t.Run("version", func(t *testing.T) {
		t.Parallel()
		got := runCLI(t, env(), "version")
		if got.code != api.ExitUsage || !strings.Contains(got.stderr, `unknown key \"colour\"`) {
			t.Errorf("got %+v, want a usage error for the unknown key", got)
		}
	})
}
