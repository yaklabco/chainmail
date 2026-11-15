package setuptest

import (
	"bytes"
	"io"
	"strings"
	"sync"

	"github.com/yaklabco/chainmail/internal/setup/system"
)

// The uids of alice, the login user on a FreshHost.
const (
	AliceLinuxUID  = 1000
	AliceDarwinUID = 501
)

// Host is a fake machine: its files and its users and groups.
type Host struct {
	Files    *Files
	Accounts *Accounts
}

// FreshHost is a machine with no chainmail on it and one login user,
// alice. goos is "linux" or "darwin". On macOS /etc and /var are links
// into /private, and /private/var/lib does not exist, as on a real Mac.
func FreshHost(goos string) Host {
	f := NewFiles()
	a := NewAccounts(goos)
	f.Dir("/", 0, 0, 0o755)
	switch goos {
	case "darwin":
		f.Symlink("/etc", 0, "private/etc").
			Symlink("/var", 0, "private/var").
			Dir("/private", 0, 0, 0o755).
			Dir("/private/etc", 0, 0, 0o755).
			Dir("/private/var", 0, 0, 0o755).
			Dir("/private/var/db", 0, 0, 0o755).
			Dir("/private/var/log", 0, 0, 0o755).
			Dir("/Library", 0, 0, 0o755).
			Dir("/Library/Application Support", 0, 0, 0o755).
			Dir("/Library/LaunchDaemons", 0, 0, 0o755)
		a.AddUser(system.User{Name: "alice", UID: AliceDarwinUID, GID: 20, Home: "/Users/alice", Shell: "/bin/zsh"})
	default:
		f.Dir("/etc", 0, 0, 0o755).
			Dir("/etc/systemd", 0, 0, 0o755).
			Dir("/etc/systemd/system", 0, 0, 0o755).
			Dir("/usr", 0, 0, 0o755).
			Dir("/usr/local", 0, 0, 0o755).
			Dir("/usr/local/libexec", 0, 0, 0o755).
			Dir("/var", 0, 0, 0o755).
			Dir("/var/lib", 0, 0, 0o755).
			Dir("/run", 0, 0, 0o755)
		a.AddUser(system.User{Name: "alice", UID: AliceLinuxUID, GID: AliceLinuxUID, Home: "/home/alice", Shell: "/bin/bash"})
	}
	return Host{Files: f, Accounts: a}
}

// TTY is a fake controlling terminal: reads come from the input it was
// made with, and writes collect in Output.
type TTY struct {
	mu     sync.Mutex
	in     io.Reader
	out    bytes.Buffer
	closed bool
}

// NewTTY returns a terminal whose input is input.
func NewTTY(input string) *TTY { return &TTY{in: strings.NewReader(input)} }

// Read implements io.Reader.
func (t *TTY) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.in.Read(p)
}

// Write implements io.Writer.
func (t *TTY) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.out.Write(p)
}

// Close implements io.Closer.
func (t *TTY) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

// Output returns everything written so far.
func (t *TTY) Output() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.out.String()
}

// Closed reports whether Close was called.
func (t *TTY) Closed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}
