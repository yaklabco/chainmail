package e2e

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

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
	if got, want := modeBits(st.Mode), modeBits(e.Mode); got != want {
		faults = append(faults, fmt.Sprintf("mode %s, want %s", got, want))
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

// specialBits are the mode bits beyond the permissions that a layout path
// must match too: a setuid, setgid or sticky bit bootstrap did not ask for
// is a fault.
const specialBits = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// modeBits is m's permission, setuid, setgid and sticky bits, without its
// type.
func modeBits(m fs.FileMode) fs.FileMode {
	return m & (fs.ModePerm | specialBits)
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

// installState is what a rerun of bootstrap on a working install must
// leave as it was.
type installState struct {
	StartedAt string               // the daemon's start time from admin status
	Paths     map[string]pathState // every layout entry, by path
	Sums      map[string]string    // the SHA-256 of every layout file, by path
}

// installChanges lists every difference from before to after, one line
// each, in path order after any start time change.
func installChanges(before, after installState) []string {
	var changes []string
	if before.StartedAt != after.StartedAt {
		changes = append(changes, fmt.Sprintf("started_at %s, then %s", before.StartedAt, after.StartedAt))
	}
	paths := slices.Concat(slices.Collect(maps.Keys(before.Paths)), slices.Collect(maps.Keys(after.Paths)),
		slices.Collect(maps.Keys(before.Sums)), slices.Collect(maps.Keys(after.Sums)))
	slices.Sort(paths)
	for _, path := range slices.Compact(paths) {
		changes = append(changes, pathChanges(path, before, after)...)
	}
	return changes
}

// pathChanges lists how path's type, mode, owner, group and contents
// differ from before to after.
func pathChanges(path string, before, after installState) []string {
	var changes []string
	b, hadB := before.Paths[path]
	a, hasA := after.Paths[path]
	switch {
	case hadB && !hasA:
		changes = append(changes, path+": gone")
	case !hadB && hasA:
		changes = append(changes, path+": new")
	case hadB:
		if b.Mode != a.Mode {
			changes = append(changes, fmt.Sprintf("%s: mode %s, then %s", path, b.Mode, a.Mode))
		}
		if b.UID != a.UID {
			changes = append(changes, fmt.Sprintf("%s: owner %d, then %d", path, b.UID, a.UID))
		}
		if b.GID != a.GID {
			changes = append(changes, fmt.Sprintf("%s: group %d, then %d", path, b.GID, a.GID))
		}
	}
	if bs, as := before.Sums[path], after.Sums[path]; bs != as {
		changes = append(changes, fmt.Sprintf("%s: SHA-256 %s, then %s", path, cmp.Or(bs, "none"), cmp.Or(as, "none")))
	}
	return changes
}

// bundleHolds reports whether a PEM bundle, such as the system's CA
// bundle, holds the certificate whose DER is der.
func bundleHolds(bundle, der []byte) bool {
	for {
		var block *pem.Block
		block, bundle = pem.Decode(bundle)
		if block == nil {
			return false
		}
		if block.Type == "CERTIFICATE" && bytes.Equal(block.Bytes, der) {
			return true
		}
	}
}

// keychainHasSHA1 reports whether `security find-certificate -a -Z`
// output lists a certificate with the SHA-1 hash sum, in hex.
func keychainHasSHA1(listing, sum string) bool {
	if sum == "" {
		return false
	}
	for line := range strings.Lines(listing) {
		if got, ok := strings.CutPrefix(strings.TrimSpace(line), "SHA-1 hash:"); ok && strings.EqualFold(strings.TrimSpace(got), sum) {
			return true
		}
	}
	return false
}

// trustListsName reports whether `security dump-trust-settings -d` output
// lists trust settings for a certificate called name.
func trustListsName(dump, name string) bool {
	if name == "" {
		return false
	}
	for line := range strings.Lines(dump) {
		label, cert, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if ok && strings.HasPrefix(label, "Cert ") && cert == name {
			return true
		}
	}
	return false
}

// deletable reports whether cleanup may delete dir with rm -rf: it must
// be absolute, clean and end in a directory called chainmail, so a wrong
// layout value can never remove a parent such as /usr/local/libexec.
func deletable(dir string) bool {
	return filepath.IsAbs(dir) && filepath.Clean(dir) == dir && filepath.Base(dir) == "chainmail"
}

// ancestors lists the directories above each absolute path, each once and
// without the root, deepest first, so removing them in order empties each
// before its parent. Relative paths are ignored.
func ancestors(paths ...string) []string {
	var dirs []string
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			continue
		}
		for d := filepath.Dir(filepath.Clean(p)); d != "/"; d = filepath.Dir(d) {
			dirs = append(dirs, d)
		}
	}
	slices.SortFunc(dirs, func(a, b string) int {
		return cmp.Or(cmp.Compare(depth(b), depth(a)), cmp.Compare(a, b))
	})
	return slices.Compact(dirs)
}

// depth counts the components of a clean absolute path.
func depth(p string) int { return strings.Count(p, "/") }

// waitDelay is how long runBounded waits for a killed command's pipes to
// close. A child that outlives the command, such as the security tool under
// a killed sudo, would otherwise keep exec.Cmd.Wait blocked forever.
const waitDelay = 5 * time.Second

// runBounded runs name with args and stdin, kills it after timeout, and
// returns its stdout and stderr. A non-zero exit or a kill is an error
// holding the stderr.
func runBounded(timeout time.Duration, stdin []byte, name string, args ...string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = waitDelay
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.Bytes(), errOut.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), errOut.Bytes(), nil
}
