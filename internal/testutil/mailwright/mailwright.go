//go:build caldav

// Package mailwright runs a Mailwright server in a Docker container for the
// CalDAV integration suite (build tag caldav, stave test:caldav).
//
// The image comes from $CHAINMAIL_MAILWRIGHT_IMAGE, ideally pinned by
// digest (for example an ECR reference pulled on incus). With the variable
// unset, Main runs the tests without a server and Require skips them, so
// the suite costs nothing where no image is available.
//
// The container runs a generated config: one plaintext HTTP listener, a
// RocksDB store, a fallback admin with a random password and the cheapest
// password policy. Principals are created through the admin HTTP API.
//
// The simplest TestMain is
//
//	func TestMain(m *testing.M) { os.Exit(mailwright.Main(m)) }
//
// Main stops the container on every exit it can see. For the ones it
// cannot, each container stops itself after its lifetime, and the next
// Start removes containers and directories left by earlier runs once their
// lifetime has passed.
package mailwright

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
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
)

// ImageEnv names the variable holding the image reference.
const ImageEnv = "CHAINMAIL_MAILWRIGHT_IMAGE"

// Domain is the mail domain of every principal.
const Domain = "example.test"

// Lifetime is how long a container runs before it stops itself. Main
// stretches it to cover the test binary's -timeout. Change it before Start
// or Main, never while tests run.
var Lifetime = 20 * time.Minute

const (
	pullTimeout    = 5 * time.Minute
	commandTimeout = time.Minute
	readyTimeout   = 2 * time.Minute
	requestTimeout = 30 * time.Second
	killGrace      = 10 * time.Second
	pollInterval   = 50 * time.Millisecond

	namePrefix   = "chainmail-mailwright-"
	dirPrefix    = "chainmail-mailwright-"
	label        = "chainmail-test=mailwright"
	expiresLabel = "chainmail-test.expires"
	adminUser    = "admin"
	httpPort     = "8080/tcp"
)

// Server is a running Mailwright container.
type Server struct {
	// URL is the base URL of the HTTP listener, such as
	// http://127.0.0.1:49153.
	URL string

	adminPassword string
	id, dir       string

	domainOnce sync.Once
	domainErr  error
}

// Principal is an account created on the server.
type Principal struct {
	Name     string
	Email    string
	Password string
}

// config is the generated Mailwright configuration. %s is the fallback
// admin's password.
const config = `[server]
hostname = "mail.test"

[server.listener.http]
bind = "0.0.0.0:8080"
protocol = "http"

[session.auth]
allow-plaintext = true

[http.rate-limit]
anonymous = "100000/1m"
account = "100000/1m"

[storage]
data = "db"

[store.db]
type = "rocksdb"
path = "/var/lib/mailwright/data"

[admin.bootstrap]
enable = false

[authentication.fallback-admin]
user = "` + adminUser + `"
secret = "%s"

[authentication.password]
hash-algorithm = "pbkdf2"
min-strength = 0
`

// Image returns the configured image reference, "" when unset.
func Image() string { return strings.TrimSpace(os.Getenv(ImageEnv)) }

// Start runs a container from Image and waits until it is ready. The caller
// must Stop it.
func Start() (*Server, error) {
	return start(Image(), Lifetime)
}

func start(image string, lifetime time.Duration) (*Server, error) {
	if image == "" {
		return nil, fmt.Errorf("%s is not set", ImageEnv)
	}
	sweep(time.Now())
	if _, err := docker(commandTimeout, "image", "inspect", image); err != nil {
		if _, err := docker(pullTimeout, "pull", "-q", image); err != nil {
			return nil, err
		}
	}
	expires := time.Now().Add(lifetime)
	dir, err := os.MkdirTemp("", dirPrefix+strconv.FormatInt(expires.Unix(), 10)+"-")
	if err != nil {
		return nil, err
	}
	password := rand.Text()
	// The container runs as uid 10001, which must read the config. The
	// directory around it stays private to this user.
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), fmt.Appendf(nil, config, password), 0o644); err != nil {
		_ = os.RemoveAll(dir) // best effort; the write error is the one to report
		return nil, err
	}
	name := namePrefix + strings.ToLower(rand.Text()[:10])
	if _, err := docker(commandTimeout, runArgs(name, dir, image, lifetime, expires)...); err != nil {
		_, _ = docker(commandTimeout, "rm", "-f", name) // usually "no such container"; the run error is the one to report
		_ = os.RemoveAll(dir)                           // as above
		return nil, err
	}
	s := &Server{adminPassword: password, id: name, dir: dir}
	if err := s.connect(); err != nil {
		logs := s.Logs()
		return nil, errors.Join(fmt.Errorf("%w\ncontainer log:\n%s", err, logs), s.Stop())
	}
	return s, nil
}

// runArgs is the docker run command line. Docker's init stays PID 1 and
// timeout ends mailwright after lifetime, so the container stops by itself
// even when nothing is left to call Stop. There is no --rm, so a container
// that dies at startup keeps its log for Logs.
func runArgs(name, dir, image string, lifetime time.Duration, expires time.Time) []string {
	return []string{
		"run", "-d", "--name", name,
		"--label", label,
		"--label", expiresLabel + "=" + strconv.FormatInt(expires.Unix(), 10),
		"-p", "127.0.0.1::8080",
		"-v", filepath.Join(dir, "config.toml") + ":/etc/mailwright/config.toml:ro",
		"--init",
		"--entrypoint", "/usr/bin/timeout",
		image,
		"-k", seconds(killGrace), seconds(lifetime),
		"/usr/local/bin/mailwright",
	}
}

func seconds(d time.Duration) string {
	return strconv.FormatInt(max(1, int64(d/time.Second)), 10) + "s"
}

// sweep removes, best effort, containers and directories that earlier runs
// left behind and whose lifetime has passed.
func sweep(now time.Time) {
	out, err := docker(commandTimeout, "ps", "-a", "--filter", "label="+label,
		"--format", `{{.Names}} {{.Label "`+expiresLabel+`"}}`)
	if err == nil {
		if stale := staleContainers(out, now); len(stale) > 0 {
			_, _ = docker(commandTimeout, append([]string{"rm", "-f"}, stale...)...) // best effort
		}
	}
	sweepDirs(os.TempDir(), now, Lifetime)
}

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

// Shared returns the server Main started, or nil.
func Shared() *Server { return shared.Load() }

// Require returns the shared server, or skips the test when Main started
// none because ImageEnv is unset.
func Require(t testing.TB) *Server {
	t.Helper()
	s := Shared()
	if s == nil {
		t.Skipf("%s is not set; the Mailwright CalDAV suite needs an image", ImageEnv)
	}
	return s
}

// Main starts a server when ImageEnv is set, runs the tests and stops the
// server, returning the exit code for os.Exit. It stops the server on a
// normal return, on a panic in the main goroutine, and on SIGINT or
// SIGTERM.
func Main(m *testing.M) int {
	if !flag.Parsed() {
		flag.Parse()
	}
	image := Image()
	if image == "" {
		fmt.Fprintf(os.Stderr, "mailwright: %s is not set; CalDAV integration tests skip\n", ImageEnv)
		return m.Run()
	}
	lifetime := lifetimeFor(Lifetime, testTimeout())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	return runMain(func() (*Server, error) { return start(image, lifetime) }, (*Server).Stop, m.Run, sigs, os.Exit)
}

const startupSlack = readyTimeout + 2*commandTimeout + killGrace + 50*time.Second

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

// runMain is Main with its effects passed in.
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
			fmt.Fprintln(os.Stderr, "mailwright: stopping:", stopErr)
		}
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case sig := <-sigs:
			interrupted.Store(true)
			fmt.Fprintf(os.Stderr, "mailwright: %v: removing the container\n", sig)
			stopServer()
			exit(1)
		case <-done:
		}
	}()
	s, err := start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mailwright: starting:", err)
		return 1
	}
	mu.Lock()
	srv = s
	shared.Store(s)
	mu.Unlock()
	defer stopServer()
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

// connect finds the published port and waits for /healthz/ready.
func (s *Server) connect() error {
	out, err := docker(commandTimeout, "port", s.id, httpPort)
	if err != nil {
		return err
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	host, port, err := net.SplitHostPort(strings.TrimSpace(first))
	if err != nil {
		return fmt.Errorf("parsing docker port output %q: %w", out, err)
	}
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	s.URL = "http://" + net.JoinHostPort(host, port)
	deadline := time.Now().Add(readyTimeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		resp, err := client.Get(s.URL + "/healthz/ready")
		if err == nil {
			_ = resp.Body.Close() // nothing to read
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mailwright not ready after %v: %w", readyTimeout, err)
		}
		time.Sleep(pollInterval)
	}
}

// CreatePrincipal creates an individual named name with the address
// name@Domain, the "user" role every DAV request needs, and a random
// password. The domain is created on first use.
func (s *Server) CreatePrincipal(name string) (Principal, error) {
	s.domainOnce.Do(func() {
		s.domainErr = s.admin(map[string]any{"type": "domain", "name": Domain})
	})
	if s.domainErr != nil {
		return Principal{}, fmt.Errorf("creating domain: %w", s.domainErr)
	}
	p := Principal{Name: name, Email: name + "@" + Domain, Password: rand.Text()}
	err := s.admin(map[string]any{
		"type":    "individual",
		"name":    p.Name,
		"emails":  []string{p.Email},
		"secrets": []string{p.Password},
		"roles":   []string{"user"},
	})
	if err != nil {
		return Principal{}, fmt.Errorf("creating principal %s: %w", name, err)
	}
	return p, nil
}

// admin POSTs a principal to the admin API as the fallback admin.
func (s *Server) admin(principal map[string]any) error {
	body, err := json.Marshal(principal)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/api/principal", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(adminUser, s.adminPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() // read below
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("admin API: status %d: %s", resp.StatusCode, bytes.TrimSpace(reply))
	}
	return nil
}

// HTTPClient returns a client that authenticates every request as p with
// Basic, as chainmail's auth.Manager does for password accounts.
func (s *Server) HTTPClient(p Principal) *http.Client {
	return &http.Client{
		Timeout:   requestTimeout,
		Transport: &basicAuth{user: p.Name, password: p.Password, base: http.DefaultTransport},
	}
}

type basicAuth struct {
	user, password string
	base           http.RoundTripper
}

func (b *basicAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.SetBasicAuth(b.user, b.password)
	return b.base.RoundTrip(out)
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
