// Package system is the part of chainmail bootstrap that touches the
// operating system: files with owners and modes, users and groups, and the
// systemd or launchd service. Every external command goes through a Runner,
// so tests replace it with a fake and never run sudo, useradd, dscl,
// systemctl or launchctl. The real runner runs only the root-owned copies in
// the fixed system directories, never a command found through PATH, with a
// minimal environment. Linux and macOS logic is chosen at run time by a
// goos string, so both are built and tested on either platform.
//
// This package must not import internal/setup: setup imports it.
package system

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Runner runs a command and returns its stdout. Stderr goes into the error.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Files does file work as root. OSFiles is the real one.
type Files interface {
	Lstat(path string) (FileInfo, error)
	ReadFile(path string) ([]byte, error)
	// EnsureDir creates path if missing, then sets owner and mode.
	EnsureDir(path string, uid, gid uint32, mode fs.FileMode) error
	// WriteFile replaces path atomically (temp file in the same directory,
	// fsync, rename, fsync dir) with the given owner and mode.
	WriteFile(path string, data []byte, uid, gid uint32, mode fs.FileMode) error
	// CreateExclusive creates path with O_CREAT|O_EXCL and the given owner
	// and mode, then writes data. An existing path is fs.ErrExist.
	CreateExclusive(path string, data []byte, uid, gid uint32, mode fs.FileMode) error
	// RemoveFile removes path. A final symlink is removed, never followed.
	// A missing path is an error wrapping fs.ErrNotExist.
	RemoveFile(path string) error
	// Lock takes an exclusive flock on path, creating it with mode 0600.
	Lock(path string) (unlock func() error, err error)
}

// FileInfo mirrors setup.FileInfo so this package does not import setup.
type FileInfo struct {
	Exists   bool
	Mode     fs.FileMode
	UID, GID uint32
	LinkDest string
}

// User is a user account as the system reports it.
type User struct {
	Name     string
	UID, GID uint32
	Home     string
	Shell    string
	Groups   []string // names of supplementary groups
}

// GroupInfo is a group as the system reports it.
type GroupInfo struct {
	Name    string
	GID     uint32
	Members []string
}

// Accounts manages users and groups.
type Accounts interface {
	LookupUser(ctx context.Context, name string) (User, bool, error)
	LookupGroup(ctx context.Context, name string) (GroupInfo, bool, error)
	// CreateGroup makes a system group. On macOS it picks the gid.
	CreateGroup(ctx context.Context, name string) (GroupInfo, error)
	// CreateDaemonUser makes the daemon's system user. Linux: useradd
	// --system --user-group --groups <clients> --home-dir <home> --shell
	// /usr/sbin/nologin <name>. macOS: dscl with the lowest free uid in
	// [300, 500), primary group <clients>, shell /usr/bin/false, home
	// /var/empty, IsHidden 1.
	CreateDaemonUser(ctx context.Context, name, clients, home string) (User, error)
	// AddMember adds user to group. Linux: usermod -aG. macOS: dseditgroup.
	AddMember(ctx context.Context, group, user string) error
}

// ServiceState is what the service manager reports.
type ServiceState struct {
	Loaded  bool
	Running bool
}

// Service drives systemd or launchd for the one chainmail service.
type Service interface {
	State(ctx context.Context) (ServiceState, error)
	// Activate makes the service run the definition on disk. changed says
	// the definition file was just written with new contents. systemd:
	// daemon-reload when changed, then enable --now. launchd: bootstrap
	// when not loaded; bootout then bootstrap when loaded and changed;
	// kickstart when loaded, unchanged and not running.
	Activate(ctx context.Context, changed bool) error
	// Restart restarts a running service. systemd: restart. launchd:
	// kickstart -k.
	Restart(ctx context.Context) error
	// Logs returns the last n lines of the daemon's log.
	Logs(ctx context.Context, n int) ([]byte, error)
}

// errUnsupported is returned for a goos other than linux and darwin.
var errUnsupported = errors.New("unsupported platform")

// commandError is the error ExecRunner returns when name fails with err:
// the command line, err (still reachable through errors.As and errors.Is)
// and the trimmed stderr, if any.
func commandError(name string, args []string, err error, stderr []byte) error {
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		return fmt.Errorf("%s: %w", commandLine(name, args), err)
	}
	return fmt.Errorf("%s: %w: %s", commandLine(name, args), err, msg)
}

// commandLine renders a command for an error message. Bootstrap never puts
// a secret in argv, so the arguments are safe to show.
func commandLine(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

// exitCode returns the exit status carried by err: 0 for nil, the status
// for an error that wraps one (such as *exec.ExitError), and -1 otherwise.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var e interface{ ExitCode() int }
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return -1
}

// Lowest and highest-plus-one ids bootstrap may pick for a macOS user or
// group: system accounts below 500 stay out of the login window.
const (
	firstFreeID = 300
	endFreeID   = 500
)

// pickFreeID returns the lowest id in [300, 500) not in used, and false
// when every one is taken.
func pickFreeID(used map[uint32]bool) (uint32, bool) {
	for id := uint32(firstFreeID); id < endFreeID; id++ {
		if !used[id] {
			return id, true
		}
	}
	return 0, false
}
