package setuptest

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"sync"

	"github.com/yaklabco/chainmail/internal/setup/system"
)

// sealedPrefix starts every credential the fake Creds seals.
const sealedPrefix = "sealed:"

// Creds is a fake system.Creds. Encrypt "seals" by returning "sealed:"
// and the base64 of the plaintext, so a test can check the key round
// trip. CheckDecrypt accepts a credential it sealed, under the same name,
// and any credential given to Accept, under any name. It keeps nothing of
// a plaintext but that sealed form.
type Creds struct {
	// EncryptErr and DecryptErr make those methods fail.
	EncryptErr, DecryptErr error
	// Journal, when set, gets an entry for each call.
	Journal *Journal

	mu       sync.Mutex
	accepted map[string]bool
	sealed   map[string]string // credential to the name it was sealed under
	calls    []string
}

var _ system.Creds = (*Creds)(nil)

// Accept makes CheckDecrypt accept cred under any name, as a credential
// sealed on this host before the test began.
func (c *Creds) Accept(cred []byte) *Creds {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accepted == nil {
		c.accepted = map[string]bool{}
	}
	c.accepted[string(cred)] = true
	return c
}

// Encrypt implements system.Creds.
func (c *Creds) Encrypt(_ context.Context, name string, plaintext []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("encrypt " + name)
	if c.EncryptErr != nil {
		return nil, c.EncryptErr
	}
	cred := sealedPrefix + base64.StdEncoding.EncodeToString(plaintext)
	if c.sealed == nil {
		c.sealed = map[string]string{}
	}
	c.sealed[cred] = name
	return []byte(cred), nil
}

// CheckDecrypt implements system.Creds.
func (c *Creds) CheckDecrypt(_ context.Context, name string, cred []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("decrypt " + name)
	if c.DecryptErr != nil {
		return c.DecryptErr
	}
	if c.accepted[string(cred)] {
		return nil
	}
	if sealedAs, ok := c.sealed[string(cred)]; ok && sealedAs == name {
		return nil
	}
	return errors.New("the credential does not decrypt on this host: fake systemd-creds rejects it")
}

// Calls lists the calls so far: "encrypt <name>" and "decrypt <name>".
func (c *Creds) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// record notes a call. The caller holds mu.
func (c *Creds) record(call string) {
	c.calls = append(c.calls, call)
	c.Journal.Add("creds " + call)
}
