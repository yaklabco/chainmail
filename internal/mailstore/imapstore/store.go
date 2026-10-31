package imapstore

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
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
	// connection. Zero means 3; negative means none; more than 10 means 10.
	// A command that timed out is retried at most once, within this limit.
	Retries int
	// Backoff is the wait before retry n, counting from 0. Nil means
	// jittered exponential backoff from 200ms.
	Backoff func(attempt int) time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
	// Logger defaults to discarding everything.
	Logger *slog.Logger
	// OnConnLimit, when set, hears the pool's new cap each time the server
	// refuses a connection for being over its limit and the pool shrinks. It
	// runs on the goroutine that dialed, so it must not block for long.
	OnConnLimit func(newMax int)
	// ArchiveFolder is where Archive moves mail on standard IMAP accounts.
	// Empty means the account's archive_folder, then the folder with the
	// \Archive role.
	ArchiveFolder string
}

const (
	defaultPoolSize       = 3
	defaultCommandTimeout = 20 * time.Second
	defaultNoopAfter      = 60 * time.Second
	defaultIdleLogout     = 10 * time.Minute
	defaultRetries        = 3
	// maxRetries caps Retries, so a misconfigured count cannot keep an
	// operation retrying for hours.
	maxRetries  = 10
	dialTimeout = 30 * time.Second
	// freshFor is how long a connection trusts what the server last told it
	// of the folder it has open, at its last SELECT, EXAMINE or NOOP.
	// Reusing the folder later, it sends NOOP first. So a change made
	// elsewhere shows within freshFor, and a run of calls on one folder
	// costs at most one NOOP per connection each freshFor.
	freshFor = time.Second
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
	case o.Retries > maxRetries:
		o.Retries = maxRetries
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

// jitteredBackoff waits about 200ms, 400ms, 800ms, give or take half. The
// wait stops growing at the ninth attempt, about 51s, so the shift never
// overflows.
func jitteredBackoff(attempt int) time.Duration {
	base := 200 * time.Millisecond << min(max(attempt, 0), 8)
	return base/2 + rand.N(base)
}

// Store reads and changes one account over IMAP. It implements
// mailstore.Store and is the only code that talks to IMAP servers.
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

	// chunk is how much of a part each FETCH asks for while streaming:
	// chunkSize, unless a test shrinks it.
	chunk int64
	// dialTimeout bounds connecting and logging in: the package's
	// dialTimeout, unless a test shrinks it.
	dialTimeout time.Duration

	// folderGen counts renames and deletes of folders. A connection opened
	// a folder before the latest one opens it again rather than trust it.
	folderGen atomic.Uint64
	// writeGen counts writes sent through this store. A connection that has
	// not looked at its folder since the latest one opens it again before it
	// reuses it, even within freshFor, so the server tells it of mail
	// another connection changed.
	writeGen atomic.Uint64

	mu      sync.Mutex
	folders []mailstore.Folder // cached; nil until listed
	// nsPrefix is the personal namespace prefix, once nsKnown. It is kept
	// when the folder list is dropped.
	nsPrefix string
	nsKnown  bool
	system   systemSpace // the system namespace of the cached list
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
	s := &Store{acct: acct, creds: creds, opts: opts, loginGate: make(chan struct{}, 1), chunk: chunkSize, dialTimeout: dialTimeout}
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

// OpenConns returns how many of the store's pooled connections hold a slot
// on the server: dialing, borrowed, idle or still logging out. A borrowed
// one, such as a stream reader's, counts until it comes back and has logged
// out, after Close too. ProbeDial's connections are not pooled and do not
// count.
func (s *Store) OpenConns() int { return s.pool.openCount() }

// ResetConnPeak starts a new ConnPeak from the pooled connections holding a
// slot now.
func (s *Store) ResetConnPeak() { s.pool.resetConnPeak() }

// ConnPeak returns the most pooled connections that held a slot on the
// server at once since the last ResetConnPeak, or since the store was made,
// as OpenConns counts them. Connections that have closed since still count,
// so a connection-limit probe that adds it to its own count cannot read the
// limit low because the account's connections came and went while it dialed.
func (s *Store) ConnPeak() int { return s.pool.connPeak() }

// PoolSize returns the most connections the store opens at once:
// Options.PoolSize, less one for each time the server refused a connection
// for being over its limit.
func (s *Store) PoolSize() int { return s.pool.size() }

// borrow takes a connection from the pool, preferring one with folder open,
// read-write when writable. Every borrow is paired with a release.
func (s *Store) borrow(ctx context.Context, folder string, writable bool) (*conn, error) {
	c, err := s.pool.get(ctx, folder, writable)
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

// errConnLimit marks a server refusing a connection for being over its limit.
var errConnLimit = errors.New("over the server's connection limit")

// dial opens a pooled connection. A refusal for being over the server's
// connection limit shrinks the pool and tells OnConnLimit.
func (s *Store) dial(ctx context.Context) (*conn, error) {
	c, err := s.connect(ctx)
	if errors.Is(err, errConnLimit) {
		n := s.pool.shrink()
		s.opts.Logger.Warn("server refused another connection; pool shrunk", "account", s.acct.Name, "pool", n)
		if s.opts.OnConnLimit != nil {
			s.opts.OnConnLimit(n)
		}
	}
	return c, err
}

// ProbeDial connects and logs in outside the pool, checks the connection
// with NOOP and returns it. The caller closes it, which logs out. The
// connection-limit probe uses it, so a refusal for being over the limit
// leaves the pool as it is.
func (s *Store) ProbeDial(ctx context.Context) (io.Closer, error) {
	c, err := s.connect(ctx)
	if err != nil {
		if errors.Is(err, errConnLost) {
			return nil, fmt.Errorf("%w: %w", mailstore.ErrUnavailable, err)
		}
		return nil, err
	}
	if err := c.Noop(ctx); err != nil {
		_ = c.Close() // the NOOP failure is the one to report
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s did not answer NOOP: %w", mailstore.ErrUnavailable, s.acct.Name, err)
	}
	return c, nil
}

// connect logs in on a fresh connection and sorts what went wrong.
func (s *Store) connect(ctx context.Context) (*conn, error) {
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
	sess, err := Dial(ctx, s.acct.IMAP, creds, DialOptions{RootCAs: s.opts.RootCAs, Timeout: s.dialTimeout})
	switch {
	case err == nil:
		return newConn(sess, s.opts.CommandTimeout, s.opts.Now), nil
	case errors.Is(err, auth.ErrRejected):
		s.creds.Rejected(s.acct.Name, creds)
		return nil, &auth.RejectedError{Hint: rejectedHint(s.acct), Err: err}
	case isConnLimit(err):
		return nil, fmt.Errorf("%w: %w: %s refused another connection: %w", mailstore.ErrUnavailable, errConnLimit, s.acct.Name, err)
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case isRefusal(err):
		// The server answered, so trying again would only resend the
		// credentials: NO [ALERT] for an app password, BAD for
		// LOGINDISABLED and the like. NO [UNAVAILABLE] and NO [INUSE] are
		// temporary (RFC 5530), but the account is reported unavailable
		// for the caller to try later rather than retried here at once.
		return nil, fmt.Errorf("%w: %s refused the login: %w", mailstore.ErrUnavailable, s.acct.Name, err)
	case errors.Is(err, errLogin):
		// The credentials went out, so a fresh connection would only send
		// them again: to a server that hung up on them, maybe one counting
		// failures towards a lockout. That holds for a BYE, a dropped
		// connection and a timeout alike.
		return nil, fmt.Errorf("%w: %s failed while logging in: %w", mailstore.ErrUnavailable, s.acct.Name, err)
	case errors.Is(err, context.DeadlineExceeded):
		// The dial's own timeout, since ctx is still live: as slow as a
		// command that timed out, and retried as sparingly.
		return nil, fmt.Errorf("%w: %w: %w", errConnLost, errCommandTimeout, err)
	default:
		// A transport or TLS failure, or a BYE, before the credentials went
		// out: worth a fresh connection.
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

// isConnLimit reports a server refusing a connection for being over its
// limit: code LIMIT, or code ALERT with text saying so, as Gmail sends. A
// code-less NO is never one, whatever its text: that rejects the
// credentials.
func isConnLimit(err error) bool {
	var e *imap.Error
	if !errors.As(err, &e) {
		return false
	}
	if e.Code == imap.ResponseCodeLimit {
		return true
	}
	if e.Code != imap.ResponseCodeAlert {
		return false
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
		s.opts.Logger.Debug("retrying after a dropped connection", logAttrs(err, "account", s.acct.Name, "attempt", attempt+1)...)
		if err := sleep(ctx, s.opts.Backoff(attempt)); err != nil {
			return err
		}
	}
}

func (s *Store) tryConn(ctx context.Context, folder string, f func(*conn) error) error {
	return s.tryConnMode(ctx, folder, false, f)
}

func (s *Store) tryConnMode(ctx context.Context, folder string, writable bool, f func(*conn) error) error {
	c, err := s.borrow(ctx, folder, writable)
	if err != nil {
		return err
	}
	defer s.release(c)
	return f(c)
}

// open opens folder on c for reading: read-only, unless it is already open
// read-write. A folder that turns out to be missing drops the cached folder
// list, so the next search lists folders again.
func (s *Store) open(ctx context.Context, c *conn, folder string) error {
	return s.openMode(ctx, c, folder, false)
}

// openMode is open, read-write when writable. A connection that opened its
// folder before a rename or delete opens it again, since a server keeps the
// old mailbox open under its new name, or none. One that reuses its open
// folder after a write through this store opens it again, so it sees what
// the write changed there. Otherwise it sends NOOP first, so it sees what
// changed elsewhere, unless the server told it of the folder within
// freshFor.
func (s *Store) openMode(ctx context.Context, c *conn, folder string, writable bool) error {
	if gen := s.folderGen.Load(); c.folderGen != gen {
		c.forget()
		c.folderGen = gen
	}
	writes := s.writeGen.Load()
	reuse := folder != "" && c.selected == folder && (c.writable || !writable)
	switch {
	case reuse && c.writeGen != writes:
		// Another of this store's connections may have changed mail here,
		// or moved a message in. Gmail tells a session that kept the folder
		// open of a message moved in only some time after it answers a
		// NOOP, while a SELECT or EXAMINE shows it at once. A read takes
		// either mode, so a folder open read-write stays so.
		writable = writable || c.writable
		c.forget()
	case reuse && c.stale():
		// New mail or another mail client may have changed mail here. A
		// server tells a session of new mail only in answer to a command,
		// and Gmail and Dovecot answer a UID FETCH, STORE or SEARCH only for
		// messages they have told the session of.
		if err := c.Noop(ctx); err != nil {
			return err
		}
	}
	err := c.SelectFolder(ctx, folder, writable)
	switch {
	case err == nil:
		c.writeGen = writes
	case errors.Is(err, mailstore.ErrNotFound):
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
