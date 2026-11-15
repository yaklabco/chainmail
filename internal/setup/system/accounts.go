package system

import (
	"context"
	"fmt"
	"strings"
)

// NewAccounts returns the Accounts for goos ("linux" or "darwin"), running
// commands through run. Any other goos gets an Accounts whose every call
// fails.
func NewAccounts(goos string, run Runner) Accounts {
	switch goos {
	case "linux":
		return linuxAccounts{run: run}
	case "darwin":
		return darwinAccounts{run: run}
	default:
		return unsupportedAccounts{goos: goos}
	}
}

// supplementaryGroups parses `id -Gn` output, which lists the primary group
// first, and returns the rest without repeats of the primary.
func supplementaryGroups(out []byte) []string {
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return nil
	}
	var groups []string
	for _, n := range names[1:] {
		if n != names[0] {
			groups = append(groups, n)
		}
	}
	return groups
}

// lookupGroups runs `id -Gn name` and returns the user's supplementary
// group names.
func lookupGroups(ctx context.Context, run Runner, name string) ([]string, error) {
	out, err := run(ctx, "id", "-Gn", name)
	if err != nil {
		return nil, fmt.Errorf("listing groups of %s: %w", name, err)
	}
	return supplementaryGroups(out), nil
}

type unsupportedAccounts struct{ goos string }

func (u unsupportedAccounts) err() error {
	return fmt.Errorf("accounts on %s: %w", u.goos, errUnsupported)
}

func (u unsupportedAccounts) LookupUser(context.Context, string) (User, bool, error) {
	return User{}, false, u.err()
}

func (u unsupportedAccounts) LookupGroup(context.Context, string) (GroupInfo, bool, error) {
	return GroupInfo{}, false, u.err()
}

func (u unsupportedAccounts) CreateGroup(context.Context, string) (GroupInfo, error) {
	return GroupInfo{}, u.err()
}

func (u unsupportedAccounts) CreateDaemonUser(context.Context, string, string, string) (User, error) {
	return User{}, u.err()
}

func (u unsupportedAccounts) AddMember(context.Context, string, string) error { return u.err() }
