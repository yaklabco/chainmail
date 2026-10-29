// Package dovecot runs a Dovecot IMAP server in a Docker container for
// integration tests. Start one in TestMain and share it: the static passdb
// accepts any user name with Password, so each test can use its own mailbox.
package dovecot

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

const (
	pullTimeout    = 5 * time.Minute
	commandTimeout = time.Minute
	readyTimeout   = time.Minute
)

//go:embed dovecot.conf
var config []byte

// Server is a running Dovecot container.
type Server struct {
	Endpoint provider.Endpoint
	CA       *tlstest.CA
	id       string
	dir      string
}

// Start runs a container with a fresh certificate and waits until it
// answers IMAPS. The caller must Stop it.
func Start() (*Server, error) {
	ca, err := tlstest.NewCA()
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := ca.PEM()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "chainmail-dovecot-")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"dovecot.conf": config, "cert.pem": certPEM, "key.pem": keyPEM}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			_ = os.RemoveAll(dir) // best effort; the write error is the one to report
			return nil, err
		}
	}
	if _, err := docker(commandTimeout, "image", "inspect", Image); err != nil {
		if _, err := docker(pullTimeout, "pull", "-q", Image); err != nil {
			_ = os.RemoveAll(dir) // as above
			return nil, err
		}
	}
	out, err := docker(commandTimeout, "run", "-d", "--rm",
		"--name", "chainmail-dovecot-"+rand.Text()[:10],
		"-p", "127.0.0.1::993",
		"-v", filepath.Join(dir, "dovecot.conf")+":/etc/dovecot/dovecot.conf:ro",
		"-v", filepath.Join(dir, "cert.pem")+":/etc/dovecot/tls/cert.pem:ro",
		"-v", filepath.Join(dir, "key.pem")+":/etc/dovecot/tls/key.pem:ro",
		Image)
	if err != nil {
		_ = os.RemoveAll(dir) // as above
		return nil, err
	}
	s := &Server{CA: ca, id: strings.TrimSpace(out), dir: dir}
	if err := s.connect(); err != nil {
		logs := s.Logs()
		return nil, errors.Join(fmt.Errorf("%w\ncontainer log:\n%s", err, logs), s.Stop())
	}
	return s, nil
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
