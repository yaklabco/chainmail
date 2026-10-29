package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/daemon"
	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/vault"
)

const (
	productionStateDir = "/var/lib/chainmail"
	daemonHint         = "run: chainmail daemon --help"
)

type daemonFlags struct {
	configPath string
	stateDir   string
	vaultKey   string
	dev        bool
}

type daemonPlan struct {
	cfg      *config.Config
	stateDir string
	vaultKey string
	socket   string
	dev      bool
}

func newDaemonCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	f := &daemonFlags{}
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the chainmail daemon",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			plan, err := f.resolve(env, g)
			if err != nil {
				return err
			}
			return runDaemon(ctx, env, plan)
		},
	}
	cmd.Flags().StringVar(&f.configPath, "config", config.DefaultPath, "config file")
	cmd.Flags().StringVar(&f.stateDir, "state-dir", productionStateDir, "directory for vault.age and state.db")
	cmd.Flags().StringVar(&f.vaultKey, "vault-key-file", "", "age identity file (default $CREDENTIALS_DIRECTORY/vault-key)")
	cmd.Flags().BoolVar(&f.dev, "dev", false, "development mode: run as yourself with a non-production state directory")
	return cmd
}

func (f *daemonFlags) resolve(env Env, g *globals) (daemonPlan, error) {
	// Abs also cleans, so spellings such as /var/lib/./chainmail/ are caught.
	stateDir, err := filepath.Abs(f.stateDir)
	if err != nil {
		return daemonPlan{}, fmt.Errorf("state dir %s: %w", f.stateDir, err)
	}
	p := daemonPlan{stateDir: stateDir, vaultKey: f.vaultKey, dev: f.dev}
	if p.dev && isProductionDir(p.stateDir, productionStateDir) {
		return p, usageError{err: errors.New("--dev needs a --state-dir other than " + productionStateDir), hint: daemonHint}
	}
	if p.vaultKey == "" {
		creds := env.Getenv("CREDENTIALS_DIRECTORY")
		if creds == "" {
			return p, usageError{err: errors.New("no vault key: pass --vault-key-file, or run under systemd with LoadCredentialEncrypted=vault-key"), hint: daemonHint}
		}
		p.vaultKey = filepath.Join(creds, "vault-key")
	}
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return p, err
	}
	p.cfg = cfg
	// Clients find the socket through --socket, then CHAINMAIL_SOCKET. A dev
	// daemon follows the same order before its own default, so one exported
	// variable points both at it. Production uses the config file instead.
	switch {
	case g.socket != "":
		p.socket = g.socket
	case p.dev && env.Getenv("CHAINMAIL_SOCKET") != "":
		p.socket = env.Getenv("CHAINMAIL_SOCKET")
	case p.dev:
		runtimeDir := env.Getenv("XDG_RUNTIME_DIR")
		if runtimeDir == "" {
			return p, usageError{err: errors.New("--dev needs --socket, CHAINMAIL_SOCKET or XDG_RUNTIME_DIR"), hint: daemonHint}
		}
		p.socket = filepath.Join(runtimeDir, "chainmail-dev.sock")
	default:
		p.socket = cfg.Daemon.Socket
	}
	return p, nil
}

func runDaemon(ctx context.Context, env Env, p daemonPlan) error {
	logger := slog.New(slog.NewJSONHandler(env.Stderr, nil))
	if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	id, err := vault.LoadIdentity(p.vaultKey)
	if err != nil {
		return err
	}
	v, err := vault.Open(filepath.Join(p.stateDir, "vault.age"), id)
	if err != nil {
		return err
	}
	stateDB, err := db.OpenState(ctx, filepath.Join(p.stateDir, "state.db"))
	if err != nil {
		return err
	}
	// Serve returns only after every handler has, so no request is still
	// writing an audit row when this runs.
	defer func() {
		if err := stateDB.Close(); err != nil {
			logger.Error("closing state.db", "err", err)
		}
	}()
	gid := -1
	if !p.dev {
		gid, err = lookupGID(p.cfg.Daemon.ClientGroup)
		if err != nil {
			return err
		}
	}
	ln, err := daemon.Listen(ctx, p.socket, gid)
	if err != nil {
		return err
	}
	srv, err := daemon.New(daemon.Options{
		Config:     p.cfg,
		Vault:      v,
		StateDB:    stateDB,
		Listener:   ln,
		Authorizer: daemon.NewAuthorizer(uint32(os.Getuid()), gid),
		DevMode:    p.dev,
		Logger:     logger,
	})
	if err != nil {
		_ = ln.Close() // the New error is the one to report
		return err
	}
	return srv.Serve(ctx)
}

func lookupGID(name string) (int, error) {
	grp, err := user.LookupGroup(name)
	if err != nil {
		return 0, fmt.Errorf("client group %s: %w", name, err)
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		return 0, fmt.Errorf("client group %s has gid %q: %w", name, grp.Gid, err)
	}
	return gid, nil
}

// isProductionDir reports whether dir is prod, by name or, when both exist,
// as the same directory, so a symlink to prod does not get past --dev.
func isProductionDir(dir, prod string) bool {
	if filepath.Clean(dir) == filepath.Clean(prod) {
		return true
	}
	di, err := os.Stat(dir)
	if err != nil {
		return false
	}
	pi, err := os.Stat(prod)
	if err != nil {
		return false
	}
	return os.SameFile(di, pi)
}
