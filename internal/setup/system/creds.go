package system

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotDecrypted is wrapped by CheckDecrypt's error when systemd-creds ran
// and refused the credential, exiting with a non-zero status. An error
// without it means systemd-creds could not be run or did not finish, which
// says nothing about the credential.
var ErrNotDecrypted = errors.New("the credential does not decrypt on this host")

// Creds seals and checks systemd credentials. It runs systemd-creds, found
// by the host finder, with the plaintext and credential only on pipes.
type Creds interface {
	// Encrypt seals plaintext under name with systemd's default key and
	// returns the credential.
	Encrypt(ctx context.Context, name string, plaintext []byte) ([]byte, error)
	// CheckDecrypt reports whether cred decrypts on this host under name.
	// The plaintext is read into memory, cleared and dropped.
	CheckDecrypt(ctx context.Context, name string, cred []byte) error
}

// NewCreds runs systemd-creds through pipe.
func NewCreds(pipe PipeRunner) Creds { return systemdCreds{pipe: pipe} }

type systemdCreds struct{ pipe PipeRunner }

// Encrypt implements Creds. The plaintext goes to systemd-creds on stdin,
// never in argv, and with no --with-key, so systemd picks its default.
func (c systemdCreds) Encrypt(ctx context.Context, name string, plaintext []byte) ([]byte, error) {
	cred, err := c.pipe(ctx, plaintext, "systemd-creds", "encrypt", "--name="+name, "-", "-")
	if err != nil {
		return nil, fmt.Errorf("sealing credential %s: %w", name, err)
	}
	return cred, nil
}

// CheckDecrypt implements Creds. The decrypted plaintext is cleared
// whether or not the command succeeded. Only a non-zero exit status of
// systemd-creds wraps ErrNotDecrypted. A command the finder could not
// resolve or trust, one killed by a signal and other run errors are
// returned wrapped without it.
func (c systemdCreds) CheckDecrypt(ctx context.Context, name string, cred []byte) error {
	plain, err := c.pipe(ctx, cred, "systemd-creds", "decrypt", "--name="+name, "-", "-")
	clear(plain)
	switch {
	case err == nil:
		return nil
	case exitCode(err) > 0:
		return fmt.Errorf("%w: %w", ErrNotDecrypted, err)
	default:
		return fmt.Errorf("checking credential %s: %w", name, err)
	}
}
