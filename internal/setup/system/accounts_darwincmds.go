package system

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// dsNotFoundMarker is in dscl's error when the record does not exist.
const dsNotFoundMarker = "eDSRecordNotFound"

// The daemon user's fixed shape on macOS, from the deploy page.
const (
	darwinDaemonShell = "/usr/bin/false"
	darwinDaemonHome  = "/var/empty"
)

// darwinAccounts manages users and groups with dscl and dseditgroup.
type darwinAccounts struct{ run Runner }

func (a darwinAccounts) LookupUser(ctx context.Context, name string) (User, bool, error) {
	attrs, ok, err := a.read(ctx, "/Users/"+name, "UniqueID", "PrimaryGroupID", "NFSHomeDirectory", "UserShell")
	if err != nil || !ok {
		return User{}, ok, err
	}
	uid, err := firstID(attrs, "UniqueID")
	if err != nil {
		return User{}, false, fmt.Errorf("looking up user %s: %w", name, err)
	}
	gid, err := firstID(attrs, "PrimaryGroupID")
	if err != nil {
		return User{}, false, fmt.Errorf("looking up user %s: %w", name, err)
	}
	u := User{
		Name:  name,
		UID:   uid,
		GID:   gid,
		Home:  first(attrs, "NFSHomeDirectory"),
		Shell: first(attrs, "UserShell"),
	}
	if u.Groups, err = lookupGroups(ctx, a.run, name); err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

func (a darwinAccounts) LookupGroup(ctx context.Context, name string) (GroupInfo, bool, error) {
	attrs, ok, err := a.read(ctx, "/Groups/"+name, "PrimaryGroupID", "GroupMembership")
	if err != nil || !ok {
		return GroupInfo{}, ok, err
	}
	gid, err := firstID(attrs, "PrimaryGroupID")
	if err != nil {
		return GroupInfo{}, false, fmt.Errorf("looking up group %s: %w", name, err)
	}
	return GroupInfo{Name: name, GID: gid, Members: attrs["GroupMembership"]}, true, nil
}

// dsMissingAttrMarkers are in dscl's error when a record exists but lacks
// one of the attributes asked for, such as an empty group's
// GroupMembership.
var dsMissingAttrMarkers = []string{"No such key", "eDSAttributeNotFound"}

// read runs dscl -plist . -read on record and returns its attributes,
// keyed without the dsAttrTypeStandard: prefix. A missing record is false
// with no error. dscl exits non-zero when one of the attributes is missing
// (an empty group has no GroupMembership) but still prints the others, so
// that failure is accepted when the output parses. Any other failure is
// an error, whatever was printed.
func (a darwinAccounts) read(ctx context.Context, record string, attrs ...string) (map[string][]string, bool, error) {
	args := append([]string{"-plist", ".", "-read", record}, attrs...)
	out, err := a.run(ctx, "dscl", args...)
	if err != nil {
		if strings.Contains(err.Error(), dsNotFoundMarker) {
			return nil, false, nil
		}
		if !missingAttr(err) {
			return nil, false, fmt.Errorf("reading %s: %w", record, err)
		}
	}
	parsed, perr := parseDSPlist(out)
	if perr != nil {
		return nil, false, fmt.Errorf("reading %s: %w", record, perr)
	}
	if len(parsed) == 0 {
		if err != nil {
			return nil, false, fmt.Errorf("reading %s: %w", record, err)
		}
		return nil, false, fmt.Errorf("reading %s: dscl printed no attributes", record)
	}
	return parsed, true, nil
}

// missingAttr reports whether err is dscl failing only because the record
// lacks an attribute. A command that did not run to an exit status, such
// as one killed on cancel, never counts.
func missingAttr(err error) bool {
	if exitCode(err) <= 0 {
		return false
	}
	for _, m := range dsMissingAttrMarkers {
		if strings.Contains(err.Error(), m) {
			return true
		}
	}
	return false
}

func (a darwinAccounts) CreateGroup(ctx context.Context, name string) (GroupInfo, error) {
	gid, err := a.freeID(ctx, "/Groups", "PrimaryGroupID")
	if err != nil {
		return GroupInfo{}, fmt.Errorf("creating group %s: %w", name, err)
	}
	id := strconv.FormatUint(uint64(gid), 10)
	if _, err := a.run(ctx, "dseditgroup", "-o", "create", "-i", id, "-r", name, name); err != nil {
		return GroupInfo{}, fmt.Errorf("creating group %s: %w", name, err)
	}
	return GroupInfo{Name: name, GID: gid}, nil
}

// CreateDaemonUser makes the daemon user with the deploy page's dscl calls.
// The home is always /var/empty on macOS, whatever home says.
func (a darwinAccounts) CreateDaemonUser(ctx context.Context, name, clients, _ string) (User, error) {
	g, ok, err := a.LookupGroup(ctx, clients)
	if err != nil {
		return User{}, fmt.Errorf("creating user %s: %w", name, err)
	}
	if !ok {
		return User{}, fmt.Errorf("creating user %s: group %s does not exist", name, clients)
	}
	uid, err := a.freeID(ctx, "/Users", "UniqueID")
	if err != nil {
		return User{}, fmt.Errorf("creating user %s: %w", name, err)
	}
	record := "/Users/" + name
	steps := [][]string{
		{record},
		{record, "UniqueID", strconv.FormatUint(uint64(uid), 10)},
		{record, "PrimaryGroupID", strconv.FormatUint(uint64(g.GID), 10)},
		{record, "UserShell", darwinDaemonShell},
		{record, "NFSHomeDirectory", darwinDaemonHome},
		{record, "IsHidden", "1"},
	}
	for _, s := range steps {
		if _, err := a.run(ctx, "dscl", append([]string{".", "-create"}, s...)...); err != nil {
			return User{}, fmt.Errorf("creating user %s: %w", name, err)
		}
	}
	return User{Name: name, UID: uid, GID: g.GID, Home: darwinDaemonHome, Shell: darwinDaemonShell}, nil
}

func (a darwinAccounts) AddMember(ctx context.Context, group, user string) error {
	if _, err := a.run(ctx, "dseditgroup", "-o", "edit", "-a", user, "-t", "user", group); err != nil {
		return fmt.Errorf("adding %s to %s: %w", user, group, err)
	}
	return nil
}

// freeID lists attr for every record under dir and picks the lowest free
// id in [300, 500).
func (a darwinAccounts) freeID(ctx context.Context, dir, attr string) (uint32, error) {
	out, err := a.run(ctx, "dscl", ".", "-list", dir, attr)
	if err != nil {
		return 0, fmt.Errorf("listing %s %s: %w", dir, attr, err)
	}
	used := map[uint32]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		// Negative ids such as nobody's -2 cannot collide with the range.
		if n, err := strconv.ParseUint(f[len(f)-1], 10, 32); err == nil {
			used[uint32(n)] = true
		}
	}
	id, ok := pickFreeID(used)
	if !ok {
		return 0, fmt.Errorf("no free id in [%d, %d) under %s", firstFreeID, endFreeID, dir)
	}
	return id, nil
}

// parseDSPlist reads the plist dscl -plist prints: a dict of attribute
// names to arrays of strings. Names lose their dsAttrTypeStandard: or
// other type prefix.
func parseDSPlist(data []byte) (map[string][]string, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	attrs := map[string][]string{}
	key := ""
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return attrs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parsing dscl output: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			var s string
			if err := d.DecodeElement(&s, &start); err != nil {
				return nil, fmt.Errorf("parsing dscl output: %w", err)
			}
			if i := strings.LastIndex(s, ":"); i >= 0 {
				s = s[i+1:]
			}
			key = s
			attrs[key] = nil
		case "string":
			var s string
			if err := d.DecodeElement(&s, &start); err != nil {
				return nil, fmt.Errorf("parsing dscl output: %w", err)
			}
			if key != "" {
				attrs[key] = append(attrs[key], s)
			}
		}
	}
}

// first returns the first value of attr, or "".
func first(attrs map[string][]string, attr string) string {
	if v := attrs[attr]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// firstID parses the first value of attr as an id.
func firstID(attrs map[string][]string, attr string) (uint32, error) {
	v := first(attrs, attr)
	if v == "" {
		return 0, fmt.Errorf("no %s", attr)
	}
	return parseID(v)
}
