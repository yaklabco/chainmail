package imapstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// errConnLost marks a failure that broke the connection: a drop, a BYE, or
// a command that ran past its timeout. Store operations retry these on a
// fresh connection.
var errConnLost = errors.New("connection to the mail server lost")

// conn is one pooled session, the folder it has open and whether that
// folder is open read-write. One goroutine uses it at a time.
type conn struct {
	sess        *Session
	timeout     time.Duration
	now         func() time.Time
	selected    string
	writable    bool
	uidValidity uint32
	// uidNext and highestModSeq are what the last SELECT or EXAMINE
	// reported. highestModSeq is 0 unless it asked for CONDSTORE and the
	// folder keeps mod-sequences.
	uidNext       uint32
	highestModSeq uint64
	// folderGen is the Store.folderGen this connection last opened a
	// folder under. A rename or delete since then means the folder open
	// here may no longer be the one its name now stands for.
	folderGen uint64
	// writeGen is the Store.writeGen as of this connection's last SELECT,
	// EXAMINE or NOOP.
	writeGen uint64
	// synced is when this connection sent its last SELECT, EXAMINE or NOOP
	// that was answered, so it knows of every message its folder held then.
	synced time.Time
	dead   atomic.Bool
}

func newConn(sess *Session, timeout time.Duration, now func() time.Time) *conn {
	return &conn{sess: sess, timeout: timeout, now: now}
}

func (c *conn) client() *imapclient.Client { return c.sess.client }

// Selected returns the folder open on c, or "".
func (c *conn) Selected() string { return c.selected }

// Writable reports whether the folder open on c was opened read-write, with SELECT.
func (c *conn) Writable() bool { return c.writable }

// Dead reports whether c can no longer be used.
func (c *conn) Dead() bool { return c.dead.Load() }

// Close logs out, bounded by the session's close timeout.
func (c *conn) Close() error { return c.sess.Close() }

// Noop checks that an idle connection still answers. The answer also tells
// it of changes to its folder.
func (c *conn) Noop(ctx context.Context) error {
	sent := c.now()
	err := c.do(ctx, func() error { return c.client().Noop().Wait() })
	if err == nil {
		c.synced = sent
	}
	return err
}

// stale reports whether the server last told c of its folder more than
// freshFor ago.
func (c *conn) stale() bool { return c.now().Sub(c.synced) > freshFor }

// errCommandTimeout marks a command the server did not answer within the
// command timeout. It always comes wrapped with errConnLost, since the
// connection is shut, but a timeout is retried at most once: a server too
// slow for the bound stays too slow, and every retry costs another timeout.
var errCommandTimeout = errors.New("the mail server stopped answering")

// do runs one IMAP exchange, bounded by the command timeout and by ctx.
// When either ends first, the connection is shut, which unblocks f, and c
// is dead. A reply from the server, such as NO, leaves c usable; any other
// failure means the connection broke.
func (c *conn) do(ctx context.Context, f func() error) error {
	return c.pipeline(ctx, func(func()) error { return f() })
}

// pipeline is do for several commands in flight at once. f calls progress
// each time one of them completes, which gives the rest a fresh command
// timeout, so the timeout bounds each command and not the whole batch.
//
// Shutting the connection releases f only while every command f has sent
// is being read. imapclient's reader waits at each literal until the command
// it belongs to takes it, and that wait does not end when the socket closes.
// So f must read pipelined FETCHes concurrently, as collector does, never
// one after another.
func (c *conn) pipeline(ctx context.Context, f func(progress func()) error) error {
	if c.Dead() {
		return errConnLost
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w := startWatchdog(c.timeout, c.kill)
	stop := context.AfterFunc(ctx, c.kill)
	err := f(w.reset)
	timedOut := w.stop()
	if !stop() {
		// The kill has started, or is about to, even if f already finished.
		c.dead.Store(true)
		return ctx.Err()
	}
	if timedOut {
		c.dead.Store(true)
		return fmt.Errorf("%w: %w within %v", errConnLost, errCommandTimeout, c.timeout)
	}
	var imapErr *imap.Error
	if err != nil && (!errors.As(err, &imapErr) || imapErr.Type == imap.StatusResponseTypeBye) {
		c.kill()
		return fmt.Errorf("%w: %w", errConnLost, err)
	}
	return err
}

// watchdog calls fire once d passes without a reset.
type watchdog struct {
	d     time.Duration
	mu    sync.Mutex
	timer *time.Timer
	// deadline is when fire is due. A run of the timer that a reset
	// overtook finds it in the future and leaves the firing to the run the
	// reset scheduled.
	deadline time.Time
	// fired is set before fire runs, and stopped by stop. Once either is
	// set, reset does nothing.
	fired, stopped bool
}

func startWatchdog(d time.Duration, fire func()) *watchdog {
	w := &watchdog{d: d}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = time.Now().Add(d)
	w.timer = time.AfterFunc(d, func() {
		w.mu.Lock()
		if w.stopped || w.fired || time.Now().Before(w.deadline) {
			w.mu.Unlock()
			return
		}
		w.fired = true
		w.mu.Unlock()
		fire()
	})
	return w
}

// reset starts the wait again.
func (w *watchdog) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.fired && !w.stopped {
		w.deadline = time.Now().Add(w.d)
		w.timer.Reset(w.d)
	}
}

// stop disarms w and reports whether it fired.
func (w *watchdog) stop() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	w.timer.Stop()
	return w.fired
}

func (c *conn) kill() {
	c.dead.Store(true)
	_ = c.sess.conn.Close() // shutting the socket is the point; its error says nothing new
}

// forget makes the next SelectFolder open its folder again, even if that is
// the folder open now.
func (c *conn) forget() {
	c.selected, c.writable, c.uidValidity, c.uidNext, c.highestModSeq = "", false, 0, 0, 0
}

// holds reports whether c has folder open in a mode that will do:
// read-write when writable, either mode otherwise.
func (c *conn) holds(folder string, writable bool) bool {
	return folder != "" && c.selected == folder && (c.writable || !writable)
}

// SelectFolder opens folder, read-write with SELECT when writable and
// read-only with EXAMINE otherwise, unless it is already open in a mode
// that will do. A read accepts either mode; a write needs read-write, so a
// folder open read-only is opened again.
func (c *conn) SelectFolder(ctx context.Context, folder string, writable bool) error {
	if c.holds(folder, writable) {
		return nil
	}
	_, err := c.selectFolder(ctx, folder, writable, false)
	return err
}

// selectFolder sends SELECT, or EXAMINE unless writable, with (CONDSTORE)
// when condStore, even if folder is open already, and returns the answer.
// It keeps what the answer reports on c.
func (c *conn) selectFolder(ctx context.Context, folder string, writable, condStore bool) (*imap.SelectData, error) {
	var data *imap.SelectData
	sent := c.now()
	err := c.do(ctx, func() error {
		var err error
		data, err = c.client().Select(folder, &imap.SelectOptions{ReadOnly: !writable, CondStore: condStore}).Wait()
		return err
	})
	if err != nil {
		// A failed SELECT or EXAMINE leaves no folder open.
		c.forget()
		if missingFolder(err) {
			return nil, fmt.Errorf("%w: no folder %q", mailstore.ErrNotFound, folder)
		}
		return nil, err
	}
	c.selected, c.writable, c.uidValidity, c.synced = folder, writable, data.UIDValidity, sent
	c.uidNext, c.highestModSeq = uint32(data.UIDNext), data.HighestModSeq
	return data, nil
}

// missingFolder reports whether err is a server saying a folder does not exist.
func missingFolder(err error) bool {
	var e *imap.Error
	if !errors.As(err, &e) || e.Type != imap.StatusResponseTypeNo {
		return false
	}
	if e.Code == imap.ResponseCodeNonExistent || e.Code == imap.ResponseCodeTryCreate {
		return true
	}
	text := strings.ToLower(e.Text)
	return strings.Contains(text, "exist") || strings.Contains(text, "unknown mailbox") || strings.Contains(text, "no such")
}
