package imapstore

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/provider"
)

// Credentialer issues login credentials for an account and hears when a
// server refuses them. *auth.Manager implements it.
type Credentialer interface {
	Credentials(ctx context.Context, account string) (auth.Credentials, error)
	Rejected(account string, creds auth.Credentials)
}

// Options tune a Store. The zero value gives the spec's values.
type Options struct {
	// RootCAs replaces the system roots. Tests use it.
	RootCAs *x509.CertPool
	// PoolSize caps connections per account. Zero means 3.
	PoolSize int
	// CommandTimeout bounds each IMAP command. In a pipeline, each command
	// that completes gives the rest a fresh timeout. Zero means 20s.
	CommandTimeout time.Duration
	// NoopAfter is how long a connection may sit idle before a NOOP checks
	// it on reuse. Zero means 60s.
	NoopAfter time.Duration
	// IdleLogout logs out a connection idle this long. Zero means 10 minutes.
	IdleLogout time.Duration
	// Retries is how often an operation is retried after a dropped
	// connection. Zero means 3; negative means none. A command that timed
	// out is retried at most once, within this limit.
	Retries int
	// Backoff is the wait before retry n, counting from 0. Nil means
	// jittered exponential backoff from 200ms.
	Backoff func(attempt int) time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
	// Logger defaults to discarding everything.
	Logger *slog.Logger
}

const (
	defaultPoolSize       = 3
	defaultCommandTimeout = 20 * time.Second
	defaultNoopAfter      = 60 * time.Second
	defaultIdleLogout     = 10 * time.Minute
	defaultRetries        = 3
	dialTimeout           = 30 * time.Second
)

func (o Options) withDefaults() Options {
	if o.PoolSize <= 0 {
		o.PoolSize = defaultPoolSize
	}
	if o.CommandTimeout <= 0 {
		o.CommandTimeout = defaultCommandTimeout
	}
	if o.NoopAfter <= 0 {
		o.NoopAfter = defaultNoopAfter
	}
	if o.IdleLogout <= 0 {
		o.IdleLogout = defaultIdleLogout
	}
	switch {
	case o.Retries == 0:
		o.Retries = defaultRetries
	case o.Retries < 0:
		o.Retries = 0
	}
	if o.Backoff == nil {
		o.Backoff = jitteredBackoff
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return o
}

// jitteredBackoff waits about 200ms, 400ms, 800ms, give or take half.
func jitteredBackoff(attempt int) time.Duration {
	base := 200 * time.Millisecond << attempt
	return base/2 + rand.N(base)
}

// Store reads one account over IMAP. It implements mailstore.Store and is
// the only code that talks to IMAP servers.
type Store struct {
	acct  config.ResolvedAccount
	creds Credentialer
	opts  Options
	pool  *pool[*conn]

	// loginGate holds one token, and every dial takes it, so logins wait in
	// line and a bad password is tried once, not once per pooled connection.
	// That holds after a password is rotated or revoked while the daemon
	// runs, too. The account parks on the refusal, and the dials behind it
	// get AUTH_REQUIRED from the Credentialer without dialing. The cost is
	// that concurrent dials, which happen when the pool fills, run one after
	// another. It is a channel rather than a mutex so a waiter can give up
	// when its context ends.
	loginGate chan struct{}

	mu      sync.Mutex
	folders []mailstore.Folder // cached; nil until listed
	// borrowed counts connections out of the pool, a stream reader's
	// included. drained, when set, is closed when it reaches zero.
	borrowed int
	drained  chan struct{}
}

var _ mailstore.Store = (*Store)(nil)

// closeWait bounds how long Close waits for borrowed connections to come back.
const closeWait = 5 * time.Second

// New returns a Store for acct. It connects on first use.
func New(acct config.ResolvedAccount, creds Credentialer, opts Options) *Store {
	opts = opts.withDefaults()
	s := &Store{acct: acct, creds: creds, opts: opts, loginGate: make(chan struct{}, 1)}
	s.pool = newPool(s.dial, poolOptions{size: opts.PoolSize, noopAfter: opts.NoopAfter, idleLogout: opts.IdleLogout, now: opts.Now})
	return s
}

func (s *Store) gmail() bool { return s.acct.Provider.Gmail }

// Close refuses new work and logs out of every connection, in parallel.
//
// Call it once every operation has returned and every reader StreamPart
// gave out is closed, as the daemon does by draining its handlers before it
// closes stores. Close still waits up to 5 seconds for connections that are
// out, logging each out as it comes back, and reports any that did not.
func (s *Store) Close() error {
	idle := make(chan error, 1)
	go func() { idle <- s.pool.close() }()
	s.mu.Lock()
	var wait chan struct{}
	if s.borrowed > 0 {
		if s.drained == nil {
			s.drained = make(chan struct{})
		}
		wait = s.drained
	}
	s.mu.Unlock()
	var errs []error
	if wait != nil {
		timer := time.NewTimer(closeWait)
		select {
		case <-wait:
		case <-timer.C:
			s.mu.Lock()
			n := s.borrowed
			s.mu.Unlock()
			errs = append(errs, fmt.Errorf("closing %s: %d connections still in use after %v", s.acct.Name, n, closeWait))
		}
		timer.Stop()
	}
	errs = append(errs, <-idle)
	return errors.Join(errs...)
}

// borrow takes a connection from the pool, preferring one with folder open.
// Every borrow is paired with a release.
func (s *Store) borrow(ctx context.Context, folder string) (*conn, error) {
	c, err := s.pool.get(ctx, folder)
	if err != nil {
		if errors.Is(err, errPoolClosed) {
			return nil, fmt.Errorf("%w: %w", mailstore.ErrUnavailable, err)
		}
		return nil, err
	}
	s.mu.Lock()
	s.borrowed++
	s.mu.Unlock()
	return c, nil
}

// release hands a borrowed connection back, or drops it if it broke.
func (s *Store) release(c *conn) {
	if c.Dead() {
		s.pool.discard(c)
	} else {
		s.pool.put(c)
	}
	s.mu.Lock()
	s.borrowed--
	if s.borrowed == 0 && s.drained != nil {
		close(s.drained)
		s.drained = nil
	}
	s.mu.Unlock()
}

func (s *Store) dial(ctx context.Context) (*conn, error) {
	select {
	case s.loginGate <- struct{}{}:
		defer func() { <-s.loginGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	creds, err := s.creds.Credentials(ctx, s.acct.Name)
	if err != nil {
		return nil, err
	}
	sess, err := Dial(ctx, s.acct.IMAP, creds, DialOptions{RootCAs: s.opts.RootCAs, Timeout: dialTimeout})
	switch {
	case err == nil:
		return newConn(sess, s.opts.CommandTimeout, s.opts.Now), nil
	case isConnLimit(err):
		s.pool.shrink()
		s.opts.Logger.Warn("server refused another connection; pool shrunk", "account", s.acct.Name)
		return nil, fmt.Errorf("%w: %s refused another connection: %w", mailstore.ErrUnavailable, s.acct.Name, err)
	case errors.Is(err, auth.ErrRejected):
		s.creds.Rejected(s.acct.Name, creds)
		return nil, &auth.RejectedError{Hint: rejectedHint(s.acct), Err: err}
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case isRefusal(err):
		// The server answered, so trying again would only resend the
		// credentials: NO [ALERT] for an app password, NO [UNAVAILABLE],
		// BAD for LOGINDISABLED and the like.
		return nil, fmt.Errorf("%w: %s refused the login: %w", mailstore.ErrUnavailable, s.acct.Name, err)
	default:
		// A transport or TLS failure, or a BYE: worth a fresh connection.
		return nil, fmt.Errorf("%w: %w", errConnLost, err)
	}
}

// isRefusal reports a NO or BAD reply from the server.
func isRefusal(err error) bool {
	var e *imap.Error
	return errors.As(err, &e) && (e.Type == imap.StatusResponseTypeNo || e.Type == imap.StatusResponseTypeBad)
}

func rejectedHint(acct config.ResolvedAccount) string {
	if acct.Auth == provider.AuthOAuth2 {
		return "the server rejected the access token; run: chainmail auth login " + acct.Name
	}
	return "the server rejected the password; run: chainmail secret set " + auth.PasswordSecret(acct.Name)
}

// isConnLimit reports a server refusing a connection for being over its limit.
func isConnLimit(err error) bool {
	var e *imap.Error
	if !errors.As(err, &e) {
		return false
	}
	if e.Code == imap.ResponseCodeLimit {
		return true
	}
	text := strings.ToLower(e.Text)
	return strings.Contains(text, "too many") || strings.Contains(text, "maximum number of connections")
}

// withConn runs f on a pooled connection open on folder when possible,
// retrying on a fresh connection when the old one dropped. f must be safe
// to run again.
func (s *Store) withConn(ctx context.Context, folder string, f func(*conn) error) error {
	return s.retry(ctx, func() error { return s.tryConn(ctx, folder, f) })
}

// timeoutRetries is how often an operation is retried after a command timed
// out. A dropped connection gets Retries; a timeout gets one fresh
// connection, in case the old one was wedged, and no more: a server or link
// too slow for the bound stays too slow, and each retry costs another
// timeout plus a refetch of the whole batch.
const timeoutRetries = 1

// retry runs try again after a dropped connection, up to Retries times with
// backoff, then reports the account unavailable. A timed-out command is
// retried at most timeoutRetries times. Every other outcome is final.
func (s *Store) retry(ctx context.Context, try func() error) error {
	timeouts := 0
	for attempt := 0; ; attempt++ {
		err := try()
		if err == nil || !errors.Is(err, errConnLost) {
			return s.serverError(err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errCommandTimeout) {
			timeouts++
		}
		if attempt >= s.opts.Retries || timeouts > timeoutRetries {
			return fmt.Errorf("%w: %s after %d attempts: %w", mailstore.ErrUnavailable, s.acct.Name, attempt+1, err)
		}
		s.opts.Logger.Debug("retrying after a dropped connection", "account", s.acct.Name, "attempt", attempt+1, "err", err)
		if err := sleep(ctx, s.opts.Backoff(attempt)); err != nil {
			return err
		}
	}
}

func (s *Store) tryConn(ctx context.Context, folder string, f func(*conn) error) error {
	c, err := s.borrow(ctx, folder)
	if err != nil {
		return err
	}
	defer s.release(c)
	return f(c)
}

// open opens folder read-only on c. A folder that turns out to be missing
// drops the cached folder list, so the next search lists folders again.
func (s *Store) open(ctx context.Context, c *conn, folder string) error {
	err := c.examine(ctx, folder)
	if errors.Is(err, mailstore.ErrNotFound) {
		s.forgetFolders()
	}
	return err
}

// serverError marks a refusal from the server (NO or BAD) as unavailable,
// unless it already means something more specific.
func (s *Store) serverError(err error) error {
	var imapErr *imap.Error
	if err == nil || !errors.As(err, &imapErr) || errors.Is(err, auth.ErrRejected) || errors.Is(err, auth.ErrAuthRequired) ||
		errors.Is(err, mailstore.ErrUnavailable) || errors.Is(err, mailstore.ErrNotFound) || errors.Is(err, mailstore.ErrStaleRef) {
		return err
	}
	return fmt.Errorf("%w: %s refused: %w", mailstore.ErrUnavailable, s.acct.Name, err)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
