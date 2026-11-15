package system

import (
	"context"
	"fmt"
)

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
// whether or not the command succeeded.
func (c systemdCreds) CheckDecrypt(ctx context.Context, name string, cred []byte) error {
	plain, err := c.pipe(ctx, cred, "systemd-creds", "decrypt", "--name="+name, "-", "-")
	clear(plain)
	if err != nil {
		return fmt.Errorf("the credential does not decrypt on this host: %w", err)
	}
	return nil
}
