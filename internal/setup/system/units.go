package system

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// UnitInfo is what systemd reports about a unit.
type UnitInfo struct {
	Loaded    bool   // LoadState is "loaded"
	LoadState string // LoadState as systemd prints it, such as "loaded" or "bad-setting"
	Fragment  string // FragmentPath with every symlink resolved, "" when none
	ExecPath  string // path= of the first ExecStart entry, "" when none
	Active    bool   // ActiveState is "active"
}

// Units reads unit facts from systemd. It never changes a unit.
type Units interface {
	Show(ctx context.Context, unit string) (UnitInfo, error)
}

// NewUnits runs systemctl show through run:
//
//	systemctl show <unit> -p LoadState -p FragmentPath -p ExecStart -p ActiveState
//
// and resolves FragmentPath with filepath.EvalSymlinks. A FragmentPath that
// does not resolve gives Fragment "".
func NewUnits(run Runner) Units { return systemdUnits{run: run} }

type systemdUnits struct{ run Runner }

// Show implements Units. systemctl prints the properties in its own order,
// one key=value per line, so they are read by key.
func (u systemdUnits) Show(ctx context.Context, unit string) (UnitInfo, error) {
	out, err := u.run(ctx, "systemctl", "show", unit,
		"-p", "LoadState", "-p", "FragmentPath", "-p", "ExecStart", "-p", "ActiveState")
	if err != nil {
		return UnitInfo{}, fmt.Errorf("reading unit %s: %w", unit, err)
	}
	props := map[string]string{}
	for line := range strings.Lines(string(out)) {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\n"), "=")
		if !ok {
			continue
		}
		// A unit with several ExecStart commands prints one line each; the
		// first is the one that counts.
		if _, seen := props[key]; !seen {
			props[key] = value
		}
	}
	return UnitInfo{
		Loaded:    props["LoadState"] == "loaded",
		LoadState: props["LoadState"],
		Fragment:  resolveFragment(props["FragmentPath"]),
		ExecPath:  execStartPath(props["ExecStart"]),
		Active:    props["ActiveState"] == "active",
	}, nil
}

// resolveFragment returns path with every symlink resolved, or "" when
// path is empty or does not resolve.
func resolveFragment(path string) string {
	if path == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return resolved
}

// execStartPath returns the path= field of one ExecStart entry, as
// systemctl show prints it:
//
//	{ path=/nix/store/x-chainmail/bin/chainmail ; argv[]=... ; ... }
//
// Anything else gives "".
func execStartPath(entry string) string {
	rest, ok := strings.CutPrefix(entry, "{ path=")
	if !ok {
		return ""
	}
	path, _, ok := strings.Cut(rest, " ;")
	if !ok {
		return ""
	}
	return path
}
