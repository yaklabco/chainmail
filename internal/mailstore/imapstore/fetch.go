package imapstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// FetchMessages implements mailstore.Store. Refs are grouped by folder, so
// each folder is opened once, and folders are read in parallel.
func (s *Store) FetchMessages(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
	results := make([]mailstore.MessageResult, len(refs))
	groups := map[string][]int{}
	var order []string
	for i, r := range refs {
		if r.Account != s.acct.Name {
			results[i].Err = fmt.Errorf("%w: the ref is for account %s", mailstore.ErrNotFound, r.Account)
			continue
		}
		if _, ok := groups[r.Folder]; !ok {
			order = append(order, r.Folder)
		}
		groups[r.Folder] = append(groups[r.Folder], i)
	}
	var wg sync.WaitGroup
	for _, folder := range order {
		idx := groups[folder]
		wg.Go(func() {
			part := make([]mailstore.MessageResult, len(idx))
			err := s.withConn(ctx, folder, func(c *conn) error {
				clear(part)
				return s.fetchGroup(ctx, c, folder, refs, idx, part)
			})
			for j, i := range idx {
				if err != nil && part[j].Message == nil && part[j].Err == nil {
					part[j].Err = err
				}
				results[i] = part[j]
			}
		})
	}
	wg.Wait()
	return results
}

// resolveUID finds r's UID in the folder open on c. A changed UIDVALIDITY
// is recovered by X-GM-MSGID on Gmail, and is stale everywhere else.
func (s *Store) resolveUID(ctx context.Context, c *conn, r ref.Ref) (uint32, error) {
	if r.UIDValidity == c.uidValidity {
		return r.UID, nil
	}
	if !s.gmail() || r.GmailMsgID == 0 {
		return 0, fmt.Errorf("%w: %s changed since this ref was issued; search again", mailstore.ErrStaleRef, r.Folder)
	}
	uid, err := c.findMsgID(ctx, r.GmailMsgID)
	if err != nil {
		return 0, err
	}
	if uid == 0 {
		return 0, fmt.Errorf("%w: the message is gone; search again", mailstore.ErrStaleRef)
	}
	return uid, nil
}

func (s *Store) fetchGroup(ctx context.Context, c *conn, folder string, refs []ref.Ref, idx []int, out []mailstore.MessageResult) error {
	if err := s.open(ctx, c, folder); err != nil {
		return err
	}
	// want maps each UID to the refs that name it: a batch may name one message twice.
	want := map[uint32][]int{}
	for j, i := range idx {
		uid, err := s.resolveUID(ctx, c, refs[i])
		switch {
		case errors.Is(err, mailstore.ErrStaleRef):
			out[j].Err = err
			continue
		case err != nil:
			return err
		}
		want[uid] = append(want[uid], j)
	}
	if len(want) == 0 {
		return nil
	}
	uids := slices.Sorted(maps.Keys(want))
	bufs, err := c.fetch(ctx, uidSet(uids), s.summaryItems(false, true))
	if err != nil {
		return err
	}
	got := make(map[uint32]*imapclient.FetchMessageBuffer, len(bufs))
	for _, b := range bufs {
		got[uint32(b.UID)] = b
	}
	for uid, js := range want {
		if got[uid] == nil {
			for _, j := range js {
				out[j].Err = fmt.Errorf("%w: the message is gone; search again", mailstore.ErrStaleRef)
			}
		}
	}
	// One FETCH per message for its text parts, all sent at once and read
	// concurrently.
	return c.pipeline(ctx, func(progress func()) error {
		type pending struct {
			js []int
			b  *imapclient.FetchMessageBuffer
		}
		// set fills the result of every ref naming the message.
		set := func(p pending, msg *mailstore.Message, err error) {
			for _, j := range p.js {
				out[j] = mailstore.MessageResult{Message: msg, Err: err}
			}
		}
		var ps []pending
		var cmds []*imapclient.FetchCommand
		for _, uid := range uids {
			b := got[uid]
			if b == nil {
				continue
			}
			var cmd *imapclient.FetchCommand
			if secs := textSections(b.BodyStructure); len(secs) > 0 {
				cmd = c.client().Fetch(imap.UIDSetNum(b.UID), &imap.FetchOptions{UID: true, BodySection: secs})
			}
			ps = append(ps, pending{js: want[uid], b: b})
			cmds = append(cmds, cmd)
		}
		replies, errs := collectAll(cmds, progress)
		for i, p := range ps {
			var parts *imapclient.FetchMessageBuffer
			if cmds[i] != nil {
				var refusal *imap.Error
				switch err := errs[i]; {
				case errors.As(err, &refusal):
					set(p, nil, fmt.Errorf("%w: reading the text: %w", mailstore.ErrUnavailable, err))
					continue
				case err != nil:
					return err
				}
				if len(replies[i]) > 0 {
					parts = replies[i][0]
				}
			}
			msg, err := s.message(folder, c.uidValidity, p.b, parts)
			set(p, msg, err)
		}
		return nil
	})
}

// collectAll reads pipelined FETCH commands, each on its own goroutine, and
// calls progress as each one completes. A nil command is skipped.
//
// Reading them one after another deadlocks. A server may send a later
// command's data before it completes an earlier one, as Dovecot does when it
// runs pipelined commands in parallel. imapclient's reader then waits for
// the later command to take its literal, while the caller waits for the
// earlier command to complete, which the reader never gets to.
func collectAll(cmds []*imapclient.FetchCommand, progress func()) ([][]*imapclient.FetchMessageBuffer, []error) {
	replies := make([][]*imapclient.FetchMessageBuffer, len(cmds))
	errs := make([]error, len(cmds))
	var wg sync.WaitGroup
	for i, cmd := range cmds {
		if cmd == nil {
			continue
		}
		wg.Go(func() {
			replies[i], errs[i] = cmd.Collect()
			progress()
		})
	}
	wg.Wait()
	return replies, errs
}

func (s *Store) message(folder string, uidValidity uint32, b, parts *imapclient.FetchMessageBuffer) (*mailstore.Message, error) {
	header := b.FindBodySection(headerSection)
	lookup := func(sec *imap.FetchItemBodySection) []byte {
		if parts == nil {
			return nil
		}
		return parts.FindBodySection(sec)
	}
	raw, clipped, err := rebuild(header, b.BodyStructure, lookup)
	if err != nil {
		return nil, fmt.Errorf("rebuilding the message: %w", err)
	}
	msg := &mailstore.Message{
		Summary: s.summary(folder, uidValidity, b), References: msgIDList(header, "References"),
		Raw: raw, TextClipped: clipped, Attachments: attachmentsOf(b.BodyStructure),
	}
	if env := b.Envelope; env != nil {
		msg.Cc, msg.MessageID, msg.InReplyTo = addresses(env.Cc), env.MessageID, env.InReplyTo
	}
	return msg, nil
}
