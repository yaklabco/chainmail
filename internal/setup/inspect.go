package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"

	"github.com/yaklabco/chainmail/internal/setup/system"
)

// nixosMarker exists on every NixOS system.
const nixosMarker = "/etc/NIXOS"

// Current is the installation as found.
type Current struct {
	Platform      Platform
	NixOS         bool
	DaemonUser    *system.User      // nil when missing
	ClientGroup   *system.GroupInfo // nil when missing
	InvokingUser  system.User
	Config        []byte // nil when missing
	ConfigMarked  bool
	Service       []byte // nil when missing
	ServiceMarked bool
	KeyExists     bool
	VaultExists   bool
	BinarySHA256  string         // "" when missing
	Health        *AccountHealth // nil when the daemon could not be asked

	// trust is the metadata of every path the trust check reads, as
	// Inspect found it. Build checks the chain against it. A Current not
	// made by Inspect has none, and Build refuses it.
	trust *trustSnapshot

	// daemonGroup is the daemon user's expected primary group as found:
	// chainmail on Linux, chainmail-clients on macOS. nil when missing.
	daemonGroup *system.GroupInfo
	// key is the vault key's metadata, read without following a final
	// symlink. Build refuses a key that is not daemon:daemon 0400.
	key system.FileInfo
}

// AccountHealth is what the daemon reported about the requested account.
type AccountHealth struct {
	Exists       bool
	SecretStored bool
	AuthOK       bool
}

// Inspect reads the current installation. It never writes.
//
// It runs as the invoking user as well as root, so a path it may not look
// at (the daemon's state directory is mode 0700) counts as missing. That
// is safe: the directory above it passed the trust check, so the invoking
// user cannot change it either, and bootstrap apply inspects again as
// root. Health is left nil; the CLI fills it in.
func Inspect(ctx context.Context, l Layout, f system.Files, a system.Accounts, invoking string) (Current, error) {
	cur := Current{Platform: l.Platform}

	nix, err := peek(f, nixosMarker)
	if err != nil {
		return Current{}, err
	}
	cur.NixOS = nix.Exists

	if cur.DaemonUser, err = lookupUser(ctx, a, l.DaemonUser); err != nil {
		return Current{}, err
	}
	if cur.ClientGroup, err = lookupGroup(ctx, a, l.ClientGroup); err != nil {
		return Current{}, err
	}
	if cur.daemonGroup, err = lookupGroup(ctx, a, daemonGroup(l)); err != nil {
		return Current{}, err
	}
	user, err := lookupUser(ctx, a, invoking)
	if err != nil {
		return Current{}, err
	}
	if user == nil {
		return Current{}, fmt.Errorf("inspect: invoking user %q does not exist", invoking)
	}
	cur.InvokingUser = *user

	if cur.Config, err = readRegular(f, l.Config); err != nil {
		return Current{}, err
	}
	cur.ConfigMarked = cur.Config != nil && HasMarker(cur.Config)
	if cur.Service, err = readRegular(f, l.Service); err != nil {
		return Current{}, err
	}
	cur.ServiceMarked = cur.Service != nil && HasMarker(cur.Service)

	key, err := peek(f, l.VaultKey)
	if err != nil {
		return Current{}, err
	}
	cur.KeyExists = key.Exists
	cur.key = key
	vault, err := peek(f, l.Vault)
	if err != nil {
		return Current{}, err
	}
	cur.VaultExists = vault.Exists

	bin, err := readRegular(f, l.Binary)
	if err != nil {
		return Current{}, err
	}
	if bin != nil {
		sum := sha256.Sum256(bin)
		cur.BinarySHA256 = hex.EncodeToString(sum[:])
	}

	if cur.trust, err = recordTrust(f, l); err != nil {
		return Current{}, err
	}
	return cur, nil
}

// lookupGroup returns the group called name, or nil when there is none.
func lookupGroup(ctx context.Context, a system.Accounts, name string) (*system.GroupInfo, error) {
	g, ok, err := a.LookupGroup(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("inspect group %s: %w", name, err)
	}
	if !ok {
		return nil, nil
	}
	return &g, nil
}

// lookupUser returns the user called name, or nil when there is none.
func lookupUser(ctx context.Context, a system.Accounts, name string) (*system.User, error) {
	u, ok, err := a.LookupUser(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("inspect user %s: %w", name, err)
	}
	if !ok {
		return nil, nil
	}
	return &u, nil
}

// peek is Lstat with a path the caller may not look at reported as
// missing.
func peek(f system.Files, path string) (system.FileInfo, error) {
	info, err := f.Lstat(path)
	if errors.Is(err, fs.ErrPermission) {
		return system.FileInfo{}, nil
	}
	if err != nil {
		return system.FileInfo{}, fmt.Errorf("inspect %s: %w", path, err)
	}
	return info, nil
}

// readRegular reads path when it is a regular file, and returns nil when
// it is missing or anything else, such as a symlink. The trust check
// refuses a symlink; Build refuses whatever else it is.
func readRegular(f system.Files, path string) ([]byte, error) {
	info, err := peek(f, path)
	if err != nil || !info.Exists || !info.Mode.IsRegular() {
		return nil, err
	}
	data, err := f.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

// trustPaths lists every path whose trust chain bootstrap checks: each
// layout entry, the socket directory, the vault, the lock and the log
// file. The socket itself is left out: it is group-writable on purpose,
// and the socket directory above it is checked.
func trustPaths(l Layout) []string {
	var paths []string
	for _, e := range l.Entries() {
		paths = append(paths, e.Path)
	}
	for _, p := range []string{l.SocketDir, l.Vault, l.Lock, l.LogFile} {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// trustSnapshot is an FSView over recorded metadata. A path that was not
// recorded is an error, so a check never passes on data nobody read.
type trustSnapshot struct {
	infos map[string]FileInfo
	errs  map[string]error
}

func (s *trustSnapshot) Lstat(path string) (FileInfo, error) {
	if err, ok := s.errs[path]; ok {
		return FileInfo{}, err
	}
	info, ok := s.infos[path]
	if !ok {
		return FileInfo{}, fmt.Errorf("%s was not inspected", path)
	}
	return info, nil
}

// trustRecorder is an FSView that records what it reads into snap and
// reports every path as owned by root and not writable by others, so a
// CheckTrust walk through it visits every component it could ever need,
// whoever owns it. Build then runs the real check on snap.
type trustRecorder struct {
	files system.Files
	snap  *trustSnapshot
}

func (r trustRecorder) Lstat(path string) (FileInfo, error) {
	info, err := peek(r.files, path)
	if err != nil {
		r.snap.errs[path] = err
		return FileInfo{}, err
	}
	fi := FileInfo(info)
	r.snap.infos[path] = fi
	view := fi
	view.UID = 0
	if view.Mode&fs.ModeSymlink == 0 {
		view.Mode &^= 0o022
	}
	return view, nil
}

// recordTrust records the metadata the trust check of every trust path
// reads.
func recordTrust(f system.Files, l Layout) (*trustSnapshot, error) {
	snap := &trustSnapshot{infos: map[string]FileInfo{}, errs: map[string]error{}}
	rec := trustRecorder{files: f, snap: snap}
	for _, p := range trustPaths(l) {
		// The permissive view only stops at a final symlink or a file
		// where a directory should be, and the real check stops there
		// too. A refusal here means nothing; Build reports it.
		err := CheckTrust(rec, p, []uint32{0})
		var r *Refusal
		if err != nil && !errors.As(err, &r) {
			return nil, err
		}
	}
	return snap, nil
}
