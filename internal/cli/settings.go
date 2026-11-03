package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/jamesainslie/chainmail/internal/config"
)

// binding ties a setting to its flag and environment variable.
type binding struct {
	// key names the setting, and is its key in cli.toml.
	key    string
	flag   string
	envVar string
}

// clientBindings are the CLI's own settings. cli.toml may hold these keys only.
var clientBindings = []binding{
	{key: "socket", flag: "socket", envVar: "CHAINMAIL_SOCKET"},
	{key: "output", flag: "output", envVar: "CHAINMAIL_OUTPUT"},
}

// daemonBindings are the daemon command's flags. They have no file layer:
// the daemon's config file is read by internal/config, and nothing in it
// can be set from the environment.
var daemonBindings = []binding{
	{key: "config", flag: "config", envVar: "CHAINMAIL_CONFIG"},
	{key: "state-dir", flag: "state-dir", envVar: "CHAINMAIL_STATE_DIR"},
	{key: "vault-key-file", flag: "vault-key-file", envVar: "CHAINMAIL_VAULT_KEY_FILE"},
	{key: "dev", flag: "dev", envVar: "CHAINMAIL_DEV"},
}

// layered returns a viper that reads each binding from, in order: its flag
// when given, its environment variable through env.Getenv, file, then any
// SetDefault the caller adds, then the flag's default. Viper's own env layer
// reads the process environment, which tests cannot set per test, so env
// values are merged over file in viper's config layer instead.
func layered(flags *pflag.FlagSet, binds []binding, env Env, file map[string]any) (*viper.Viper, error) {
	v := viper.New()
	merged := maps.Clone(file)
	if merged == nil {
		merged = map[string]any{}
	}
	for _, b := range binds {
		f := flags.Lookup(b.flag)
		if f == nil {
			return nil, fmt.Errorf("no --%s flag to bind", b.flag)
		}
		if err := v.BindPFlag(b.key, f); err != nil {
			return nil, fmt.Errorf("binding --%s: %w", b.flag, err)
		}
		if s := env.Getenv(b.envVar); s != "" {
			merged[b.key] = s
		}
	}
	if err := v.MergeConfigMap(merged); err != nil {
		return nil, fmt.Errorf("merging settings: %w", err)
	}
	return v, nil
}

// source names where b's value came from, for error messages.
func source(flags *pflag.FlagSet, b binding, env Env, path string, file map[string]any) string {
	switch {
	case flags.Changed(b.flag):
		return "--" + b.flag
	case env.Getenv(b.envVar) != "":
		return b.envVar
	case file[b.key] != nil:
		return path
	default:
		return "the default"
	}
}

// cliConfigPath is the user's cli.toml: under $XDG_CONFIG_HOME, else under
// $HOME/.config. It is empty when neither is an absolute path.
func cliConfigPath(env Env) string {
	if x := env.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "chainmail", "cli.toml")
	}
	if h := env.Getenv("HOME"); filepath.IsAbs(h) {
		return filepath.Join(h, ".config", "chainmail", "cli.toml")
	}
	return ""
}

// readCLIConfig reads cli.toml at path. A missing file holds no settings.
// A file that does not parse, or holds anything but string socket and
// output keys, is a usage error naming the path.
func readCLIConfig(path string) (map[string]any, error) {
	settings := map[string]any{}
	if path == "" {
		return settings, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return settings, nil
	}
	hint := "fix or remove " + path + "; it takes socket and output"
	if err != nil {
		return nil, usageError{err: fmt.Errorf("reading %s: %w", path, err), hint: hint}
	}
	// Read as written, not through viper, which drops empty tables and so
	// would let output = {} or an empty [unexpected] table pass.
	var all map[string]any
	if err := toml.Unmarshal(data, &all); err != nil {
		return nil, usageError{err: fmt.Errorf("parsing %s: %w", path, err), hint: hint}
	}
	for _, key := range slices.Sorted(maps.Keys(all)) {
		if !slices.ContainsFunc(clientBindings, func(b binding) bool { return b.key == key }) {
			return nil, usageError{err: fmt.Errorf("%s: unknown key %q", path, key), hint: hint}
		}
		s, ok := all[key].(string)
		if !ok {
			return nil, usageError{err: fmt.Errorf("%s: %s must be a string", path, key), hint: hint}
		}
		settings[key] = s
	}
	return settings, nil
}

// resolve fills g.socket and g.output from cmd's flags, the environment,
// cli.toml and the defaults, in that order, and checks the output format.
func (g *globals) resolve(env Env, cmd *cobra.Command) error {
	path := cliConfigPath(env)
	file, err := readCLIConfig(path)
	if err != nil {
		return err
	}
	flags := cmd.Flags()
	v, err := layered(flags, clientBindings, env, file)
	if err != nil {
		return err
	}
	v.SetDefault("socket", config.DefaultSocket)
	out := "json"
	if env.StdoutTerminal {
		out = "table"
	}
	v.SetDefault("output", out)
	g.socket, g.output = v.GetString("socket"), v.GetString("output")
	from := source(flags, clientBindings[1], env, path, file)
	g.outputChosen = from != "the default"
	if g.output != "json" && g.output != "table" {
		return usageError{err: fmt.Errorf("--output must be json or table, got %q from %s", g.output, from), hint: "run: chainmail --help"}
	}
	return nil
}

// fromSettings fills f from its flags, else the CHAINMAIL_* environment
// variables, else the flag defaults.
func (f *daemonFlags) fromSettings(env Env, flags *pflag.FlagSet) error {
	// An explicit --dev wins, so CHAINMAIL_DEV is only checked when it is used.
	if s := env.Getenv("CHAINMAIL_DEV"); s != "" && !flags.Changed("dev") {
		if _, err := strconv.ParseBool(s); err != nil {
			return usageError{err: fmt.Errorf("CHAINMAIL_DEV must be true or false, got %q", s), hint: daemonHint}
		}
	}
	v, err := layered(flags, daemonBindings, env, nil)
	if err != nil {
		return err
	}
	f.configPath = v.GetString("config")
	f.stateDir = v.GetString("state-dir")
	f.vaultKey = v.GetString("vault-key-file")
	f.dev = v.GetBool("dev")
	return nil
}
