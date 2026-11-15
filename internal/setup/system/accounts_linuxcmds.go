package system

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// getentNotFound is getent's exit status for a key that is not in the
// database.
const getentNotFound = 2

// linuxAccounts manages users and groups with getent, groupadd, useradd and
// usermod.
type linuxAccounts struct{ run Runner }

func (a linuxAccounts) LookupUser(ctx context.Context, name string) (User, bool, error) {
	out, err := a.run(ctx, "getent", "passwd", name)
	if exitCode(err) == getentNotFound {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("looking up user %s: %w", name, err)
	}
	u, err := parsePasswd(out)
	if err != nil {
		return User{}, false, fmt.Errorf("looking up user %s: %w", name, err)
	}
	if u.Groups, err = lookupGroups(ctx, a.run, name); err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

func (a linuxAccounts) LookupGroup(ctx context.Context, name string) (GroupInfo, bool, error) {
	out, err := a.run(ctx, "getent", "group", name)
	if exitCode(err) == getentNotFound {
		return GroupInfo{}, false, nil
	}
	if err != nil {
		return GroupInfo{}, false, fmt.Errorf("looking up group %s: %w", name, err)
	}
	g, err := parseGroup(out)
	if err != nil {
		return GroupInfo{}, false, fmt.Errorf("looking up group %s: %w", name, err)
	}
	return g, true, nil
}

func (a linuxAccounts) CreateGroup(ctx context.Context, name string) (GroupInfo, error) {
	if _, err := a.run(ctx, "groupadd", "--system", name); err != nil {
		return GroupInfo{}, fmt.Errorf("creating group %s: %w", name, err)
	}
	g, ok, err := a.LookupGroup(ctx, name)
	if err != nil {
		return GroupInfo{}, err
	}
	if !ok {
		return GroupInfo{}, fmt.Errorf("group %s is missing after groupadd", name)
	}
	return g, nil
}

func (a linuxAccounts) CreateDaemonUser(ctx context.Context, name, clients, home string) (User, error) {
	_, err := a.run(ctx, "useradd",
		"--system", "--user-group",
		"--groups", clients,
		"--home-dir", home,
		"--shell", "/usr/sbin/nologin",
		name)
	if err != nil {
		return User{}, fmt.Errorf("creating user %s: %w", name, err)
	}
	u, ok, err := a.LookupUser(ctx, name)
	if err != nil {
		return User{}, err
	}
	if !ok {
		return User{}, fmt.Errorf("user %s is missing after useradd", name)
	}
	return u, nil
}

func (a linuxAccounts) AddMember(ctx context.Context, group, user string) error {
	if _, err := a.run(ctx, "usermod", "-aG", group, user); err != nil {
		return fmt.Errorf("adding %s to %s: %w", user, group, err)
	}
	return nil
}

// parsePasswd parses one getent passwd line:
// name:password:uid:gid:gecos:home:shell.
func parsePasswd(out []byte) (User, error) {
	f := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(f) != 7 {
		return User{}, fmt.Errorf("unexpected passwd entry with %d fields", len(f))
	}
	uid, err := parseID(f[2])
	if err != nil {
		return User{}, fmt.Errorf("passwd uid: %w", err)
	}
	gid, err := parseID(f[3])
	if err != nil {
		return User{}, fmt.Errorf("passwd gid: %w", err)
	}
	return User{Name: f[0], UID: uid, GID: gid, Home: f[5], Shell: f[6]}, nil
}

// parseGroup parses one getent group line: name:password:gid:member,member.
func parseGroup(out []byte) (GroupInfo, error) {
	f := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(f) != 4 {
		return GroupInfo{}, fmt.Errorf("unexpected group entry with %d fields", len(f))
	}
	gid, err := parseID(f[2])
	if err != nil {
		return GroupInfo{}, fmt.Errorf("group gid: %w", err)
	}
	g := GroupInfo{Name: f[0], GID: gid}
	if f[3] != "" {
		g.Members = strings.Split(f[3], ",")
	}
	return g, nil
}

// parseID parses a uid or gid.
func parseID(s string) (uint32, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing id %q: %w", s, err)
	}
	return uint32(n), nil
}
