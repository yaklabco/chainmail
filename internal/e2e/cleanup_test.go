//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha1" // security delete-certificate -Z names a certificate by its SHA-1 hash
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/setup"
)

const (
	// linuxCABundle is the bundle update-ca-certificates writes, which Go
	// reads on Debian and Ubuntu.
	linuxCABundle = "/etc/ssl/certs/ca-certificates.crt"
	// bootoutWait bounds how long cleanup waits for launchd to drop the job.
	bootoutWait = 30 * time.Second
	// launchdNotFound is launchctl print's exit status for a job it does
	// not know.
	launchdNotFound = 113
)

// made records what the test has changed on the host, so cleanup undoes
// that and nothing else. Each flag is set before its change starts, so a
// change that fails part way is undone too.
type made struct {
	ca        bool // the test CA in the system trust store
	otherUser bool // otherUser
	install   bool // bootstrap ran: its users, group, paths and service, and cli.toml
}

// recordMissing notes the directories above the paths bootstrap and the
// test create that do not exist yet, such as /private/var/lib on macOS,
// so cleanup removes them too. It runs before the first change.
func (h *host) recordMissing() {
	h.t.Helper()
	paths := append(h.removable(), h.l.Service)
	if h.cliConfig != "" && !h.cliConfigExisted {
		paths = append(paths, h.cliConfig)
	}
	for _, dir := range ancestors(paths...) {
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			h.missing = append(h.missing, dir)
		}
	}
}

// removable lists the directories cleanup deletes whole. Each is a layout
// directory bootstrap creates; parents bootstrap did not create stay.
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

// cleanup undoes what the test made, then checks the host is as
// requireClean found it. A step that fails is a test error, and the
// remaining steps still run.
func (h *host) cleanup() {
	h.t.Helper()
	if h.made.install {
		h.removeService()
	}
	if h.made.otherUser {
		h.removeUser(otherUser)
	}
	if h.made.install {
		h.removeAccounts()
		h.removePaths()
		h.removeCLIConfig()
	}
	if h.made.ca {
		h.untrustCA()
	}

	h.checkServiceGone()
	h.checkAccountsGone()
	h.checkPathsGone()
	h.checkCAGone()
}

// cleanupStep runs one cleanup command as root and reports a failure as a
// test error. Every step runs only on something that is there, so any
// failure is unexpected.
func (h *host) cleanupStep(args ...string) {
	h.t.Helper()
	if _, err := h.sudo(args...); err != nil {
		h.t.Errorf("cleanup: %v", err)
	}
}

// exists reports whether path exists, without following a final symlink.
func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// removeService stops the service and removes its definition.
func (h *host) removeService() {
	h.t.Helper()
	switch h.l.Platform {
	case setup.Linux:
		load, active, err := h.unitState()
		switch {
		case err != nil:
			h.t.Errorf("cleanup: %v", err)
		case load != "not-found":
			h.cleanupStep("systemctl", "disable", "--now", h.l.ServiceLabel)
		case active != "inactive" && active != "failed":
			h.cleanupStep("systemctl", "stop", h.l.ServiceLabel)
		}
		h.removeFile(h.l.Service)
		h.cleanupStep("systemctl", "daemon-reload")
		if _, active, err := h.unitState(); err == nil && active == "failed" {
			h.cleanupStep("systemctl", "reset-failed", h.l.ServiceLabel)
		}
	case setup.Darwin:
		loaded, err := h.jobLoaded()
		if err != nil {
			h.t.Errorf("cleanup: %v", err)
		}
		if loaded {
			h.cleanupStep("launchctl", "bootout", "system/"+h.l.ServiceLabel)
			h.waitJobGone()
		}
		h.removeFile(h.l.Service)
	}
}

// removeFile deletes one root-owned file if it is there.
func (h *host) removeFile(path string) {
	h.t.Helper()
	there, err := exists(path)
	if err != nil {
		h.t.Errorf("cleanup: %v", err)
	}
	if there {
		h.cleanupStep("rm", "-f", path)
	}
}

// unitState is the service's systemd load and active states, such as
// "not-found" and "inactive" for a unit systemd no longer knows.
func (h *host) unitState() (load, active string, err error) {
	h.t.Helper()
	prop := func(name string) (string, error) {
		out, _, err := h.run(nil, "systemctl", "show", "--property="+name, "--value", h.l.ServiceLabel)
		return strings.TrimSpace(string(out)), err
	}
	if load, err = prop("LoadState"); err != nil {
		return "", "", err
	}
	if active, err = prop("ActiveState"); err != nil {
		return "", "", err
	}
	return load, active, nil
}

// jobLoaded reports whether launchd knows the service's job.
func (h *host) jobLoaded() (bool, error) {
	h.t.Helper()
	_, stderr, err := h.run(nil, "sudo", "-n", "launchctl", "print", "system/"+h.l.ServiceLabel)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && (exit.ExitCode() == launchdNotFound || bytes.Contains(stderr, []byte("Could not find service"))) {
		return false, nil
	}
	return false, err
}

// waitJobGone waits for launchd to drop the job after bootout.
func (h *host) waitJobGone() {
	h.t.Helper()
	deadline := time.Now().Add(bootoutWait)
	for {
		loaded, err := h.jobLoaded()
		if err == nil && !loaded {
			return
		}
		if time.Now().After(deadline) {
			h.t.Errorf("cleanup: launchd still has %s %s after bootout (%v)", h.l.ServiceLabel, bootoutWait, err)
			return
		}
		time.Sleep(time.Second)
	}
}

// removeAccounts deletes the daemon user and the groups bootstrap made.
func (h *host) removeAccounts() {
	h.t.Helper()
	h.removeUser(h.l.DaemonUser)
	groups := []string{h.l.ClientGroup}
	if h.l.Platform == setup.Linux {
		// userdel usually removes the daemon user's own group, but not
		// when the group was made apart from the user.
		groups = append([]string{h.l.DaemonUser}, groups...)
	}
	for _, g := range groups {
		there, err := groupExists(g)
		if err != nil {
			h.t.Errorf("cleanup: %v", err)
		}
		if !there {
			continue
		}
		if h.l.Platform == setup.Linux {
			h.cleanupStep("groupdel", g)
		} else {
			h.cleanupStep("dscl", ".", "-delete", "/Groups/"+g)
			h.cleanupStep("dscacheutil", "-flushcache")
		}
	}
}

// removeUser deletes a user if it is there.
func (h *host) removeUser(name string) {
	h.t.Helper()
	there, err := userExists(name)
	if err != nil {
		h.t.Errorf("cleanup: %v", err)
	}
	if !there {
		return
	}
	if h.l.Platform == setup.Linux {
		h.cleanupStep("userdel", name)
		return
	}
	h.cleanupStep("dscl", ".", "-delete", "/Users/"+name)
	h.cleanupStep("dscacheutil", "-flushcache")
}

// userExists reports whether the user database has name.
func userExists(name string) (bool, error) {
	if runtime.GOOS == "darwin" {
		return dsclExists("/Users/" + name)
	}
	_, err := user.Lookup(name)
	var unknown user.UnknownUserError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &unknown):
		return false, nil
	default:
		return false, fmt.Errorf("look up user %s: %w", name, err)
	}
}

// groupExists reports whether the group database has name.
func groupExists(name string) (bool, error) {
	if runtime.GOOS == "darwin" {
		return dsclExists("/Groups/" + name)
	}
	_, err := user.LookupGroup(name)
	var unknown user.UnknownGroupError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &unknown):
		return false, nil
	default:
		return false, fmt.Errorf("look up group %s: %w", name, err)
	}
}

// dsclExists asks a fresh dscl process whether record exists. On macOS the
// test process's own user and group lookups go through libc's directory
// services cache, which can keep answering for a record that was just
// deleted, so they cannot prove a cleanup worked.
func dsclExists(record string) (bool, error) {
	_, stderr, err := runBounded(cmdTimeout, nil, "dscl", ".", "-read", record, "RecordName")
	there, err := dsclRecordExists(stderr, err)
	if err != nil {
		return false, fmt.Errorf("look up %s: %w", record, err)
	}
	return there, nil
}

// removePaths deletes the layout directories, then the parents that did
// not exist before the test, deepest first.
func (h *host) removePaths() {
	h.t.Helper()
	for _, dir := range h.removable() {
		if !deletable(dir) {
			h.t.Errorf("cleanup: refusing to delete %q", dir)
			continue
		}
		h.cleanupStep("rm", "-rf", dir)
	}
	for _, dir := range h.missing {
		there, err := exists(dir)
		if err != nil {
			h.t.Errorf("cleanup: %v", err)
		}
		if there && !h.isCLIDir(dir) {
			// rmdir removes only an empty directory, so nothing the test
			// did not make can go with it.
			h.cleanupStep("rmdir", dir)
		}
	}
}

// isCLIDir reports whether dir is above the runner's cli.toml, which the
// runner owns and removeCLIConfig removes.
func (h *host) isCLIDir(dir string) bool {
	return h.cliConfig != "" && strings.HasPrefix(h.cliConfig, dir+"/")
}

// removeCLIConfig deletes the cli.toml bootstrap wrote for the runner on
// macOS, and the directories made for it, unless it was there before.
func (h *host) removeCLIConfig() {
	h.t.Helper()
	if h.cliConfig == "" || h.cliConfigExisted {
		return
	}
	if err := os.Remove(h.cliConfig); err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.t.Errorf("cleanup: %v", err)
	}
	for _, dir := range h.missing {
		if !h.isCLIDir(dir) {
			continue
		}
		if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			h.t.Errorf("cleanup: %v", err)
		}
	}
}

// testCA is what cleanup needs to find the test CA in a trust store.
type testCA struct {
	der  []byte
	sha1 string // upper-case hex, as security prints it
	name string // the subject's common name, as dump-trust-settings prints it
}

// parseCA reads the test CA's PEM.
func parseCA(pemCert []byte) (testCA, error) {
	block, _ := pem.Decode(pemCert)
	if block == nil {
		return testCA{}, errors.New("the test CA is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return testCA{}, fmt.Errorf("the test CA: %w", err)
	}
	sum := sha1.Sum(cert.Raw)
	return testCA{der: cert.Raw, sha1: strings.ToUpper(hex.EncodeToString(sum[:])), name: cert.Subject.CommonName}, nil
}

// untrustCA removes the test CA from the system trust store.
func (h *host) untrustCA() {
	h.t.Helper()
	switch h.l.Platform {
	case setup.Linux:
		h.removeFile(linuxCAFile)
		h.cleanupStep("update-ca-certificates", "--fresh")
	case setup.Darwin:
		if h.caPEM == nil {
			return
		}
		ca, err := parseCA(h.caPEM)
		if err != nil {
			h.t.Errorf("cleanup: %v", err)
			return
		}
		trusted, err := h.adminTrust()
		if err != nil {
			h.t.Errorf("cleanup: %v", err)
		}
		if trustListsName(trusted, ca.name) {
			// This runs in a cleanup, where t.TempDir may not be called.
			file, err := writeTemp(h.caPEM)
			if err != nil {
				h.t.Errorf("cleanup: %v", err)
			} else {
				// Removing admin trust settings can wait for an authorization
				// dialog even as root, which a runner never answers. Leftover
				// settings are inert: the CA's private key died with this test
				// process, so nothing can present a chain it signs, and the
				// certificate itself is deleted from the keychain below.
				if err := h.sudoWithin(untrustTimeout, "security", "remove-trusted-cert", "-d", file); err != nil {
					h.t.Logf("cleanup: could not remove the admin trust settings for %q, which are inert without the CA key: %v", ca.name, err)
				}
				_ = os.Remove(file) // a leftover copy of a public certificate harms nothing
			}
		}
		listing, err := h.keychainListing()
		if err != nil {
			h.t.Errorf("cleanup: %v", err)
		}
		if keychainHasSHA1(listing, ca.sha1) {
			h.cleanupStep("security", "delete-certificate", "-Z", ca.sha1, systemKeychain)
		}
	}
}

// adminTrust is `security dump-trust-settings -d`: the certificates with
// admin trust settings. None at all is an empty dump, not an error.
func (h *host) adminTrust() (string, error) {
	h.t.Helper()
	out, stderr, err := h.run(nil, "sudo", "-n", "security", "dump-trust-settings", "-d")
	if err != nil && bytes.Contains(stderr, []byte("No Trust Settings were found")) {
		return "", nil
	}
	return string(out), err
}

// keychainListing lists every certificate in the System keychain with its
// hashes.
func (h *host) keychainListing() (string, error) {
	h.t.Helper()
	out, _, err := h.run(nil, "sudo", "-n", "security", "find-certificate", "-a", "-Z", systemKeychain)
	return string(out), err
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

// checkServiceGone checks the service manager no longer knows the service
// and its definition is gone.
func (h *host) checkServiceGone() {
	h.t.Helper()
	switch h.l.Platform {
	case setup.Linux:
		load, active, err := h.unitState()
		if err != nil || load != "not-found" || active != "inactive" {
			h.t.Errorf("cleanup left %s: load state %q, active state %q (%v), want not-found and inactive",
				h.l.ServiceLabel, load, active, err)
		}
	case setup.Darwin:
		if loaded, err := h.jobLoaded(); err != nil || loaded {
			h.t.Errorf("cleanup left launchd job %s (%v)", h.l.ServiceLabel, err)
		}
	}
	h.checkGone(h.l.Service)
}

// checkAccountsGone checks no user or group the test or bootstrap made is
// left.
func (h *host) checkAccountsGone() {
	h.t.Helper()
	for _, name := range []string{h.l.DaemonUser, otherUser} {
		if there, err := userExists(name); err != nil || there {
			h.t.Errorf("cleanup left user %s (%v)", name, err)
		}
	}
	groups := []string{h.l.ClientGroup}
	if h.l.Platform == setup.Linux {
		groups = append(groups, h.l.DaemonUser)
	}
	for _, name := range groups {
		if there, err := groupExists(name); err != nil || there {
			h.t.Errorf("cleanup left group %s (%v)", name, err)
		}
	}
}

// checkPathsGone checks every layout path, the socket, the removed
// directories, the parents the test made and the runner's new cli.toml are
// gone.
func (h *host) checkPathsGone() {
	h.t.Helper()
	paths := []string{h.l.Socket, h.l.Lock, h.l.Vault}
	for _, e := range h.l.Entries() {
		paths = append(paths, e.Path)
	}
	paths = append(paths, h.removable()...)
	paths = append(paths, h.missing...)
	if h.cliConfig != "" && !h.cliConfigExisted {
		paths = append(paths, h.cliConfig)
	}
	for _, path := range paths {
		h.checkGone(path)
	}
}

// checkGone reports path as left behind unless it does not exist.
func (h *host) checkGone(path string) {
	h.t.Helper()
	if there, err := exists(path); err != nil || there {
		h.t.Errorf("cleanup left %s (%v)", path, err)
	}
}

// checkCAGone checks the system trust store no longer holds the test CA.
func (h *host) checkCAGone() {
	h.t.Helper()
	if h.caPEM == nil {
		return
	}
	ca, err := parseCA(h.caPEM)
	if err != nil {
		h.t.Errorf("cleanup: %v", err)
		return
	}
	switch h.l.Platform {
	case setup.Linux:
		h.checkGone(linuxCAFile)
		bundle, err := os.ReadFile(linuxCABundle)
		if err != nil {
			h.t.Errorf("cleanup: read the CA bundle: %v", err)
		} else if bundleHolds(bundle, ca.der) {
			h.t.Errorf("cleanup left the test CA in %s", linuxCABundle)
		}
	case setup.Darwin:
		// Admin trust settings may outlive the cleanup (see untrustCA); they
		// are reported, not failed. The certificate itself must be gone.
		trusted, err := h.adminTrust()
		if err != nil || trustListsName(trusted, ca.name) {
			h.t.Logf("cleanup left admin trust settings for %q (%v); they are inert without the CA key", ca.name, err)
		}
		listing, err := h.keychainListing()
		if err != nil || keychainHasSHA1(listing, ca.sha1) {
			h.t.Errorf("cleanup left the test CA %s in %s (%v)", ca.sha1, systemKeychain, err)
		}
	}
}
