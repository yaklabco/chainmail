package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/vault"
)

func newSecretCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("secret", "Store secrets in the daemon's vault")
	var fromPass string
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Store a secret such as accounts/<account> or oauth_clients/<client>",
		Long: "Store a secret in the daemon's vault. The value comes from --from-pass (first line), " +
			"or is typed at the terminal without echo, or is read from stdin. Secrets can't be read back.",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			// Check the name before asking for the value, so nobody types a
			// secret only to have it refused.
			if !vault.ValidName(name) {
				return &exitError{
					reason: api.ReasonInvalidArgument,
					msg:    fmt.Sprintf("invalid secret name %q", name),
					hint:   "names look like accounts/<account> or oauth_clients/<client>",
				}
			}
			// pass would read a leading - as an option, and --clip would put
			// the secret on the clipboard. A .. segment would leave the store.
			if strings.HasPrefix(fromPass, "-") || slices.Contains(strings.Split(fromPass, "/"), "..") {
				return &exitError{reason: api.ReasonInvalidArgument, msg: fmt.Sprintf("invalid pass entry %q", fromPass)}
			}
			value, err := readSecret(ctx, env, name, fromPass)
			if err != nil {
				return err
			}
			c := g.client(env)
			defer c.Close()
			req := connect.NewRequest(&chainmailv1.SetSecretRequest{Name: name, Value: value})
			if _, err := c.Admin.SetSecret(ctx, req); err != nil {
				return err
			}
			return writeJSON(env.Stdout, map[string]any{"name": name, "set": true})
		},
	}
	set.Flags().StringVar(&fromPass, "from-pass", "", "read the secret from this pass entry (first line)")
	cmd.AddCommand(set)
	return cmd
}

// maxSecretBytes caps a secret read from stdin. Passwords, client secrets and
// tokens are far smaller, and the daemon refuses requests over 1 MiB.
const maxSecretBytes = 64 << 10

// readSecret gets the value for name from pass, the terminal or stdin. It
// never echoes or logs the value.
func readSecret(ctx context.Context, env Env, name, passPath string) ([]byte, error) {
	var raw []byte
	var err error
	switch {
	case passPath != "":
		if env.PassShow == nil {
			return nil, errNoPass
		}
		// For a folder, pass show prints a tree listing and succeeds, so
		// insist on an entry file and refuse anything that looks like a tree.
		if err := checkPassEntry(env, passPath); err != nil {
			return nil, err
		}
		raw, err = env.PassShow(ctx, passPath)
		if err != nil {
			return nil, err
		}
		raw, _, _ = bytes.Cut(raw, []byte("\n"))
		if bytes.IndexByte(raw, 0x1b) >= 0 {
			return nil, &exitError{
				reason: api.ReasonInvalidArgument,
				msg:    fmt.Sprintf("pass entry %q printed a folder listing, not a secret", passPath),
				hint:   "run: pass ls, then name an entry rather than a folder",
			}
		}
	case env.ReadPassword != nil:
		_, _ = fmt.Fprintf(env.Stderr, "Secret for %s: ", name) // a failed prompt write only loses the prompt
		raw, err = env.ReadPassword()
		_, _ = fmt.Fprintln(env.Stderr) // as above
		if err != nil {
			return nil, fmt.Errorf("reading secret: %w", err)
		}
	default:
		raw, err = io.ReadAll(io.LimitReader(env.Stdin, maxSecretBytes+1))
		if err != nil {
			return nil, fmt.Errorf("reading secret from stdin: %w", err)
		}
		if len(raw) > maxSecretBytes {
			return nil, usageError{err: fmt.Errorf("secret is longer than %d bytes", maxSecretBytes)}
		}
	}
	value := bytes.TrimRight(raw, "\r\n")
	if len(value) == 0 {
		return nil, usageError{err: errors.New("secret is empty")}
	}
	// A secret is one line. More than one usually means a whole pass entry
	// was piped in, and storing it would only fail at the next login.
	if bytes.ContainsAny(value, "\r\n") {
		return nil, usageError{
			err:  errors.New("secret has more than one line"),
			hint: "for a pass entry use: chainmail secret set " + name + " --from-pass <entry>",
		}
	}
	return value, nil
}

// checkPassEntry makes sure entry is a file in the password store, which is
// $PASSWORD_STORE_DIR or ~/.password-store, as pass itself decides.
func checkPassEntry(env Env, entry string) error {
	store := env.Getenv("PASSWORD_STORE_DIR")
	if store == "" {
		home := env.Getenv("HOME")
		if home == "" {
			return usageError{err: errors.New("cannot find the password store: set PASSWORD_STORE_DIR or HOME")}
		}
		store = filepath.Join(home, ".password-store")
	}
	info, err := os.Stat(filepath.Join(store, entry+".gpg"))
	switch {
	case errors.Is(err, fs.ErrNotExist) || (err == nil && !info.Mode().IsRegular()):
		return &exitError{
			reason: api.ReasonNotFound,
			msg:    fmt.Sprintf("pass entry %q is not in the password store %s", entry, store),
			hint:   "run: pass ls",
		}
	case err != nil:
		return fmt.Errorf("checking pass entry %q: %w", entry, err)
	}
	return nil
}
