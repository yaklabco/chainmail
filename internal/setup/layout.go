package setup

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
)

// Platform is an operating system bootstrap supports.
type Platform string

// The platforms bootstrap supports.
const (
	Linux  Platform = "linux"
	Darwin Platform = "darwin"
)

// Owner and Group name who owns a path, resolved to ids at apply time.
type Owner string

// The owners a layout entry can have.
const (
	OwnerRoot   Owner = "root"
	OwnerDaemon Owner = "daemon"
)

// Group names the group that owns a path, resolved to a gid at apply time.
type Group string

// The groups a layout entry can have.
const (
	GroupRoot    Group = "root"    // root on Linux, wheel on macOS
	GroupDaemon  Group = "daemon"  // chainmail on Linux, chainmail-clients on macOS
	GroupClients Group = "clients" // chainmail-clients
)

// Kind says whether an entry is a directory or a regular file.
type Kind string

// The kinds of layout entry.
const (
	KindDir  Kind = "dir"
	KindFile Kind = "file"
)

// Entry is one path bootstrap creates or checks, with its owner and mode.
type Entry struct {
	Name  string // stable id, such as "config" or "vault-key"
	Path  string
	Kind  Kind
	Owner Owner
	Group Group
	Mode  fs.FileMode
}

// Layout is the fixed set of paths for one platform. Nothing in a plan may
// name a path outside it.
type Layout struct {
	Platform     Platform
	DaemonUser   string // "chainmail" or "_chainmail"
	ClientGroup  string // "chainmail-clients"
	RootGroup    string // "root" or "wheel"
	BinaryDir    string
	Binary       string
	ConfigDir    string // /etc/chainmail
	Config       string // /etc/chainmail/config.toml
	VaultKey     string // /etc/chainmail/vault-key
	Lock         string // /etc/chainmail/.bootstrap.lock
	StateDir     string // /var/lib/chainmail
	Vault        string // /var/lib/chainmail/vault.age
	SocketDir    string // /run/chainmail or /var/db/chainmail
	Socket       string
	Service      string // unit or plist path
	ServiceLabel string // "chainmail.service" or "sh.chainmail.daemon"
	LogDir       string // "" on Linux, /var/log/chainmail on macOS
	LogFile      string // "" on Linux, /var/log/chainmail/daemon.log on macOS
}

// LayoutFor returns the layout for p. It panics for any other platform.
func LayoutFor(p Platform) Layout {
	switch p {
	case Linux:
		return Layout{
			Platform:     Linux,
			DaemonUser:   "chainmail",
			ClientGroup:  "chainmail-clients",
			RootGroup:    "root",
			BinaryDir:    "/usr/local/libexec/chainmail",
			Binary:       "/usr/local/libexec/chainmail/chainmail",
			ConfigDir:    "/etc/chainmail",
			Config:       "/etc/chainmail/config.toml",
			VaultKey:     "/etc/chainmail/vault-key",
			Lock:         "/etc/chainmail/.bootstrap.lock",
			StateDir:     "/var/lib/chainmail",
			Vault:        "/var/lib/chainmail/vault.age",
			SocketDir:    "/run/chainmail",
			Socket:       "/run/chainmail/chainmail.sock",
			Service:      "/etc/systemd/system/chainmail.service",
			ServiceLabel: "chainmail.service",
		}
	case Darwin:
		return Layout{
			Platform:     Darwin,
			DaemonUser:   "_chainmail",
			ClientGroup:  "chainmail-clients",
			RootGroup:    "wheel",
			BinaryDir:    "/Library/Application Support/chainmail/bin",
			Binary:       "/Library/Application Support/chainmail/bin/chainmail",
			ConfigDir:    "/etc/chainmail",
			Config:       "/etc/chainmail/config.toml",
			VaultKey:     "/etc/chainmail/vault-key",
			Lock:         "/etc/chainmail/.bootstrap.lock",
			StateDir:     "/var/lib/chainmail",
			Vault:        "/var/lib/chainmail/vault.age",
			SocketDir:    "/var/db/chainmail",
			Socket:       "/var/db/chainmail/chainmail.sock",
			Service:      "/Library/LaunchDaemons/sh.chainmail.daemon.plist",
			ServiceLabel: "sh.chainmail.daemon",
			LogDir:       "/var/log/chainmail",
			LogFile:      "/var/log/chainmail/daemon.log",
		}
	default:
		panic(fmt.Sprintf("setup: no layout for platform %q", p))
	}
}

// Entries lists every directory and file bootstrap creates, in creation
// order, with the owner and mode from the spec's Part 2 table. The socket
// is not listed: the daemon creates it. /run/chainmail is not listed on
// Linux: systemd creates it.
func (l Layout) Entries() []Entry {
	entries := []Entry{
		{Name: "binary-dir", Path: l.BinaryDir, Kind: KindDir, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o755},
		{Name: "binary", Path: l.Binary, Kind: KindFile, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o755},
		{Name: "config-dir", Path: l.ConfigDir, Kind: KindDir, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o755},
		{Name: "config", Path: l.Config, Kind: KindFile, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o644},
		{Name: "vault-key", Path: l.VaultKey, Kind: KindFile, Owner: OwnerDaemon, Group: GroupDaemon, Mode: 0o400},
		{Name: "state-dir", Path: l.StateDir, Kind: KindDir, Owner: OwnerDaemon, Group: GroupDaemon, Mode: 0o700},
	}
	if l.Platform == Darwin {
		entries = append(entries,
			Entry{Name: "socket-dir", Path: l.SocketDir, Kind: KindDir, Owner: OwnerDaemon, Group: GroupDaemon, Mode: 0o755},
			Entry{Name: "log-dir", Path: l.LogDir, Kind: KindDir, Owner: OwnerDaemon, Group: GroupDaemon, Mode: 0o700},
		)
	}
	return append(entries,
		Entry{Name: "service", Path: l.Service, Kind: KindFile, Owner: OwnerRoot, Group: GroupRoot, Mode: 0o644},
	)
}

// Entry returns the entry called name, and false when there is none.
func (l Layout) Entry(name string) (Entry, bool) {
	for _, e := range l.Entries() {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Allowed reports whether path is one of the layout's paths: an entry, the
// socket directory and socket, the lock, the vault or the log file. Paths
// are compared after filepath.Clean, and a path with a ".." component or
// one that is not absolute is never allowed.
func (l Layout) Allowed(path string) bool {
	if !filepath.IsAbs(path) || hasDotDot(path) {
		return false
	}
	path = filepath.Clean(path)
	return slices.Contains(l.paths(), path)
}

// paths lists every non-empty path the layout names.
func (l Layout) paths() []string {
	var paths []string
	for _, e := range l.Entries() {
		paths = append(paths, e.Path)
	}
	for _, p := range []string{l.SocketDir, l.Socket, l.Lock, l.Vault, l.LogFile} {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// ServiceArgs is the daemon's argument list after the binary path.
func (l Layout) ServiceArgs() []string {
	return []string{
		"daemon",
		"--config", l.Config,
		"--state-dir", l.StateDir,
		"--vault-key-file", l.VaultKey,
	}
}

// hasDotDot reports whether path has a ".." component.
func hasDotDot(path string) bool {
	return slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "..")
}
