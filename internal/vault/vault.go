// Package vault keeps chainmail's secrets in one age-encrypted file.
//
// The daemon can set and read secrets. No API returns them to a client.
package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"

	"filippo.io/age"
)

// Errors returned by the vault.
var (
	ErrNotFound    = errors.New("secret not found")
	ErrInvalidName = errors.New("invalid secret name")
	ErrEmptyValue  = errors.New("secret value is empty")
)

const formatVersion = 1

type fileFormat struct {
	Version int               `json:"version"`
	Secrets map[string][]byte `json:"secrets"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)

// ValidName reports whether name can name a secret, such as "accounts/work".
func ValidName(name string) bool {
	return len(name) <= 128 && nameRE.MatchString(name)
}

// Vault is an encrypted secret store backed by one file.
type Vault struct {
	mu       sync.RWMutex
	path     string
	identity *age.X25519Identity
	secrets  map[string][]byte
}

// LoadIdentity reads the age X25519 identity in path. The file must not be
// readable by group or others.
func LoadIdentity(path string) (*age.X25519Identity, error) {
	// Check the mode and read the key through one descriptor, so the file
	// that passed the check is the file that gets read.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("vault key: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only; nothing to flush
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("vault key: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("vault key %s: mode %v is too open, want 0600 or 0400", path, perm)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("vault key: %w", err)
	}
	ids, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("vault key %s: %w", path, err)
	}
	if len(ids) != 1 {
		return nil, fmt.Errorf("vault key %s: want exactly one identity, found %d", path, len(ids))
	}
	id, ok := ids[0].(*age.X25519Identity)
	if !ok {
		return nil, fmt.Errorf("vault key %s: want an X25519 identity, found %T", path, ids[0])
	}
	return id, nil
}

// Open loads the vault at path. A missing file is an empty vault; the first Set creates it.
func Open(path string, id *age.X25519Identity) (*Vault, error) {
	v := &Vault{path: path, identity: id, secrets: map[string][]byte{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading vault: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(data), id)
	if err != nil {
		return nil, fmt.Errorf("decrypting vault %s: %w", path, err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("decrypting vault %s: %w", path, err)
	}
	var f fileFormat
	if err := json.Unmarshal(plain, &f); err != nil {
		return nil, fmt.Errorf("decoding vault %s: %w", path, err)
	}
	if f.Version != formatVersion {
		return nil, fmt.Errorf("vault %s has format version %d, want %d", path, f.Version, formatVersion)
	}
	if f.Secrets != nil {
		v.secrets = f.Secrets
	}
	return v, nil
}

// Get returns a copy of the secret called name.
func (v *Vault) Get(name string) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s, ok := v.secrets[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return bytes.Clone(s), nil
}

// Set stores value under name and rewrites the vault file atomically.
func (v *Vault) Set(name string, value []byte) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if len(value) == 0 {
		return ErrEmptyValue
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	next := maps.Clone(v.secrets)
	next[name] = bytes.Clone(value)
	if err := v.write(next); err != nil {
		return err
	}
	v.secrets = next
	return nil
}

// Names returns the stored secret names in sorted order.
func (v *Vault) Names() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return slices.Sorted(maps.Keys(v.secrets))
}

func (v *Vault) write(secrets map[string][]byte) error {
	plain, err := json.Marshal(fileFormat{Version: formatVersion, Secrets: secrets})
	if err != nil {
		return fmt.Errorf("encoding vault: %w", err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, v.identity.Recipient())
	if err != nil {
		return fmt.Errorf("encrypting vault: %w", err)
	}
	if _, err := w.Write(plain); err != nil {
		return fmt.Errorf("encrypting vault: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("encrypting vault: %w", err)
	}
	return writeFileAtomic(v.path, buf.Bytes())
}

// writeFileAtomic writes data to a 0600 temp file beside path, syncs it, and
// renames it over path, so a crash leaves either the old vault or the new one.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".vault-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp vault: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name()) // best effort; the write already failed
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close() // the write error is the one to report
		return fmt.Errorf("writing temp vault: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close() // the sync error is the one to report
		return fmt.Errorf("syncing temp vault: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing temp vault: %w", err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing vault: %w", err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening %s: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close() // the sync error is the one to report
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", dir, err)
	}
	return nil
}
