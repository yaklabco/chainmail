package setuptest

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/yaklabco/chainmail/internal/setup/system"
)

// firstFakeID is one less than the first id CreateGroup and
// CreateDaemonUser hand out.
const firstFakeID = 500

// Accounts is a map-backed system.Accounts that records the calls that
// change something.
type Accounts struct {
	// Journal, when set, gets the same entries as Changes.
	Journal *Journal

	goos    string
	mu      sync.Mutex
	users   map[string]system.User
	groups  map[string]system.GroupInfo
	nextID  uint32
	changes []string
}

var _ system.Accounts = (*Accounts)(nil)

// NewAccounts returns an empty user and group database that creates
// users the way goos ("linux" or "darwin") does.
func NewAccounts(goos string) *Accounts {
	return &Accounts{goos: goos, users: map[string]system.User{}, groups: map[string]system.GroupInfo{}, nextID: firstFakeID}
}

// AddUser adds u, replacing any user of the same name.
func (a *Accounts) AddUser(u system.User) *Accounts {
	a.mu.Lock()
	defer a.mu.Unlock()
	u.Groups = slices.Clone(u.Groups)
	a.users[u.Name] = u
	return a
}

// AddGroup adds g, replacing any group of the same name.
func (a *Accounts) AddGroup(g system.GroupInfo) *Accounts {
	a.mu.Lock()
	defer a.mu.Unlock()
	g.Members = slices.Clone(g.Members)
	a.groups[g.Name] = g
	return a
}

// RemoveGroup removes the group called name, if there is one.
func (a *Accounts) RemoveGroup(name string) *Accounts {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.groups, name)
	return a
}

// User returns the user called name, and false when there is none.
func (a *Accounts) User(name string) (system.User, bool) {
	u, ok, _ := a.LookupUser(context.Background(), name)
	return u, ok
}

// Group returns the group called name, and false when there is none.
func (a *Accounts) Group(name string) (system.GroupInfo, bool) {
	g, ok, _ := a.LookupGroup(context.Background(), name)
	return g, ok
}

// Changes lists the changes made through the system.Accounts methods:
// "create-group <name>", "create-user <name>" and "add-member <group>
// <user>".
func (a *Accounts) Changes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.changes)
}

// LookupUser implements system.Accounts.
func (a *Accounts) LookupUser(_ context.Context, name string) (system.User, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[name]
	u.Groups = slices.Clone(u.Groups)
	return u, ok, nil
}

// LookupGroup implements system.Accounts.
func (a *Accounts) LookupGroup(_ context.Context, name string) (system.GroupInfo, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.groups[name]
	g.Members = slices.Clone(g.Members)
	return g, ok, nil
}

// CreateGroup implements system.Accounts.
func (a *Accounts) CreateGroup(_ context.Context, name string) (system.GroupInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.groups[name]; ok {
		return system.GroupInfo{}, fmt.Errorf("group %s exists", name)
	}
	a.nextID++
	g := system.GroupInfo{Name: name, GID: a.nextID}
	a.groups[name] = g
	a.record("create-group " + name)
	return g, nil
}

// CreateDaemonUser implements system.Accounts. On Linux the user gets a
// primary group of its own name and clients as a supplementary group, as
// useradd --user-group does. On macOS clients is its primary group.
func (a *Accounts) CreateDaemonUser(_ context.Context, name, clients, home string) (system.User, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.users[name]; ok {
		return system.User{}, fmt.Errorf("user %s exists", name)
	}
	g, ok := a.groups[clients]
	if !ok {
		return system.User{}, fmt.Errorf("group %s does not exist", clients)
	}
	a.nextID++
	var u system.User
	if a.goos == "darwin" {
		u = system.User{Name: name, UID: a.nextID, GID: g.GID, Home: "/var/empty", Shell: "/usr/bin/false"}
	} else {
		u = system.User{Name: name, UID: a.nextID, GID: a.nextID, Home: home, Shell: "/usr/sbin/nologin", Groups: []string{clients}}
		if _, ok := a.groups[name]; !ok {
			a.groups[name] = system.GroupInfo{Name: name, GID: a.nextID}
		}
		if !slices.Contains(g.Members, name) {
			g.Members = append(g.Members, name)
			a.groups[clients] = g
		}
	}
	a.users[name] = u
	a.record("create-user " + name)
	return u, nil
}

// AddMember implements system.Accounts.
func (a *Accounts) AddMember(_ context.Context, group, user string) error {
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
	a.record("add-member " + group + " " + user)
	return nil
}

// record notes a change. The caller holds mu.
func (a *Accounts) record(change string) {
	a.changes = append(a.changes, change)
	a.Journal.Add(change)
}
