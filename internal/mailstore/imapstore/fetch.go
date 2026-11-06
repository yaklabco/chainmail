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

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
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

// errUnfound is the ErrStaleRef resolveUID returns when Gmail finds no
// message with the ref's X-GM-MSGID in the open folder. A read on a reused
// folder takes it as a message the server left out, since Gmail may not yet
// have told the connection of the message, and looks again after opening
// the folder afresh.
var errUnfound = fmt.Errorf("%w: the message is gone; search again", mailstore.ErrStaleRef)

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
		return 0, errUnfound
	}
	return uid, nil
}

// fetchRef opens r's folder on c and fetches items of the message r names,
// as readIn does. It returns the message's UID there and the answer, or
// ErrStaleRef when the message is gone. A message Gmail cannot find by
// X-GM-MSGID counts as left out, as one the FETCH leaves out does.
func (s *Store) fetchRef(ctx context.Context, c *conn, r ref.Ref, items *imap.FetchOptions) (uint32, *imapclient.FetchMessageBuffer, error) {
	var uid uint32
	var bufs []*imapclient.FetchMessageBuffer
	var unfound error
	err := s.readIn(ctx, c, r.Folder, func() (bool, error) {
		var err error
		bufs, unfound = nil, nil
		uid, err = s.resolveUID(ctx, c, r)
		switch {
		case errors.Is(err, errUnfound):
			unfound = err
			return true, nil
		case err != nil:
			return false, err
		}
		bufs, err = c.fetch(ctx, imap.UIDSetNum(imap.UID(uid)), items)
		return len(bufs) == 0, err
	})
	switch {
	case err != nil:
		return 0, nil, err
	case unfound != nil:
		return 0, nil, unfound
	case len(bufs) == 0:
		return 0, nil, fmt.Errorf("%w: the message is gone; search again", mailstore.ErrStaleRef)
	}
	return uid, bufs[0], nil
}

// resolveRefs adds the UID of each ref refs[idx[j]] names in the folder
// open on c to want, mapped to j, for each j of js: a batch may name one
// message twice. A stale ref fails in out on its own, and a ref succeeding
// now clears the failure an earlier run left in out. It returns the js of
// the refs Gmail found no message for by X-GM-MSGID, which fail in out
// with errUnfound.
func (s *Store) resolveRefs(ctx context.Context, c *conn, refs []ref.Ref, idx, js []int, want map[uint32][]int, out []mailstore.MessageResult) ([]int, error) {
	var unfound []int
	for _, j := range js {
		uid, err := s.resolveUID(ctx, c, refs[idx[j]])
		out[j].Err = nil
		switch {
		case errors.Is(err, errUnfound):
			unfound = append(unfound, j)
			fallthrough
		case errors.Is(err, mailstore.ErrStaleRef):
			out[j].Err = err
			continue
		case err != nil:
			return nil, err
		}
		want[uid] = append(want[uid], j)
	}
	return unfound, nil
}

// missingUIDs returns the UIDs of want that got holds no answer for, in order.
func missingUIDs(want map[uint32][]int, got map[uint32]*imapclient.FetchMessageBuffer) []uint32 {
	var out []uint32
	for uid := range want {
		if got[uid] == nil {
			out = append(out, uid)
		}
	}
	slices.Sort(out)
	return out
}

func (s *Store) fetchGroup(ctx context.Context, c *conn, folder string, refs []ref.Ref, idx []int, out []mailstore.MessageResult) error {
	// want maps each UID to the refs that name it, and got each UID the
	// server answered for to its answer. unfound holds the refs Gmail found
	// no message for by X-GM-MSGID. A second run, after readIn opened the
	// folder again, looks those up once more and asks only for the UIDs
	// left out, unless the folder now has another UIDVALIDITY and every ref
	// is resolved again.
	var want map[uint32][]int
	var got map[uint32]*imapclient.FetchMessageBuffer
	var unfound []int
	var validity uint32
	err := s.readIn(ctx, c, folder, func() (bool, error) {
		pending := unfound
		if want == nil || c.uidValidity != validity {
			clear(out)
			want, got, validity = map[uint32][]int{}, map[uint32]*imapclient.FetchMessageBuffer{}, c.uidValidity
			pending = make([]int, len(idx))
			for j := range pending {
				pending[j] = j
			}
		}
		var err error
		if unfound, err = s.resolveRefs(ctx, c, refs, idx, pending, want, out); err != nil {
			return false, err
		}
		if missing := missingUIDs(want, got); len(missing) > 0 {
			bufs, err := c.fetch(ctx, uidSet(missing), s.summaryItems(false, true))
			if err != nil {
				return false, asDownload(err)
			}
			for _, b := range bufs {
				got[uint32(b.UID)] = b
			}
		}
		return len(unfound) > 0 || len(missingUIDs(want, got)) > 0, nil
	})
	if err != nil || len(want) == 0 {
		return err
	}
	uids := slices.Sorted(maps.Keys(want))
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
			js      []int
			b       *imapclient.FetchMessageBuffer
			fetched bool // whether a FETCH for its text parts went out
		}
		// set fills the result of every ref naming the message.
		set := func(p pending, msg *mailstore.Message, err error) {
			for _, j := range p.js {
				out[j] = mailstore.MessageResult{Message: msg, Err: err}
			}
		}
		var ps []pending
		col := newCollector(len(uids), progress)
		for _, uid := range uids {
			b := got[uid]
			if b == nil {
				continue
			}
			p := pending{js: want[uid], b: b}
			if secs := textSections(b.BodyStructure); len(secs) > 0 {
				col.add(len(ps), c.client().Fetch(imap.UIDSetNum(b.UID), &imap.FetchOptions{UID: true, BodySection: secs}))
				p.fetched = true
			}
			ps = append(ps, p)
		}
		replies, errs := col.wait()
		for i, p := range ps {
			var parts *imapclient.FetchMessageBuffer
			if p.fetched {
				var refusal *imap.Error
				switch err := asDownload(errs[i]); {
				case s.throttled(err) != nil:
					// A quota or bandwidth limit is about the account,
					// not this message.
					set(p, nil, s.throttled(err))
					continue
				case errors.As(err, &refusal):
					// The server refused this one message, so it alone
					// fails, as unreadable rather than unavailable: the
					// connection still serves the others, and a thread
					// leaves the message out instead of failing. The error
					// matches no mailstore sentinel.
					set(p, nil, fmt.Errorf("the server refused to read the text: %w", err))
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

// collector reads pipelined FETCH commands, each on its own goroutine
// started as soon as the command is sent, and calls progress as each one
// completes.
//
// Reading them one after another deadlocks. A server may send a later
// command's data before it completes an earlier one, as Dovecot does when it
// runs pipelined commands in parallel. imapclient's reader then waits for
// the later command to take its literal, while the caller waits for the
// earlier command to complete, which the reader never gets to. Reading only
// once every command is sent risks the same stall on a long batch: the
// reader parks at the first literal, the server stops reading while its
// output backs up, and the next send blocks.
type collector struct {
	progress func()
	wg       sync.WaitGroup
	replies  [][]*imapclient.FetchMessageBuffer
	errs     []error
}

// newCollector makes room for n commands.
func newCollector(n int, progress func()) *collector {
	return &collector{progress: progress, replies: make([][]*imapclient.FetchMessageBuffer, n), errs: make([]error, n)}
}

// add starts reading cmd as command i.
func (k *collector) add(i int, cmd *imapclient.FetchCommand) {
	k.wg.Go(func() {
		k.replies[i], k.errs[i] = cmd.Collect()
		k.progress()
	})
}

// wait returns each command's replies and error once all are read. A slot
// no command was added to holds nil and nil.
func (k *collector) wait() ([][]*imapclient.FetchMessageBuffer, []error) {
	k.wg.Wait()
	return k.replies, k.errs
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
