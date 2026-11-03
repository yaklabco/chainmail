package cache

import (
	"context"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// FetchMessages implements mailstore.Store's FetchMessages through the
// cache, as FetchMessagesFrom does, without saying where each came from.
func (v *View) FetchMessages(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
	out, _ := v.FetchMessagesFrom(ctx, refs)
	return out
}

// FetchMessagesFrom reads refs as the server's FetchMessages would, and
// says where each result came from, SourceCache or SourceServer, in the
// order of refs.
//
// A ref whose row and body the cache holds is a hit: it is answered from
// the cache and its body marked read. A body never changes for a UID, so a
// cached one is always right; flags and labels can change, so they come
// from the cache only when the ref's folder is fresh, and otherwise one
// FetchSummaries per folder refreshes them, and a ref that call does not
// answer for is read as a miss. A miss is read from the server and kept
// (keepRead).
func (v *View) FetchMessagesFrom(ctx context.Context, refs []ref.Ref) ([]mailstore.MessageResult, []string) {
	return v.read(ctx, refs)
}

// read is FetchMessagesFrom.
func (v *View) read(ctx context.Context, refs []ref.Ref) ([]mailstore.MessageResult, []string) {
	out := make([]mailstore.MessageResult, len(refs))
	sources := make([]string, len(refs))
	misses := v.readHits(ctx, refs, out, sources)
	if len(misses) == 0 {
		return out, sources
	}
	missed := make([]ref.Ref, len(misses))
	for j, i := range misses {
		missed[j] = refs[i]
	}
	for j, res := range v.server.FetchMessages(ctx, missed) {
		i := misses[j]
		out[i], sources[i] = res, SourceServer
		if res.Err == nil {
			v.keepRead(ctx, res.Message)
		}
	}
	return out, sources
}

// readHit is a ref the cache answers: where it sits in the call, and its
// message.
type readHit struct {
	i   int
	msg *mailstore.Message
}

// readHits fills out and sources for each ref the cache answers and returns
// the indices of the rest, in order.
func (v *View) readHits(ctx context.Context, refs []ref.Ref, out []mailstore.MessageResult, sources []string) []int {
	var misses []int
	var order []mailstore.Source
	hits := map[mailstore.Source][]readHit{}
	for i, r := range refs {
		msg, ok := v.cachedMessage(ctx, r)
		if !ok {
			misses = append(misses, i)
			continue
		}
		src := mailstore.Source{Folder: r.Folder, UIDValidity: r.UIDValidity}
		if _, seen := hits[src]; !seen {
			order = append(order, src)
		}
		hits[src] = append(hits[src], readHit{i: i, msg: msg})
	}
	for _, src := range order {
		kept := hits[src]
		if !v.readFresh(ctx, src.Folder) {
			var stale []int
			kept, stale = v.refreshFlags(ctx, src, kept)
			misses = append(misses, stale...)
		}
		for _, h := range kept {
			out[h.i], sources[h.i] = mailstore.MessageResult{Message: h.msg}, SourceCache
			if err := v.a.o.Store.MarkRead(ctx, h.msg.Summary.Ref); err != nil {
				v.logReadError("marking a cached body read failed", err)
			}
		}
	}
	slices.Sort(misses)
	return misses
}

// cachedMessage returns r's message from the cache when it holds both the
// row and the body. A cache error reads as a miss, so a broken cache never
// fails a read the server can answer.
func (v *View) cachedMessage(ctx context.Context, r ref.Ref) (*mailstore.Message, bool) {
	store := v.a.o.Store
	if r.Account != v.a.o.Account.Name {
		return nil, false
	}
	row, ok, err := store.Row(ctx, r)
	if err != nil {
		v.logReadError("reading a cached row failed", err)
		return nil, false
	}
	if !ok {
		return nil, false
	}
	body, ok, err := store.Body(ctx, r)
	if err != nil {
		v.logReadError("reading a cached body failed", err)
		return nil, false
	}
	if !ok {
		return nil, false
	}
	return row.Message(body), true
}

// readFresh reports whether folder is fresh, so its cached flags stand.
func (v *View) readFresh(ctx context.Context, folder string) bool {
	ok, _, _, err := v.a.Freshness(ctx, []string{folder})
	return err == nil && ok
}

// refreshFlags replaces the flags and labels of hits, all in src, with the
// server's, from one FetchSummaries, and writes them to their rows. It
// returns the hits the server answered for and the indices of the rest,
// which the caller reads as misses: gone, moved, or unreadable now.
func (v *View) refreshFlags(ctx context.Context, src mailstore.Source, hits []readHit) (kept []readHit, missed []int) {
	uids := make([]uint32, len(hits))
	for j, h := range hits {
		uids[j] = h.msg.Summary.Ref.UID
	}
	sums, err := v.server.FetchSummaries(ctx, src, uids, mailstore.SummaryOptions{})
	if err != nil {
		for _, h := range hits {
			missed = append(missed, h.i)
		}
		return nil, missed
	}
	byUID := make(map[uint32]mailstore.Summary, len(sums))
	for _, s := range sums {
		byUID[s.Ref.UID] = s
	}
	for _, h := range hits {
		s, ok := byUID[h.msg.Summary.Ref.UID]
		if !ok {
			missed = append(missed, h.i)
			continue
		}
		h.msg.Summary.Flags, h.msg.Summary.Labels = s.Flags, s.Labels
		if _, err := v.a.o.Store.SetFlags(ctx, h.msg.Summary.Ref, s.Flags, s.Labels); err != nil {
			v.logReadError("updating cached flags failed", err)
		}
		kept = append(kept, h)
	}
	return kept, missed
}

// keepRead stores a message read from the server: its row, when the cache
// tracks the folder under the message's UIDVALIDITY and lacks the row, and
// its body, marked read, counted against the account's budget for the UTC
// day, and then the file's size is checked, which wakes the evictor when
// it is over max_size. Folders the cache does not track, such as Trash or a
// Google label folder other than All Mail, are never stored.
func (v *View) keepRead(ctx context.Context, msg *mailstore.Message) {
	store, account := v.a.o.Store, v.a.o.Account.Name
	r := msg.Summary.Ref
	p, tracked, err := store.Progress(ctx, account, r.Folder)
	if err != nil {
		v.logReadError("reading cache progress failed", err)
		return
	}
	if !tracked || p.UIDValidity != r.UIDValidity {
		return
	}
	_, have, err := store.Row(ctx, r)
	if err != nil {
		v.logReadError("reading a cached row failed", err)
		return
	}
	if !have {
		if err := store.PutRows(ctx, []Row{messageRow(msg)}); err != nil {
			v.logReadError("storing a read row failed", err)
			return
		}
	}
	stored, err := store.PutBody(ctx, r, Body{Raw: msg.Raw, TextClipped: msg.TextClipped}, true)
	if err != nil {
		v.logReadError("storing a read body failed", err)
		return
	}
	if !stored {
		return
	}
	day := v.a.o.Clock.Now().UTC().Format("2006-01-02")
	if _, err := store.AddBudget(ctx, account, day, int64(len(msg.Raw))); err != nil {
		v.logReadError("counting a read against the budget failed", err)
	}
	if err := store.noteWrite(ctx, v.a.o.MaxSize()); err != nil {
		v.logReadError("checking the cache size failed", err)
	}
}

// messageRow is the cache row of a message read from the server. A Message
// carries no Bcc, so such a row has none until the folder's headers are
// fetched again.
func messageRow(msg *mailstore.Message) Row {
	sum := msg.Summary
	sum.Preview, sum.Sample = nil, nil
	return RowFromHeader(mailstore.Header{
		Summary: sum, Cc: msg.Cc, MessageID: msg.MessageID,
		InReplyTo: msg.InReplyTo, References: msg.References, Attachments: msg.Attachments,
	})
}

// logReadError logs a cache failure on the read path, which then falls back
// to the server or skips the write. Cache errors are SQLite's and carry no
// message content.
func (v *View) logReadError(msg string, err error) {
	v.a.o.Logger.Warn(msg, "account", v.a.o.Account.Name, "err", err)
}
