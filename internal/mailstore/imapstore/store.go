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
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/provider"
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

	// nsGate holds one token, which a namespace discovery takes, so a store
	// asks the server once and every other call waits for its answer. It is
	// a channel rather than a mutex so a waiter can give up when its
	// context ends.
	nsGate chan struct{}

	// chunk is how much of a part each FETCH asks for while streaming:
	// chunkSize, unless a test shrinks it.
	chunk int64
	// dialTimeout bounds connecting and logging in: the package's
	// dialTimeout, unless a test shrinks it.
	dialTimeout time.Duration
	// nsWait, when a test sets it, runs as a call starts waiting for
	// another call's namespace discovery to finish.
	nsWait func()
	// after is the store's clock for the waits it runs on its own, such as
	// Idle's limit: time.After, unless a test drives it.
	after func(d time.Duration) <-chan time.Time

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
	// ns is the personal namespace prefix and hierarchy delimiter, once
	// nsKnown. It is kept when the folder list is dropped.
	ns      folderSpace
	nsKnown bool
	system  systemSpace // the system namespace of the cached list
	// borrowed counts connections out of the pool, a stream reader's
	// included. drained, when set, is closed when it reaches zero.
	borrowed int
	drained  chan struct{}
	// watches are the open watch connections, which Close logs out. The
	// pool counts them, and the ones still dialing, as outside connections
	// (holdOutside), for OpenConns and ConnPeak. watchesClosed, set by
	// Close, refuses new ones.
	watches       map[*watchConn]struct{}
	watchesClosed bool
}

var _ mailstore.Store = (*Store)(nil)

// closeWait bounds how long Close waits for borrowed connections to come back.
const closeWait = 5 * time.Second

// New returns a Store for acct. It connects on first use.
func New(acct config.ResolvedAccount, creds Credentialer, opts Options) *Store {
	opts = opts.withDefaults()
	s := &Store{acct: acct, creds: creds, opts: opts, loginGate: make(chan struct{}, 1), nsGate: make(chan struct{}, 1), chunk: chunkSize, dialTimeout: dialTimeout, after: time.After, watches: map[*watchConn]struct{}{}}
	s.pool = newPool(s.dial, poolOptions{size: opts.PoolSize, noopAfter: opts.NoopAfter, idleLogout: opts.IdleLogout, now: opts.Now})
	return s
}

func (s *Store) gmail() bool { return s.acct.Provider.Gmail }

// Close refuses new work and logs out of every connection, pooled and
// watch, in parallel.
//
// Watch connections are closed under their owners: Close never waits for a
// watcher to stop. A watcher syncing or idling on one gets ErrUnavailable
// at once and reconnects, through the store it holds by then, such as the
// one that replaced this after a limits probe.
//
// Call it once every pooled operation has returned and every reader StreamPart
// gave out is closed, as the daemon does by draining its handlers before it
// closes stores. Close still waits up to 5 seconds for connections that are
// out, logging each out as it comes back, and reports any that did not.
func (s *Store) Close() error {
	idle := make(chan error, 1)
	go func() { idle <- s.pool.close() }()
	watched := make(chan error, 1)
	go func() { watched <- s.closeWatches() }()
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
	errs = append(errs, <-idle, <-watched)
	return errors.Join(errs...)
}

// OpenConns returns how many of the store's connections hold a slot on the
// server: pooled ones dialing, borrowed, idle or still logging out, and
// watch connections from the start of OpenWatch until they have logged out.
// A borrowed one, such as a stream reader's, counts until it comes back and
// has logged out, after Close too. ProbeDial's connections do not count.
func (s *Store) OpenConns() int { return s.pool.openCount() }

// ResetConnPeak starts a new ConnPeak from the connections holding a slot
// now, pooled and watch.
func (s *Store) ResetConnPeak() { s.pool.resetConnPeak() }

// ConnPeak returns the most connections, pooled and watch, that held a slot
// on the server at once since the last ResetConnPeak, or since the store was
// made, as OpenConns counts them. Connections that have closed since still
// count, so a connection-limit probe that adds it to its own count cannot
// read the limit low because the account's connections came and went while
// it dialed. Leaving the watch connections out would read it low as well.
func (s *Store) ConnPeak() int { return s.pool.connPeak() }

// PoolSize returns the most connections the store opens at once:
// Options.PoolSize, less one for each time the server refused a connection
// for being over its limit.
func (s *Store) PoolSize() int { return s.pool.size() }

// borrow takes a connection from the pool, preferring one with folder open,
// read-write when writable. On a background context
// (mailstore.IsBackground) it never waits: it takes an idle connection, or
// dials one while the pool has room, only when no foreground call is
// waiting, and otherwise fails at once with mailstore.ErrBusy. Every borrow
// is paired with a release.
func (s *Store) borrow(ctx context.Context, folder string, writable bool) (*conn, error) {
	return s.borrowAs(ctx, folder, writable, mailstore.IsBackground(ctx))
}

// borrowAs is borrow, for background work when background is set, whatever
// ctx says. A background dial the server refuses for being over its
// connection limit shrinks the pool as any refusal does (dial), and is
// ErrBusy to the caller: the account is not down, it has no connection to
// spare.
func (s *Store) borrowAs(ctx context.Context, folder string, writable, background bool) (*conn, error) {
	get := s.pool.get
	if background {
		get = s.pool.tryGet
	}
	c, err := get(ctx, folder, writable)
	if err != nil {
		switch {
		case errors.Is(err, errPoolClosed):
			return nil, fmt.Errorf("%w: %w", mailstore.ErrUnavailable, err)
		case background && errors.Is(err, mailstore.ErrConnLimit):
			return nil, mailstore.ErrBusy
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

// dial opens a pooled connection. A refusal for being over the server's
// connection limit shrinks the pool and tells OnConnLimit.
func (s *Store) dial(ctx context.Context) (*conn, error) {
	c, err := s.connect(ctx)
	if errors.Is(err, mailstore.ErrConnLimit) {
		s.connLimited(s.pool.shrink())
	}
	return c, err
}

// connLimited logs that the server refused a connection for being over
// its limit and the pool's cap is now n, and tells OnConnLimit.
func (s *Store) connLimited(n int) {
	s.opts.Logger.Warn("server refused another connection; pool shrunk", "account", s.acct.Name, "pool", n)
	if s.opts.OnConnLimit != nil {
		s.opts.OnConnLimit(n)
	}
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
func (s *Store) connect(ctx context.Context) (*conn, error) { return s.connectWith(ctx, nil) }

// connectWith is connect, with unilateral, when set, hearing what the server
// reports unprompted. Watch connections pass one.
func (s *Store) connectWith(ctx context.Context, unilateral *imapclient.UnilateralDataHandler) (*conn, error) {
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
	sess, err := Dial(ctx, s.acct.IMAP, creds, DialOptions{RootCAs: s.opts.RootCAs, Timeout: s.dialTimeout, unilateral: unilateral})
	switch {
	case err == nil:
		return newConn(sess, s.opts.CommandTimeout, s.opts.Now), nil
	case errors.Is(err, auth.ErrRejected):
		s.creds.Rejected(s.acct.Name, creds)
		return nil, &auth.RejectedError{Hint: rejectedHint(s.acct), Err: err}
	case isConnLimit(err):
		return nil, fmt.Errorf("%w: %w: %s refused another connection: %w", mailstore.ErrUnavailable, mailstore.ErrConnLimit, s.acct.Name, err)
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
// retried at most timeoutRetries times, and a connection the server closed
// for a quota or bandwidth limit not at all (mailstore.ErrThrottled). Every
// other outcome is final, mailstore.ErrBusy included: background work tries
// again on its own schedule.
func (s *Store) retry(ctx context.Context, try func() error) error {
	timeouts := 0
	for attempt := 0; ; attempt++ {
		err := try()
		if errors.Is(err, mailstore.ErrBusy) {
			return err
		}
		// A server that hung up for a quota or bandwidth limit would only
		// refuse a fresh connection again.
		if t := s.throttled(err); t != nil {
			return t
		}
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
	_, err := s.openFresh(ctx, c, folder, writable)
	return err
}

// openFresh is openMode, and reports whether it sent SELECT or EXAMINE
// rather than reuse the folder open on c.
func (s *Store) openFresh(ctx context.Context, c *conn, folder string, writable bool) (bool, error) {
	if gen := s.folderGen.Load(); c.folderGen != gen {
		c.forget()
		c.folderGen = gen
	}
	writes := s.writeGen.Load()
	reuse := c.holds(folder, writable)
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
			return false, err
		}
	}
	fresh := !c.holds(folder, writable)
	err := c.SelectFolder(ctx, folder, writable)
	switch {
	case err == nil:
		c.writeGen = writes
	case errors.Is(err, mailstore.ErrNotFound):
		s.forgetFolders()
	}
	return fresh, err
}

// readIn opens folder on c for reading and runs read, which reports whether
// the server's answer left out a message read asked for. When it did and c
// reused the folder it had open, readIn opens the folder again, in the same
// mode, and runs read once more, so a message is reported gone only from a
// fresh view of the folder. A server may tell a session that keeps a folder
// open of a message late: Gmail tells of one another session moved there
// only some time after the NOOP that asks, while a SELECT or EXAMINE shows
// it at once. read must be safe to run again.
func (s *Store) readIn(ctx context.Context, c *conn, folder string, read func() (missed bool, err error)) error {
	fresh, err := s.openFresh(ctx, c, folder, false)
	if err != nil {
		return err
	}
	missed, err := read()
	if err != nil || !missed || fresh {
		return err
	}
	writable := c.writable
	c.forget()
	if err := s.openMode(ctx, c, folder, writable); err != nil {
		return err
	}
	_, err = read()
	return err
}

// gmailThrottleText is the lower-cased text Gmail sends, with or without a
// response code, when an account has used up its IMAP command or download
// allowance.
const gmailThrottleText = "exceeded command or bandwidth limits"

// downloadError marks the error of a message download, a FETCH of headers
// or bodies, so isThrottled can tell a download limit from a storage quota
// or an implementation limit on another command. Its text is err's.
type downloadError struct{ err error }

func (e *downloadError) Error() string { return e.err.Error() }

func (e *downloadError) Unwrap() error { return e.err }

// asDownload marks err, from a message download, as downloadError when it
// holds a server reply. Any other error, nil included, is returned as is.
func asDownload(err error) error {
	var e *imap.Error
	if !errors.As(err, &e) {
		return err
	}
	return &downloadError{err: err}
}

// isThrottled reports whether err holds a server reply refusing for a quota
// or bandwidth limit: Gmail's bandwidth text on any command, or code
// OVERQUOTA or LIMIT on a message download (asDownload). On other commands
// those codes are a storage quota (APPEND, COPY, MOVE) or an implementation
// limit (too many results), which a pause until the next day would not
// lift. The reply may be a NO, a BAD or a BYE.
func isThrottled(err error) bool {
	var e *imap.Error
	if !errors.As(err, &e) {
		return false
	}
	if strings.Contains(strings.ToLower(e.Text), gmailThrottleText) {
		return true
	}
	var d *downloadError
	return (e.Code == imap.ResponseCodeOverQuota || e.Code == imap.ResponseCodeLimit) && errors.As(err, &d)
}

// throttled returns mailstore.ErrThrottled for the account when err is a
// throttling reply, and nil otherwise. A connection limit at login and a
// login refusal keep their own meaning, whatever their code or text. The
// error never carries the server's reply or its text.
func (s *Store) throttled(err error) error {
	if !isThrottled(err) || errors.Is(err, mailstore.ErrConnLimit) ||
		errors.Is(err, auth.ErrRejected) || errors.Is(err, auth.ErrAuthRequired) {
		return nil
	}
	return fmt.Errorf("%w: %s", mailstore.ErrThrottled, s.acct.Name)
}

// serverError marks a refusal from the server (NO or BAD) as unavailable,
// or as throttled when it refuses for a quota or bandwidth limit, unless
// it already means something more specific.
func (s *Store) serverError(err error) error {
	if t := s.throttled(err); t != nil {
		return t
	}
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
