package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yaklabco/chainmail/internal/setup"
)

// The two variables that must both be set before the test changes the host.
const (
	ciEnv     = "CI"
	mutateEnv = "CHAINMAIL_E2E_MUTATE_HOST"
)

// mutateHostBlocked returns why the test must not change this host, or ""
// when CI is "true" and CHAINMAIL_E2E_MUTATE_HOST is "1". Any other value,
// such as CI=1, blocks it, so only a deliberate CI job runs it.
func mutateHostBlocked(getenv func(string) string) string {
	if getenv(ciEnv) != "true" || getenv(mutateEnv) != "1" {
		return fmt.Sprintf("this test creates users, installs a service and changes the trust store; "+
			"it runs only with %s=true and %s=1, which only CI's e2e jobs set", ciEnv, mutateEnv)
	}
	return ""
}

// hostIDs are the ids the layout's owners and groups resolve to on this
// host. Root and the root group (root or wheel) are 0 on every platform.
type hostIDs struct {
	DaemonUID  uint32 // the daemon user
	DaemonGID  uint32 // the daemon user's primary group: chainmail on Linux, chainmail-clients on macOS
	ClientsGID uint32 // chainmail-clients
}

// owner resolves a layout owner to a uid.
func (h hostIDs) owner(o setup.Owner) (uint32, error) {
	switch o {
	case setup.OwnerRoot:
		return 0, nil
	case setup.OwnerDaemon:
		return h.DaemonUID, nil
	default:
		return 0, fmt.Errorf("unknown owner %q", o)
	}
}

// group resolves a layout group to a gid.
func (h hostIDs) group(g setup.Group) (uint32, error) {
	switch g {
	case setup.GroupRoot:
		return 0, nil
	case setup.GroupDaemon:
		return h.DaemonGID, nil
	case setup.GroupClients:
		return h.ClientsGID, nil
	default:
		return 0, fmt.Errorf("unknown group %q", g)
	}
}

// pathState is what checkEntry needs to know about a path, read without
// following a final symlink.
type pathState struct {
	Mode     fs.FileMode
	UID, GID uint32
}

// checkEntry compares a path's state with its layout entry and returns one
// error naming the path and every difference, or nil.
func checkEntry(e setup.Entry, st pathState, ids hostIDs) error {
	if st.Mode&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", e.Path)
	}
	var faults []string
	switch {
	case e.Kind == setup.KindDir && !st.Mode.IsDir():
		faults = append(faults, "is not a directory")
	case e.Kind == setup.KindFile && !st.Mode.IsRegular():
		faults = append(faults, "is not a regular file")
	}
	if st.Mode.Perm() != e.Mode.Perm() {
		faults = append(faults, fmt.Sprintf("mode %s, want %s", st.Mode.Perm(), e.Mode.Perm()))
	}
	uid, err := ids.owner(e.Owner)
	if err != nil {
		return fmt.Errorf("%s: %w", e.Path, err)
	}
	if st.UID != uid {
		faults = append(faults, fmt.Sprintf("owner %d, want %d", st.UID, uid))
	}
	gid, err := ids.group(e.Group)
	if err != nil {
		return fmt.Errorf("%s: %w", e.Path, err)
	}
	if st.GID != gid {
		faults = append(faults, fmt.Sprintf("group %d, want %d", st.GID, gid))
	}
	if len(faults) == 0 {
		return nil
	}
	return fmt.Errorf("%s (%s): %s", e.Path, e.Name, strings.Join(faults, "; "))
}

// decodeResult reads the one setup.Result object bootstrap --output json
// prints on stdout, refusing anything else.
func decodeResult(stdout []byte) (setup.Result, error) {
	dec := json.NewDecoder(bytes.NewReader(stdout))
	var res setup.Result
	if err := dec.Decode(&res); err != nil {
		return setup.Result{}, fmt.Errorf("bootstrap stdout is not a result object: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return setup.Result{}, errors.New("bootstrap stdout holds more than one JSON value")
	}
	return res, nil
}

// startedAt reads started_at from admin status --output json.
func startedAt(statusJSON []byte) (string, error) {
	var st struct {
		StartedAt *string `json:"started_at"`
	}
	if err := json.Unmarshal(statusJSON, &st); err != nil {
		return "", fmt.Errorf("admin status output is not JSON: %w", err)
	}
	if st.StartedAt == nil || *st.StartedAt == "" {
		return "", errors.New("admin status output has no started_at")
	}
	return *st.StartedAt, nil
}

// lowestFreeID returns the lowest id in [lo, hi) that no line of listing
// uses. listing is `dscl . -list /Users UniqueID` output: a name and an id
// per line.
func lowestFreeID(listing string, lo, hi int) (int, error) {
	used := map[int]bool{}
	for line := range strings.Lines(listing) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		id, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			return 0, fmt.Errorf("id listing line %q: %w", strings.TrimSpace(line), err)
		}
		used[id] = true
	}
	for id := lo; id < hi; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free id in [%d, %d)", lo, hi)
}

// shellCommand quotes each argument for /bin/sh and joins them, for sg -c.
func shellCommand(args ...string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// cliConfigPath is where bootstrap writes the cli.toml of the user whose
// environment getenv reads, or "" when it has no place for one.
func cliConfigPath(getenv func(string) string) string {
	if x := getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "chainmail", "cli.toml")
	}
	if h := getenv("HOME"); filepath.IsAbs(h) {
		return filepath.Join(h, ".config", "chainmail", "cli.toml")
	}
	return ""
}
