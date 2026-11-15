package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sync"

	"github.com/yaklabco/chainmail/internal/setup/system"
)

// Fake ids used by the host fakes.
const (
	fakeUserUID     = 1000
	fakeLinuxDaemon = 990
	fakeLinuxGroup  = 989
	fakeDarwinUID   = 300
	fakeClientsGID  = 988
	fakeDarwinGID   = 301
)

// fakeBinary is the installed binary's contents in the host fakes.
var fakeBinary = []byte("chainmail binary v1")

// fakeBinarySHA is the hex SHA-256 of fakeBinary.
var fakeBinarySHA = sha256Hex(fakeBinary)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fakeNode is one path in fakeFiles.
type fakeNode struct {
	info system.FileInfo
	data []byte
}

// fakeFiles is a map-backed system.Files. Paths not in the map do not
// exist. errs makes Lstat or ReadFile of a path fail.
type fakeFiles struct {
	mu     sync.Mutex
	nodes  map[string]fakeNode
	errs   map[string]error
	writes []string
	locked map[string]bool
}

var _ system.Files = (*fakeFiles)(nil)

func newFakeFiles() *fakeFiles {
	return &fakeFiles{nodes: map[string]fakeNode{}, errs: map[string]error{}, locked: map[string]bool{}}
}

// resolve follows symlinks in the ancestors of path, as the kernel does, so
// /etc/chainmail on a macOS tree is /private/etc/chainmail. The final
// component is never followed. The caller holds mu, or owns f.
func (f *fakeFiles) resolve(path string) string {
	parts := splitPath(filepath.Clean(path))
	cur := "/"
	for hops := 0; len(parts) > 0 && hops < maxSymlinkHops; {
		name := parts[0]
		parts = parts[1:]
		if name == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		n, ok := f.nodes[next]
		if ok && len(parts) > 0 && n.info.Mode&fs.ModeSymlink != 0 {
			hops++
			if filepath.IsAbs(n.info.LinkDest) {
				cur = "/"
			}
			parts = append(splitPath(n.info.LinkDest), parts...)
			continue
		}
		cur = next
	}
	return cur
}

// dir adds a directory.
func (f *fakeFiles) dir(path string, uid, gid uint32, perm fs.FileMode) *fakeFiles {
	path = f.resolve(path)
	f.nodes[path] = fakeNode{info: system.FileInfo{Exists: true, Mode: fs.ModeDir | perm, UID: uid, GID: gid}}
	return f
}

// file adds a regular file holding data.
func (f *fakeFiles) file(path string, uid, gid uint32, perm fs.FileMode, data []byte) *fakeFiles {
	path = f.resolve(path)
	f.nodes[path] = fakeNode{info: system.FileInfo{Exists: true, Mode: perm, UID: uid, GID: gid}, data: data}
	return f
}

// symlink adds a symlink to dest owned by uid.
func (f *fakeFiles) symlink(path string, uid uint32, dest string) *fakeFiles {
	path = f.resolve(path)
	f.nodes[path] = fakeNode{info: system.FileInfo{Exists: true, Mode: fs.ModeSymlink | 0o777, UID: uid, LinkDest: dest}}
	return f
}

// remove deletes path from the tree.
func (f *fakeFiles) remove(path string) *fakeFiles {
	path = f.resolve(path)
	delete(f.nodes, path)
	return f
}

func (f *fakeFiles) Lstat(path string) (system.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if err := f.errs[path]; err != nil {
		return system.FileInfo{}, fmt.Errorf("inspecting %s: %w", path, err)
	}
	n, ok := f.nodes[path]
	if !ok {
		return system.FileInfo{}, nil
	}
	return n.info, nil
}

func (f *fakeFiles) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if err := f.errs[path]; err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	n, ok := f.nodes[path]
	switch {
	case !ok:
		return nil, fmt.Errorf("opening %s: %w", path, fs.ErrNotExist)
	case !n.info.Mode.IsRegular():
		return nil, fmt.Errorf("opening %s: not a regular file", path)
	}
	return slices.Clone(n.data), nil
}

func (f *fakeFiles) EnsureDir(path string, uid, gid uint32, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if err := f.parentDir(path); err != nil {
		return err
	}
	if n, ok := f.nodes[path]; ok && !n.info.Mode.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", path)
	}
	f.nodes[path] = fakeNode{info: system.FileInfo{Exists: true, Mode: fs.ModeDir | mode, UID: uid, GID: gid}}
	f.writes = append(f.writes, path)
	return nil
}

func (f *fakeFiles) WriteFile(path string, data []byte, uid, gid uint32, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if err := f.parentDir(path); err != nil {
		return err
	}
	f.nodes[path] = fakeNode{info: system.FileInfo{Exists: true, Mode: mode, UID: uid, GID: gid}, data: slices.Clone(data)}
	f.writes = append(f.writes, path)
	return nil
}

func (f *fakeFiles) CreateExclusive(path string, data []byte, uid, gid uint32, mode fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if err := f.parentDir(path); err != nil {
		return err
	}
	if _, ok := f.nodes[path]; ok {
		return fmt.Errorf("creating %s: %w", path, fs.ErrExist)
	}
	f.nodes[path] = fakeNode{info: system.FileInfo{Exists: true, Mode: mode, UID: uid, GID: gid}, data: slices.Clone(data)}
	f.writes = append(f.writes, path)
	return nil
}

func (f *fakeFiles) Lock(path string) (func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = f.resolve(path)
	if f.locked[path] {
		return nil, fmt.Errorf("locking %s: already locked", path)
	}
	if _, ok := f.nodes[path]; !ok {
		f.nodes[path] = fakeNode{info: system.FileInfo{Exists: true, Mode: 0o600}}
	}
	f.locked[path] = true
	return func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.locked, path)
		return nil
	}, nil
}

// parentDir fails unless path's parent is a directory. The caller holds mu.
func (f *fakeFiles) parentDir(path string) error {
	parent := filepath.Dir(path)
	if n, ok := f.nodes[parent]; !ok || !n.info.Mode.IsDir() {
		return fmt.Errorf("parent of %s: %w", path, fs.ErrNotExist)
	}
	return nil
}

// fakeAccounts is a map-backed system.Accounts that records the calls
// that change something.
type fakeAccounts struct {
	mu      sync.Mutex
	users   map[string]system.User
	groups  map[string]system.GroupInfo
	nextID  uint32
	changes []string
}

var _ system.Accounts = (*fakeAccounts)(nil)

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{users: map[string]system.User{}, groups: map[string]system.GroupInfo{}, nextID: 500}
}

func (a *fakeAccounts) addUser(u system.User) *fakeAccounts {
	a.users[u.Name] = u
	return a
}

func (a *fakeAccounts) addGroup(g system.GroupInfo) *fakeAccounts {
	a.groups[g.Name] = g
	return a
}

func (a *fakeAccounts) LookupUser(_ context.Context, name string) (system.User, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[name]
	u.Groups = slices.Clone(u.Groups)
	return u, ok, nil
}

func (a *fakeAccounts) LookupGroup(_ context.Context, name string) (system.GroupInfo, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.groups[name]
	g.Members = slices.Clone(g.Members)
	return g, ok, nil
}

func (a *fakeAccounts) CreateGroup(_ context.Context, name string) (system.GroupInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.groups[name]; ok {
		return system.GroupInfo{}, fmt.Errorf("group %s exists", name)
	}
	a.nextID++
	g := system.GroupInfo{Name: name, GID: a.nextID}
	a.groups[name] = g
	a.changes = append(a.changes, "create-group "+name)
	return g, nil
}

func (a *fakeAccounts) CreateDaemonUser(_ context.Context, name, clients, home string) (system.User, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.users[name]; ok {
		return system.User{}, fmt.Errorf("user %s exists", name)
	}
	a.nextID++
	u := system.User{Name: name, UID: a.nextID, GID: a.nextID, Home: home, Shell: "/usr/sbin/nologin", Groups: []string{clients}}
	a.users[name] = u
	a.changes = append(a.changes, "create-user "+name)
	return u, nil
}

func (a *fakeAccounts) AddMember(_ context.Context, group, user string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.groups[group]
	if !ok {
		return fmt.Errorf("group %s does not exist", group)
	}
	if !slices.Contains(g.Members, user) {
		g.Members = append(g.Members, user)
	}
	a.groups[group] = g
	if u, ok := a.users[user]; ok && !slices.Contains(u.Groups, group) {
		u.Groups = append(u.Groups, group)
		a.users[user] = u
	}
	a.changes = append(a.changes, "add-member "+group+" "+user)
	return nil
}

// host is a fake machine: its files and its users and groups.
type host struct {
	files    *fakeFiles
	accounts *fakeAccounts
}

// freshHost is a machine with no chainmail on it and one login user, alice.
func freshHost(p Platform) host {
	f := newFakeFiles()
	a := newFakeAccounts()
	f.dir("/", 0, 0, 0o755)
	switch p {
	case Linux:
		f.dir("/etc", 0, 0, 0o755).
			dir("/etc/systemd", 0, 0, 0o755).
			dir("/etc/systemd/system", 0, 0, 0o755).
			dir("/usr", 0, 0, 0o755).
			dir("/usr/local", 0, 0, 0o755).
			dir("/usr/local/libexec", 0, 0, 0o755).
			dir("/var", 0, 0, 0o755).
			dir("/var/lib", 0, 0, 0o755).
			dir("/run", 0, 0, 0o755)
		a.addUser(system.User{Name: "alice", UID: fakeUserUID, GID: fakeUserUID, Home: "/home/alice", Shell: "/bin/bash"})
	case Darwin:
		f.symlink("/etc", 0, "private/etc").
			symlink("/var", 0, "private/var").
			dir("/private", 0, 0, 0o755).
			dir("/private/etc", 0, 0, 0o755).
			dir("/private/var", 0, 0, 0o755).
			dir("/private/var/db", 0, 0, 0o755).
			dir("/private/var/log", 0, 0, 0o755).
			dir("/Library", 0, 0, 0o755).
			dir("/Library/Application Support", 0, 0, 0o755).
			dir("/Library/LaunchDaemons", 0, 0, 0o755)
		a.addUser(system.User{Name: "alice", UID: 501, GID: 20, Home: "/Users/alice", Shell: "/bin/zsh"})
	}
	return host{files: f, accounts: a}
}

// installedHost is a machine bootstrap already set up for spec, with
// alice in the client group and the binary from fakeBinary.
func installedHost(p Platform, spec AccountSpec, client *OAuthClientSpec) host {
	h := freshHost(p)
	l := LayoutFor(p)
	daemonUID, daemonGID, clientsGID := uint32(fakeLinuxDaemon), uint32(fakeLinuxGroup), uint32(fakeClientsGID)
	daemonUser := system.User{Name: l.DaemonUser, UID: daemonUID, GID: daemonGID, Home: l.StateDir, Shell: "/usr/sbin/nologin", Groups: []string{l.ClientGroup}}
	if p == Darwin {
		daemonUID, daemonGID, clientsGID = fakeDarwinUID, fakeDarwinGID, fakeDarwinGID
		daemonUser = system.User{Name: l.DaemonUser, UID: daemonUID, GID: clientsGID, Home: "/var/empty", Shell: "/usr/bin/false"}
	}
	h.accounts.addUser(daemonUser)
	h.accounts.addGroup(system.GroupInfo{Name: l.ClientGroup, GID: clientsGID, Members: []string{"alice"}})
	if p == Linux {
		h.accounts.addGroup(system.GroupInfo{Name: l.DaemonUser, GID: daemonGID})
		h.accounts.users[l.DaemonUser] = daemonUser
	}
	alice := h.accounts.users["alice"]
	alice.Groups = append(alice.Groups, l.ClientGroup)
	h.accounts.users["alice"] = alice

	f := h.files
	data := map[string][]byte{
		"binary":    fakeBinary,
		"config":    NewConfig(l, spec, client),
		"vault-key": []byte("AGE-SECRET-KEY-FAKE\n"),
		"service":   RenderService(l),
	}
	for _, e := range l.Entries() {
		uid, gid := uint32(0), uint32(0)
		if e.Owner == OwnerDaemon {
			uid = daemonUID
		}
		if e.Group == GroupDaemon {
			gid = daemonGID
			if p == Darwin {
				gid = clientsGID
			}
		}
		path := e.Path
		if e.Kind == KindDir {
			f.dir(path, uid, gid, e.Mode)
			continue
		}
		f.file(path, uid, gid, e.Mode, data[e.Name])
	}
	if p == Linux {
		f.dir(l.SocketDir, daemonUID, daemonGID, 0o755)
	}
	f.file(l.Socket, daemonUID, clientsGID, 0o660|fs.ModeSocket, nil)
	f.file(l.Vault, daemonUID, daemonGID, 0o600, []byte("vault"))
	if p == Darwin {
		f.file(l.LogFile, daemonUID, clientsGID, 0o600, nil)
	}
	return h
}

// inspect runs Inspect on h for alice and fails the test on error.
func (h host) inspect(t interface {
	Helper()
	Fatalf(string, ...any)
}, p Platform,
) Current {
	t.Helper()
	cur, err := Inspect(context.Background(), LayoutFor(p), h.files, h.accounts, "alice")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return cur
}
