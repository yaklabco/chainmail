// Package dovecot runs a Dovecot IMAP server in a Docker container for
// integration tests. Start one in TestMain and share it: the static passdb
// accepts any user name with Password, so each test can use its own mailbox.
//
// The simplest TestMain is
//
//	func TestMain(m *testing.M) { os.Exit(dovecot.Main(m)) }
//
// with tests reaching the server through Shared. Main stops the container
// on every exit it can see. For the ones it cannot (a test panic, a -timeout
// expiry, SIGKILL), each container stops itself after its lifetime, and the
// next Start removes containers and temporary directories left by earlier
// runs once their lifetime has passed.
package dovecot

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// Image is the pinned Dovecot image.
const Image = "dovecot/dovecot:2.3.21.1"

// Password logs in any user.
const Password = "pw"

// Lifetime is how long a container runs before it stops itself, so one
// outlives a killed test binary by at most this long. It is longer than the
// 15-minute -timeout of stave test:integration, and Main stretches it to
// cover a longer -timeout. With -timeout 0 (a .test binary or a debugger run
// directly) Main uses it as is, so raise it before a debugging session that
// may outlast it. Change it before Start or Main, never while tests run.
var Lifetime = 20 * time.Minute

const (
	pullTimeout    = 5 * time.Minute
	commandTimeout = time.Minute
	readyTimeout   = time.Minute
	// killGrace is how long dovecot gets to exit after its lifetime's SIGTERM.
	killGrace = 10 * time.Second

	namePrefix   = "chainmail-dovecot-"
	dirPrefix    = "chainmail-dovecot-"
	label        = "chainmail-test=dovecot"
	expiresLabel = "chainmail-test.expires"
)

//go:embed dovecot.conf
var config []byte

// prefixedConfig is config with the personal namespace under "INBOX.", with
// "." between levels, as Courier-style servers such as Hostinger's keep it.
// Mailbox sections inside the namespace name folders without the prefix.
var prefixedConfig = bytes.Replace(config, []byte("  separator = /\n"), []byte("  prefix = INBOX.\n  separator = .\n"), 1)

// Server is a running Dovecot container.
type Server struct {
	Endpoint provider.Endpoint
	CA       *tlstest.CA
	id       string
	dir      string
}

// Start runs a container with a fresh certificate and waits until it
// answers IMAPS. The caller must Stop it. It first removes containers and
// directories from earlier runs whose lifetime has passed.
func Start() (*Server, error) {
	return start(Lifetime, config)
}

// StartPrefixed is Start with the personal namespace under "INBOX.", with
// "." between levels, as Hostinger's Dovecot keeps it. The caller must Stop it.
func StartPrefixed() (*Server, error) {
	return start(Lifetime, prefixedConfig)
}

// start runs a container whose Dovecot reads conf as its dovecot.conf.
func start(lifetime time.Duration, conf []byte) (*Server, error) {
	sweep(time.Now())
	ca, err := tlstest.NewCA()
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := ca.PEM()
	if err != nil {
		return nil, err
	}
	if _, err := docker(commandTimeout, "image", "inspect", Image); err != nil {
		if _, err := docker(pullTimeout, "pull", "-q", Image); err != nil {
			return nil, err
		}
	}
	expires := time.Now().Add(lifetime)
	dir, err := os.MkdirTemp("", dirPrefix+strconv.FormatInt(expires.Unix(), 10)+"-")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"dovecot.conf": conf, "cert.pem": certPEM, "key.pem": keyPEM}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			_ = os.RemoveAll(dir) // best effort; the write error is the one to report
			return nil, err
		}
	}
	// Name the container up front, so a run that times out can still be removed.
	name := namePrefix + strings.ToLower(rand.Text()[:10])
	if _, err := docker(commandTimeout, runArgs(name, dir, lifetime, expires)...); err != nil {
		_, _ = docker(commandTimeout, "rm", "-f", name) // usually "no such container"; the run error is the one to report
		_ = os.RemoveAll(dir)                           // as above
		return nil, err
	}
	s := &Server{CA: ca, id: name, dir: dir}
	if err := s.connect(); err != nil {
		logs := s.Logs()
		return nil, errors.Join(fmt.Errorf("%w\ncontainer log:\n%s", err, logs), s.Stop())
	}
	return s, nil
}

// runArgs is the docker run command line. There is no --rm: a container that
// dies at startup keeps its log for Logs, and Stop removes it. tini stays PID 1
// and timeout ends dovecot after lifetime, so the container stops by itself
// even when nothing is left to call Stop.
func runArgs(name, dir string, lifetime time.Duration, expires time.Time) []string {
	return []string{
		"run", "-d", "--name", name,
		"--label", label,
		"--label", expiresLabel + "=" + strconv.FormatInt(expires.Unix(), 10),
		"-p", "127.0.0.1::993",
		"-v", filepath.Join(dir, "dovecot.conf") + ":/etc/dovecot/dovecot.conf:ro",
		"-v", filepath.Join(dir, "cert.pem") + ":/etc/dovecot/tls/cert.pem:ro",
		"-v", filepath.Join(dir, "key.pem") + ":/etc/dovecot/tls/key.pem:ro",
		"--entrypoint", "/usr/bin/tini",
		Image, "--",
		"/usr/bin/timeout", "-k", seconds(killGrace), seconds(lifetime),
		"/usr/sbin/dovecot", "-F",
	}
}

func seconds(d time.Duration) string {
	return strconv.FormatInt(max(1, int64(d/time.Second)), 10) + "s"
}

// sweep removes, best effort, containers and directories that earlier runs
// left behind and whose lifetime has passed. Live ones, perhaps another test
// binary's, are left alone.
func sweep(now time.Time) {
	out, err := docker(commandTimeout, "ps", "-a", "--filter", "label="+label,
		"--format", `{{.Names}} {{.Label "`+expiresLabel+`"}}`)
	if err == nil {
		if stale := staleContainers(out, now); len(stale) > 0 {
			_, _ = docker(commandTimeout, append([]string{"rm", "-f"}, stale...)...) // best effort; Start goes on regardless
		}
	}
	sweepDirs(os.TempDir(), now, Lifetime)
}

// staleContainers picks from "name expiry" lines the containers whose expiry
// has passed or cannot be read.
func staleContainers(out string, now time.Time) []string {
	var stale []string
	for line := range strings.Lines(out) {
		name, exp, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name == "" {
			continue
		}
		if unix, err := strconv.ParseInt(exp, 10, 64); err != nil || unix < now.Unix() {
			stale = append(stale, name)
		}
	}
	return stale
}

// sweepDirs removes the chainmail-dovecot-* directories in root whose expiry,
// from their name, has passed, and those without one older than lifetime.
func sweepDirs(root string, now time.Time, lifetime time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), dirPrefix)
		if ok && e.IsDir() && dirExpired(rest, e, now, lifetime) {
			_ = os.RemoveAll(filepath.Join(root, e.Name())) // best effort
		}
	}
}

func dirExpired(rest string, e fs.DirEntry, now time.Time, lifetime time.Duration) bool {
	if exp, _, ok := strings.Cut(rest, "-"); ok {
		if unix, err := strconv.ParseInt(exp, 10, 64); err == nil {
			return unix < now.Unix()
		}
	}
	info, err := e.Info()
	return err == nil && info.ModTime().Before(now.Add(-lifetime))
}

var shared atomic.Pointer[Server]

// Shared returns the server Main started, or nil outside Main.
func Shared() *Server { return shared.Load() }

// Main starts a server, runs the tests and stops the server, returning the
// exit code for os.Exit. It stops the server on a normal return, on a panic
// in the main goroutine, and on SIGINT or SIGTERM. While the tests run,
// Shared returns the server.
func Main(m *testing.M) int {
	if !flag.Parsed() {
		flag.Parse() // for -test.timeout; m.Run does not parse again
	}
	lifetime := lifetimeFor(Lifetime, testTimeout())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	return runMain(func() (*Server, error) { return start(lifetime, config) }, (*Server).Stop, m.Run, sigs, os.Exit)
}

// startupSlack covers the time between a container's start, when its
// lifetime begins, and m.Run, when the test binary's -timeout clock does.
const startupSlack = readyTimeout + 2*commandTimeout + killGrace + 50*time.Second

// lifetimeFor stretches base to outlast a test binary that runs for timeout.
// A timeout of 0, which is the default when a .test binary or a debugger
// runs directly, means no limit, and base is used as is.
func lifetimeFor(base, timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return base
	}
	return max(base, timeout+startupSlack)
}

func testTimeout() time.Duration {
	f := flag.Lookup("test.timeout")
	if f == nil {
		return 0
	}
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return 0
	}
	d, _ := g.Get().(time.Duration) // zero, meaning no timeout, if it is not a duration
	return d
}

// runMain is Main with its effects passed in. Signals are watched from the
// start: one that arrives while start is still running exits at once, and
// the container start creates then stops itself after its lifetime.
func runMain(start func() (*Server, error), stop func(*Server) error, run func() int, sigs <-chan os.Signal, exit func(int)) int {
	var (
		mu          sync.Mutex
		srv         *Server
		stopped     bool
		stopErr     error
		interrupted atomic.Bool
	)
	stopServer := func() {
		mu.Lock()
		defer mu.Unlock()
		if srv == nil || stopped {
			return
		}
		stopped = true
		shared.CompareAndSwap(srv, nil)
		if stopErr = stop(srv); stopErr != nil {
			fmt.Fprintln(os.Stderr, "dovecot: stopping:", stopErr)
		}
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case sig := <-sigs:
			interrupted.Store(true)
			fmt.Fprintf(os.Stderr, "dovecot: %v: removing the container\n", sig)
			stopServer()
			exit(1)
		case <-done:
		}
	}()
	s, err := start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dovecot: starting:", err)
		return 1
	}
	mu.Lock()
	srv = s
	shared.Store(s)
	mu.Unlock()
	defer stopServer() // a panic in run skips the call below
	if interrupted.Load() {
		stopServer()
		return 1
	}
	code := run()
	stopServer()
	if code == 0 && (stopErr != nil || interrupted.Load()) {
		code = 1
	}
	return code
}

func (s *Server) connect() error {
	out, err := docker(commandTimeout, "port", s.id, "993/tcp")
	if err != nil {
		return err
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(first))
	if err != nil {
		return fmt.Errorf("parsing docker port output %q: %w", out, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("parsing port %q: %w", portStr, err)
	}
	s.Endpoint = provider.Endpoint{Host: "127.0.0.1", Port: port, Security: provider.SecurityTLS}
	deadline := time.Now().Add(readyTimeout)
	for {
		c, err := s.Client("ready@example.test")
		if err == nil {
			return errors.Join(c.Logout().Wait(), c.Close())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dovecot not ready after %v: %w", readyTimeout, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Client logs in as user over IMAPS. The caller logs out and closes it.
func (s *Server) Client(user string) (*imapclient.Client, error) {
	c, err := imapclient.DialTLS(s.Endpoint.Addr(), &imapclient.Options{
		TLSConfig: &tls.Config{RootCAs: s.CA.Pool, MinVersion: tls.VersionTLS12},
	})
	if err != nil {
		return nil, err
	}
	if err := c.Login(user, Password).Wait(); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func (s *Server) with(user string, f func(*imapclient.Client) error) error {
	c, err := s.Client(user)
	if err != nil {
		return err
	}
	ferr := f(c)
	return errors.Join(ferr, c.Logout().Wait(), c.Close())
}

// Append stores raw in folder with the given arrival date and flags and returns its UID.
func (s *Server) Append(user, folder, raw string, date time.Time, flags ...imap.Flag) (imap.UID, error) {
	var uid imap.UID
	err := s.with(user, func(c *imapclient.Client) error {
		cmd := c.Append(folder, int64(len(raw)), &imap.AppendOptions{Flags: flags, Time: date})
		// Close the command even after a failed write: until it ends, the
		// client holds its encoder lock and the Logout in with would hang.
		_, werr := cmd.Write([]byte(raw))
		if err := errors.Join(werr, cmd.Close()); err != nil {
			return err
		}
		data, err := cmd.Wait()
		if err != nil {
			return err
		}
		uid = data.UID
		return nil
	})
	return uid, err
}

// Flags reads a message's flags without changing them.
func (s *Server) Flags(user, folder string, uid imap.UID) ([]imap.Flag, error) {
	var flags []imap.Flag
	err := s.with(user, func(c *imapclient.Client) error {
		if _, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return err
		}
		msgs, err := c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{Flags: true}).Collect()
		if err != nil {
			return err
		}
		if len(msgs) != 1 {
			return fmt.Errorf("UID %d not found in %s", uid, folder)
		}
		flags = msgs[0].Flags
		return nil
	})
	return flags, err
}

// UIDValidity returns a folder's UIDVALIDITY.
func (s *Server) UIDValidity(user, folder string) (uint32, error) {
	var v uint32
	err := s.with(user, func(c *imapclient.Client) error {
		data, err := c.Status(folder, &imap.StatusOptions{UIDValidity: true}).Wait()
		if err != nil {
			return err
		}
		v = data.UIDValidity
		return nil
	})
	return v, err
}

// SetUIDValidity changes a folder's UIDVALIDITY, as a server-side reset
// would, and disconnects the user's sessions so clients notice.
func (s *Server) SetUIDValidity(user, folder string, v uint32) error {
	if _, err := docker(commandTimeout, "exec", s.id, "doveadm", "mailbox", "update", "-u", user,
		"--uid-validity", strconv.FormatUint(uint64(v), 10), folder); err != nil {
		return err
	}
	// kick exits non-zero when the user has no sessions, which is fine here.
	_ = s.Kick(user)
	return nil
}

// Kick disconnects every session of user.
func (s *Server) Kick(user string) error {
	_, err := docker(commandTimeout, "exec", s.id, "doveadm", "kick", user)
	return err
}

// Logs returns the container's output, for failure messages.
func (s *Server) Logs() string {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "logs", s.id).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(docker logs failed: %v)\n%s", err, out)
	}
	return string(out)
}

// Stop removes the container and its files.
func (s *Server) Stop() error {
	_, err := docker(commandTimeout, "rm", "-f", s.id)
	return errors.Join(err, os.RemoveAll(s.dir))
}

func docker(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
