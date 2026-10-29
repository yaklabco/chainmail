package imapstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// conn is one pooled session and the folder it has open read-only. One
// goroutine uses it at a time.
type conn struct {
	sess        *Session
	timeout     time.Duration
	now         func() time.Time
	selected    string
	uidValidity uint32
	dead        atomic.Bool
}

func newConn(sess *Session, timeout time.Duration, now func() time.Time) *conn {
	return &conn{sess: sess, timeout: timeout, now: now}
}

func (c *conn) client() *imapclient.Client { return c.sess.client }

// Selected returns the folder open on c, or "".
func (c *conn) Selected() string { return c.selected }

// Dead reports whether c can no longer be used.
func (c *conn) Dead() bool { return c.dead.Load() }

// Close logs out, bounded by the session's close timeout.
func (c *conn) Close() error { return c.sess.Close() }

// Noop checks that an idle connection still answers.
func (c *conn) Noop(ctx context.Context) error {
	return c.do(ctx, func() error { return c.client().Noop().Wait() })
}

// do runs one IMAP exchange, bounded by the command timeout and by ctx.
// When either ends first, the connection is shut, which unblocks f, and c
// is dead. A reply from the server, such as NO, leaves c usable; any other
// failure means the connection broke.
func (c *conn) do(ctx context.Context, f func() error) error {
	if c.Dead() {
		return errConnLost
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.AfterFunc(c.timeout, c.kill)
	stop := context.AfterFunc(ctx, c.kill)
	err := f()
	// A false Stop means the kill has started, or is about to, even when f
	// already finished, so c is dead from here on either way.
	timerFired := !timer.Stop()
	if !stop() {
		c.dead.Store(true)
		return ctx.Err()
	}
	if timerFired {
		c.dead.Store(true)
		return fmt.Errorf("%w: no answer within %v", errConnLost, c.timeout)
	}
	var imapErr *imap.Error
	if err != nil && (!errors.As(err, &imapErr) || imapErr.Type == imap.StatusResponseTypeBye) {
		c.kill()
		return fmt.Errorf("%w: %w", errConnLost, err)
	}
	return err
}

func (c *conn) kill() {
	c.dead.Store(true)
	_ = c.sess.conn.Close() // shutting the socket is the point; its error says nothing new
}

// examine opens folder read-only, unless it is already open.
func (c *conn) examine(ctx context.Context, folder string) error {
	if folder != "" && c.selected == folder {
		return nil
	}
	var data *imap.SelectData
	err := c.do(ctx, func() error {
		var err error
		data, err = c.client().Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
		return err
	})
	if err != nil {
		// A failed EXAMINE leaves no folder open.
		c.selected, c.uidValidity = "", 0
		if missingFolder(err) {
			return fmt.Errorf("%w: no folder %q", mailstore.ErrNotFound, folder)
		}
		return err
	}
	c.selected, c.uidValidity = folder, data.UIDValidity
	return nil
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
