//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1" // security delete-certificate -Z names a certificate by its SHA-1 hash
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/setup"
	"github.com/yaklabco/chainmail/internal/testutil/imaptest"
	"github.com/yaklabco/chainmail/internal/testutil/smtptest"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

const (
	// e2eAddress is the account bootstrap sets up. Its name is "e2e".
	e2eAddress = "e2e@example.org"
	e2eAccount = "e2e"
	// otherUser is a user outside chainmail-clients, which the socket
	// must refuse.
	otherUser = "chainmail-e2e-other"
	// otherUIDs is where the macOS other user's uid is picked from, clear
	// of the daemon user's [300, 500).
	otherUIDLo, otherUIDHi = 700, 800
	// staffGID is the primary group of an ordinary macOS user.
	staffGID = "20"

	// linuxCAFile is where update-ca-certificates picks up the test CA.
	linuxCAFile = "/usr/local/share/ca-certificates/chainmail-e2e.crt"
	// systemKeychain holds the macOS test CA and its trust setting.
	systemKeychain = "/Library/Keychains/System.keychain"

	// cmdTimeout bounds every command but the build.
	cmdTimeout = 3 * time.Minute
	// buildTimeout bounds go build, which may start with a cold cache.
	buildTimeout = 10 * time.Minute
)

// TestBootstrapEndToEnd runs chainmail bootstrap for real on a CI runner
// and checks what it built, then runs it again and checks nothing
// restarted. See the package comment for why it skips everywhere else.
func TestBootstrapEndToEnd(t *testing.T) {
	if reason := mutateHostBlocked(os.Getenv); reason != "" {
		t.Skip(reason)
	}
	p := setup.Platform(runtime.GOOS)
	if p != setup.Linux && p != setup.Darwin {
		t.Skipf("bootstrap does not support %s", runtime.GOOS)
	}
	if os.Getuid() == 0 {
		t.Fatal("run this test as an ordinary user with passwordless sudo; bootstrap refuses root")
	}
	h := newHost(t, setup.LayoutFor(p))
	h.requireSudo()
	h.requireClean()
	// Registered before the first change, so a failure part way still
	// leaves the runner clean. Cleanups run last first: the log dump
	// below runs before this.
	t.Cleanup(h.cleanup)

	password := randomPassword(t)
	ca := tlstest.New(t)
	h.trustCA(ca)
	imapEP := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{Username: e2eAddress, Password: password})
	smtpEP := smtptest.Start(t, ca, provider.SecurityTLS, smtptest.User{Username: e2eAddress, Password: password})
	bin := buildChainmail(t)
	t.Cleanup(h.dumpLogsOnFailure)

	env := bootstrapEnv(imapEP, smtpEP)
	res := h.bootstrap(bin, env, password)
	if res.Test != "ok" || !res.ServiceRunning || res.Account != e2eAccount || res.Provider != "hostinger" {
		t.Fatalf("first bootstrap result = %+v; want account %s, provider hostinger, service running, test ok", res, e2eAccount)
	}

	ids := h.checkAccounts()
	h.checkLayout(ids)
	h.checkKeyHidden()
	h.checkOtherUserRefused()
	h.checkRunnerReaches(bin)
	if p == setup.Darwin {
		h.checkCLIConfig()
	}

	before := h.startedAt(bin)
	config := h.readFile(h.l.Config)
	service := h.readFile(h.l.Service)

	// On Linux the test process's groups predate the membership bootstrap
	// added, so the rerun cannot ask the daemon about the account and asks
	// for the password again. macOS may resolve the new membership at once
	// and skip the password. Either way the rerun tests the account,
	// changes neither the config nor the service file, and restarts
	// nothing.
	res = h.bootstrap(bin, env, password)
	if res.Test != "ok" || !res.ServiceRunning {
		t.Errorf("second bootstrap result = %+v; want service running, test ok", res)
	}
	if after := h.startedAt(bin); after != before {
		t.Errorf("daemon started_at changed across the second run: %s, then %s; the rerun restarted it", before, after)
	}
	if got := h.readFile(h.l.Config); !bytes.Equal(got, config) {
		t.Errorf("the second run changed %s:\nbefore:\n%s\nafter:\n%s", h.l.Config, config, got)
	}
	if got := h.readFile(h.l.Service); !bytes.Equal(got, service) {
		t.Errorf("the second run changed %s", h.l.Service)
	}
}

// host runs commands on the CI runner and knows the layout bootstrap uses.
type host struct {
	t      *testing.T
	l      setup.Layout
	runner string // the user running the test
	// cliConfig is the runner's cli.toml, and cliConfigExisted says it was
	// there before the test, so cleanup leaves it alone.
	cliConfig        string
	cliConfigExisted bool
	caPEM            []byte
}

func newHost(t *testing.T, l setup.Layout) *host {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatalf("look up the runner user: %v", err)
	}
	h := &host{t: t, l: l, runner: u.Username, cliConfig: cliConfigPath(os.Getenv)}
	if h.cliConfig != "" {
		_, err := os.Lstat(h.cliConfig)
		h.cliConfigExisted = err == nil
	}
	return h
}

// run runs name with args, with stdin as its input, and returns its
// stdout and stderr. A non-zero exit is an error holding the stderr.
func (h *host) run(stdin []byte, name string, args ...string) (stdout, stderr []byte, err error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	if err != nil {
		err = fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), errOut.Bytes(), err
}

// sudo runs a command as root without asking for a password.
func (h *host) sudo(args ...string) ([]byte, error) {
	h.t.Helper()
	out, _, err := h.run(nil, "sudo", append([]string{"-n"}, args...)...)
	return out, err
}

// mustSudo is sudo that fails the test on error.
func (h *host) mustSudo(args ...string) []byte {
	h.t.Helper()
	out, err := h.sudo(args...)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// requireSudo fails unless sudo works without a password, as it does on
// GitHub's runners.
func (h *host) requireSudo() {
	h.t.Helper()
	if _, err := h.sudo("true"); err != nil {
		h.t.Fatalf("this test needs passwordless sudo: %v", err)
	}
}

// requireClean fails before any change when the runner already has a
// chainmail install, users or groups, so the test never touches one it
// did not make.
func (h *host) requireClean() {
	h.t.Helper()
	for _, name := range []string{h.l.DaemonUser, otherUser} {
		if _, err := user.Lookup(name); err == nil {
			h.t.Fatalf("user %s already exists; this test needs a runner without chainmail", name)
		}
	}
	if _, err := user.LookupGroup(h.l.ClientGroup); err == nil {
		h.t.Fatalf("group %s already exists; this test needs a runner without chainmail", h.l.ClientGroup)
	}
	for _, path := range []string{h.l.ConfigDir, h.l.StateDir, h.l.Service, h.l.Binary} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			h.t.Fatalf("%s already exists (%v); this test needs a runner without chainmail", path, err)
		}
	}
}

// trustCA adds the test CA to the system trust store, so the daemon,
// which sets no environment, trusts the test servers.
func (h *host) trustCA(ca *tlstest.CA) {
	h.t.Helper()
	cert, _, err := ca.PEM()
	if err != nil {
		h.t.Fatal(err)
	}
	h.caPEM = cert
	file := filepath.Join(h.t.TempDir(), "chainmail-e2e-ca.pem")
	if err := os.WriteFile(file, cert, 0o644); err != nil {
		h.t.Fatal(err)
	}
	switch h.l.Platform {
	case setup.Linux:
		h.mustSudo("install", "-m", "0644", "-o", "root", "-g", "root", file, linuxCAFile)
		h.mustSudo("update-ca-certificates")
	case setup.Darwin:
		h.mustSudo("security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", systemKeychain, file)
	}
}

// untrustCA removes the test CA from the system trust store.
func (h *host) untrustCA() {
	h.t.Helper()
	switch h.l.Platform {
	case setup.Linux:
		h.cleanupStep("rm", "-f", linuxCAFile)
		h.cleanupStep("update-ca-certificates", "--fresh")
	case setup.Darwin:
		if h.caPEM == nil {
			return
		}
		// This runs in a cleanup, where t.TempDir may not be called.
		file, err := writeTemp(h.caPEM)
		if err != nil {
			h.t.Errorf("cleanup: %v", err)
			return
		}
		defer func() { _ = os.Remove(file) }() // a leftover temp file harms nothing
		h.cleanupStep("security", "remove-trusted-cert", "-d", file)
		sum, err := certSHA1(h.caPEM)
		if err != nil {
			h.t.Errorf("cleanup: %v", err)
			return
		}
		h.cleanupStep("security", "delete-certificate", "-Z", sum, systemKeychain)
	}
}

// writeTemp writes data to a new temp file readable by root and returns
// its path.
func writeTemp(data []byte) (string, error) {
	f, err := os.CreateTemp("", "chainmail-e2e-ca-*.pem")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close() // the write error is the one to report
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// certSHA1 is the SHA-1 hash of a PEM certificate's DER, in the form
// security delete-certificate -Z takes.
func certSHA1(pemCert []byte) (string, error) {
	block, _ := pem.Decode(pemCert)
	if block == nil {
		return "", errors.New("the test CA is not PEM")
	}
	sum := sha1.Sum(block.Bytes)
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

// buildChainmail builds chainmail with the e2e tag, the only build that
// takes CHAINMAIL_E2E_IMAP and CHAINMAIL_E2E_SMTP.
func buildChainmail(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "chainmail")
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-tags", "e2e", "-trimpath", "-o", bin,
		"github.com/yaklabco/chainmail/cmd/chainmail")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build -tags e2e: %v\n%s", err, out)
	}
	return bin
}

// bootstrapEnv is the runner's environment with no CHAINMAIL_ settings of
// its own, plus the test server endpoints.
func bootstrapEnv(imapEP, smtpEP provider.Endpoint) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "CHAINMAIL_") })
	return append(env,
		"CHAINMAIL_E2E_IMAP="+net.JoinHostPort(imapEP.Host, strconv.Itoa(imapEP.Port)),
		"CHAINMAIL_E2E_SMTP="+net.JoinHostPort(smtpEP.Host, strconv.Itoa(smtpEP.Port)),
	)
}

// bootstrap runs bin bootstrap as the runner, with the password on stdin,
// and returns its result. Anything but exit 0 fails the test.
func (h *host) bootstrap(bin string, env []string, password string) setup.Result {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "bootstrap", e2eAddress, "--provider", "hostinger", "--yes", "--output", "json")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(password + "\n")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	h.t.Logf("chainmail bootstrap stderr:\n%s", errOut.String())
	if err != nil {
		h.t.Fatalf("chainmail bootstrap: %v\nstdout:\n%s", err, out.String())
	}
	res, err := decodeResult(out.Bytes())
	if err != nil {
		h.t.Fatalf("%v\nstdout:\n%s", err, out.String())
	}
	return res
}

// checkAccounts checks the daemon user and the client group have the
// shape bootstrap gives them, and returns their ids.
func (h *host) checkAccounts() hostIDs {
	h.t.Helper()
	if _, _, err := h.run(nil, "id", h.l.DaemonUser); err != nil {
		h.t.Fatalf("the daemon user: %v", err)
	}
	u, err := user.Lookup(h.l.DaemonUser)
	if err != nil {
		h.t.Fatalf("look up %s: %v", h.l.DaemonUser, err)
	}
	clients, err := user.LookupGroup(h.l.ClientGroup)
	if err != nil {
		h.t.Fatalf("look up group %s: %v", h.l.ClientGroup, err)
	}
	ids := hostIDs{DaemonUID: parseID(h.t, u.Uid), DaemonGID: parseID(h.t, u.Gid), ClientsGID: parseID(h.t, clients.Gid)}

	switch h.l.Platform {
	case setup.Linux:
		primary, err := user.LookupGroupId(u.Gid)
		if err != nil || primary.Name != h.l.DaemonUser {
			h.t.Errorf("%s's primary group is %+v (%v), want %s", h.l.DaemonUser, primary, err, h.l.DaemonUser)
		}
		groups, err := u.GroupIds()
		if err != nil || !slices.Contains(groups, clients.Gid) {
			h.t.Errorf("%s's groups are %v (%v), want them to hold %s (%s)", h.l.DaemonUser, groups, err, h.l.ClientGroup, clients.Gid)
		}
	case setup.Darwin:
		if u.Gid != clients.Gid {
			h.t.Errorf("%s's primary group is %s, want %s (%s)", h.l.DaemonUser, u.Gid, h.l.ClientGroup, clients.Gid)
		}
		if uid := int(ids.DaemonUID); uid < 300 || uid >= 500 {
			h.t.Errorf("%s has uid %d, want one in [300, 500)", h.l.DaemonUser, uid)
		}
	}
	if shell, want := h.userShell(h.l.DaemonUser), daemonShell(h.l.Platform); shell != want {
		h.t.Errorf("%s's shell is %q, want %q", h.l.DaemonUser, shell, want)
	}

	out, _, err := h.run(nil, "id", "-Gn", h.runner)
	if err != nil {
		h.t.Fatal(err)
	}
	if !slices.Contains(strings.Fields(string(out)), h.l.ClientGroup) {
		h.t.Errorf("runner %s is in %q, not in %s", h.runner, strings.TrimSpace(string(out)), h.l.ClientGroup)
	}
	return ids
}

// daemonShell is the login shell bootstrap gives the daemon user.
func daemonShell(p setup.Platform) string {
	if p == setup.Darwin {
		return "/usr/bin/false"
	}
	return "/usr/sbin/nologin"
}

// userShell reads name's login shell from the user database.
func (h *host) userShell(name string) string {
	h.t.Helper()
	if h.l.Platform == setup.Darwin {
		out, _, err := h.run(nil, "dscl", ".", "-read", "/Users/"+name, "UserShell")
		if err != nil {
			h.t.Fatal(err)
		}
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "UserShell:"))
	}
	out, _, err := h.run(nil, "getent", "passwd", name)
	if err != nil {
		h.t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	return fields[len(fields)-1]
}

// parseID parses a numeric user or group id.
func parseID(t *testing.T, s string) uint32 {
	t.Helper()
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		t.Fatalf("id %q: %v", s, err)
	}
	return uint32(n)
}

// checkLayout checks every path the layout lists, and the socket, has its
// owner and mode.
func (h *host) checkLayout(ids hostIDs) {
	h.t.Helper()
	for _, e := range h.l.Entries() {
		st, err := statPath(e.Path)
		if err != nil {
			h.t.Errorf("%s (%s): %v", e.Path, e.Name, err)
			continue
		}
		if err := checkEntry(e, st, ids); err != nil {
			h.t.Error(err)
		}
	}
	st, err := statPath(h.l.Socket)
	if err != nil {
		h.t.Errorf("the socket: %v", err)
		return
	}
	if st.Mode.Type() != fs.ModeSocket || st.Mode.Perm() != 0o660 || st.UID != ids.DaemonUID || st.GID != ids.ClientsGID {
		h.t.Errorf("socket %s is %s owned by %d:%d, want a socket with mode 0660 owned by %d:%d",
			h.l.Socket, st.Mode, st.UID, st.GID, ids.DaemonUID, ids.ClientsGID)
	}
}

// statPath reads path's type, mode and owner without following a final
// symlink.
func statPath(path string) (pathState, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return pathState{}, err
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return pathState{}, fmt.Errorf("%s: no owner in %T", path, fi.Sys())
	}
	return pathState{Mode: fi.Mode(), UID: sys.Uid, GID: sys.Gid}, nil
}

// checkKeyHidden checks the runner cannot read the vault key, or even see
// the vault behind the state directory.
func (h *host) checkKeyHidden() {
	h.t.Helper()
	if data, err := os.ReadFile(h.l.VaultKey); !errors.Is(err, fs.ErrPermission) {
		clear(data)
		h.t.Errorf("reading %s as %s: %v, want a permission error", h.l.VaultKey, h.runner, err)
	}
	if _, err := os.Lstat(h.l.Vault); !errors.Is(err, fs.ErrPermission) {
		h.t.Errorf("stat %s as %s: %v, want a permission error", h.l.Vault, h.runner, err)
	}
}

// checkOtherUserRefused makes a user outside chainmail-clients and checks
// the socket refuses it. It runs the installed binary: the test's temp
// directory is private to the runner.
func (h *host) checkOtherUserRefused() {
	h.t.Helper()
	h.createOtherUser()
	// env -i keeps the runner's HOME, and so its cli.toml, out of the
	// other user's run, so only the socket can refuse it.
	_, stderr, err := h.run(nil, "sudo", "-n", "-u", otherUser, "env", "-i", "HOME=/var/empty", "PATH=/usr/bin:/bin",
		h.l.Binary, "admin", "status", "--socket", h.l.Socket, "--output", "json")
	if err == nil {
		h.t.Fatalf("%s, who is not in %s, reached the daemon", otherUser, h.l.ClientGroup)
	}
	var out struct {
		Error struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if jerr := json.Unmarshal(bytes.TrimSpace(stderr), &out); jerr != nil {
		h.t.Fatalf("admin status as %s: %v; stderr is not one error object: %v", otherUser, err, jerr)
	}
	if !strings.Contains(strings.ToLower(out.Error.Message), "permission denied") && out.Error.Reason != "FORBIDDEN" {
		h.t.Errorf("admin status as %s failed with %+v, want permission denied at the socket", otherUser, out.Error)
	}
}

// createOtherUser makes otherUser, with no membership in the client group.
func (h *host) createOtherUser() {
	h.t.Helper()
	if h.l.Platform == setup.Linux {
		h.mustSudo("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", otherUser)
		return
	}
	out, _, err := h.run(nil, "dscl", ".", "-list", "/Users", "UniqueID")
	if err != nil {
		h.t.Fatal(err)
	}
	uid, err := lowestFreeID(string(out), otherUIDLo, otherUIDHi)
	if err != nil {
		h.t.Fatal(err)
	}
	record := "/Users/" + otherUser
	h.mustSudo("dscl", ".", "-create", record)
	for _, kv := range [][2]string{
		{"UniqueID", strconv.Itoa(uid)},
		{"PrimaryGroupID", staffGID},
		{"UserShell", "/usr/bin/false"},
		{"NFSHomeDirectory", "/var/empty"},
		{"IsHidden", "1"},
	} {
		h.mustSudo("dscl", ".", "-create", record, kv[0], kv[1])
	}
	h.mustSudo("dscacheutil", "-flushcache")
}

// asRunnerWithGroups runs bin with args as the runner in a session that
// has the membership bootstrap added: sg on Linux, a fresh login through
// sudo -i on macOS.
func (h *host) asRunnerWithGroups(bin string, args ...string) ([]byte, error) {
	h.t.Helper()
	var out []byte
	var err error
	if h.l.Platform == setup.Linux {
		out, _, err = h.run(nil, "sg", h.l.ClientGroup, "-c", shellCommand(append([]string{bin}, args...)...))
	} else {
		out, _, err = h.run(nil, "sudo", append([]string{"-n", "-u", h.runner, "-i", bin}, args...)...)
	}
	return out, err
}

// checkRunnerReaches checks the runner reaches the daemon once its new
// group membership applies, and that the account is there.
func (h *host) checkRunnerReaches(bin string) {
	h.t.Helper()
	out, err := h.asRunnerWithGroups(bin, "accounts", "list", "--socket", h.l.Socket, "--output", "json")
	if err != nil {
		h.t.Fatalf("accounts list as %s with group %s: %v", h.runner, h.l.ClientGroup, err)
	}
	var list struct {
		Accounts []struct {
			Name string `json:"name"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		h.t.Fatalf("accounts list output %q: %v", out, err)
	}
	found := false
	for _, a := range list.Accounts {
		found = found || a.Name == e2eAccount
	}
	if !found {
		h.t.Errorf("accounts list = %s, want account %s", out, e2eAccount)
	}
}

// checkCLIConfig checks bootstrap told the runner's commands where the
// macOS socket is.
func (h *host) checkCLIConfig() {
	h.t.Helper()
	if h.cliConfigExisted {
		h.t.Logf("%s existed before the test, so bootstrap left it alone", h.cliConfig)
		return
	}
	data, err := os.ReadFile(h.cliConfig)
	if err != nil {
		h.t.Errorf("bootstrap did not write cli.toml: %v", err)
		return
	}
	if want := "socket = " + strconv.Quote(h.l.Socket) + "\n"; string(data) != want {
		h.t.Errorf("%s = %q, want %q", h.cliConfig, data, want)
	}
}

// startedAt is the daemon's start time from admin status.
func (h *host) startedAt(bin string) string {
	h.t.Helper()
	out, err := h.asRunnerWithGroups(bin, "admin", "status", "--socket", h.l.Socket, "--output", "json")
	if err != nil {
		h.t.Fatalf("admin status: %v", err)
	}
	at, err := startedAt(out)
	if err != nil {
		h.t.Fatal(err)
	}
	return at
}

// readFile reads a file the runner may read.
func (h *host) readFile(path string) []byte {
	h.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	return data
}

// dumpLogsOnFailure logs the end of the daemon's log when the test failed.
func (h *host) dumpLogsOnFailure() {
	if !h.t.Failed() {
		return
	}
	var out []byte
	var err error
	if h.l.Platform == setup.Linux {
		out, err = h.sudo("journalctl", "-u", h.l.ServiceLabel, "-n", "100", "--no-pager")
	} else {
		out, err = h.sudo("tail", "-n", "100", h.l.LogFile)
	}
	h.t.Logf("daemon log (%v):\n%s", err, out)
}

// cleanupStep runs one cleanup command as root. A step that fails is
// logged and the next one runs: after a failed bootstrap some of what
// they remove was never made.
func (h *host) cleanupStep(args ...string) {
	h.t.Helper()
	if _, err := h.sudo(args...); err != nil {
		h.t.Logf("cleanup: %v", err)
	}
}

// removable lists the directories cleanup deletes. Each is a layout path
// that bootstrap creates whole; parents such as /usr/local/libexec stay.
func (h *host) removable() []string {
	dirs := []string{h.l.ConfigDir, h.l.StateDir, h.l.SocketDir}
	switch h.l.Platform {
	case setup.Linux:
		dirs = append(dirs, h.l.BinaryDir)
	case setup.Darwin:
		// BinaryDir is .../chainmail/bin; bootstrap made .../chainmail too.
		dirs = append(dirs, filepath.Dir(h.l.BinaryDir), h.l.LogDir)
	}
	return dirs
}

// cleanup stops and removes the service, the users, the group, every
// path bootstrap made and the test CA, then checks they are gone, so a
// rerun on the same runner starts clean.
func (h *host) cleanup() {
	h.t.Helper()
	switch h.l.Platform {
	case setup.Linux:
		h.cleanupStep("systemctl", "disable", "--now", h.l.ServiceLabel)
		h.cleanupStep("rm", "-f", h.l.Service)
		h.cleanupStep("systemctl", "daemon-reload")
		h.cleanupStep("systemctl", "reset-failed", h.l.ServiceLabel)
		h.cleanupStep("userdel", otherUser)
		h.cleanupStep("userdel", h.l.DaemonUser)
		if _, err := user.LookupGroup(h.l.DaemonUser); err == nil {
			h.cleanupStep("groupdel", h.l.DaemonUser)
		}
		h.cleanupStep("groupdel", h.l.ClientGroup)
	case setup.Darwin:
		h.cleanupStep("launchctl", "bootout", "system/"+h.l.ServiceLabel)
		h.cleanupStep("rm", "-f", h.l.Service)
		h.cleanupStep("dscl", ".", "-delete", "/Users/"+otherUser)
		h.cleanupStep("dscl", ".", "-delete", "/Users/"+h.l.DaemonUser)
		h.cleanupStep("dscl", ".", "-delete", "/Groups/"+h.l.ClientGroup)
		h.cleanupStep("dscacheutil", "-flushcache")
		if h.cliConfig != "" && !h.cliConfigExisted {
			if err := os.Remove(h.cliConfig); err != nil && !errors.Is(err, fs.ErrNotExist) {
				h.t.Logf("cleanup: %v", err)
			}
		}
	}
	for _, dir := range h.removable() {
		if !filepath.IsAbs(dir) || !strings.Contains(dir, "chainmail") {
			h.t.Errorf("cleanup: refusing to delete %q", dir)
			continue
		}
		h.cleanupStep("rm", "-rf", dir)
	}
	h.untrustCA()

	for _, name := range []string{h.l.DaemonUser, otherUser} {
		if _, err := user.Lookup(name); err == nil {
			h.t.Errorf("cleanup left user %s", name)
		}
	}
	if _, err := user.LookupGroup(h.l.ClientGroup); err == nil {
		h.t.Errorf("cleanup left group %s", h.l.ClientGroup)
	}
	for _, e := range h.l.Entries() {
		if _, err := os.Lstat(e.Path); !errors.Is(err, fs.ErrNotExist) {
			h.t.Errorf("cleanup left %s (%v)", e.Path, err)
		}
	}
}

// randomPassword is a fresh password for the test servers.
func randomPassword(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
