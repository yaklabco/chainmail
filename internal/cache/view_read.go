package cache

import (
	"context"
	"errors"
	"slices"
	"time"

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
//
// On an account other than Google, a miss the server calls stale is
// looked up by its Message-ID and, when exactly one other cached message
// has it, read at that message's ref, which the result carries (spec 7.3).
func (v *View) FetchMessagesFrom(ctx context.Context, refs []ref.Ref) ([]mailstore.MessageResult, []string) {
	return v.read(ctx, refs, true)
}

// read is FetchMessagesFrom. With recovering set, a miss the server calls
// stale is looked up by Message-ID (recoverRef) and, when found, read at
// its new ref, once: that second read does not recover again.
func (v *View) read(ctx context.Context, refs []ref.Ref, recovering bool) ([]mailstore.MessageResult, []string) {
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
	guard := v.a.guardReads()
	defer v.a.endGuard(guard)
	epoch := v.a.writeEpoch()
	for j, res := range v.server.FetchMessages(ctx, missed) {
		i := misses[j]
		out[i], sources[i] = res, SourceServer
		switch {
		case res.Err == nil:
			v.keepRead(ctx, epoch, guard, res.Message)
		case recovering && errors.Is(res.Err, mailstore.ErrStaleRef):
			moved, ok := v.recoverRef(ctx, refs[i])
			if !ok {
				continue
			}
			again, from := v.read(ctx, []ref.Ref{moved}, false)
			if again[0].Err == nil {
				out[i], sources[i] = again[0], from[0]
			}
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

// readFresh reports whether folder is fresh, so its cached flags stand. A
// failed check is logged and reads as stale, so the server's flags are
// fetched.
func (v *View) readFresh(ctx context.Context, folder string) bool {
	ok, _, _, err := v.a.Freshness(ctx, []string{folder})
	if err != nil {
		v.logReadError("checking a folder's freshness failed", err)
		return false
	}
	return ok
}

// refreshFlags replaces the flags and labels of hits, all in src, with the
// server's, from one FetchSummaries, and writes them to their rows
// (storeFlags). It returns the hits the server answered for and the indices
// of the rest, which the caller reads as misses: gone, moved, or unreadable
// now.
func (v *View) refreshFlags(ctx context.Context, src mailstore.Source, hits []readHit) (kept []readHit, missed []int) {
	uids := make([]uint32, len(hits))
	for j, h := range hits {
		uids[j] = h.msg.Summary.Ref.UID
	}
	guard := v.a.guardReads()
	defer v.a.endGuard(guard)
	epoch := v.a.writeEpoch()
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
		kept = append(kept, h)
	}
	v.storeFlags(ctx, epoch, guard, kept)
	return kept, missed
}

// storeFlags writes the server's flags and labels of hits, fetched under
// the account's write epoch epoch while guard ran, to their rows. It
// writes under wmu, as the builder does, and nothing when a folder drop or
// rebuild ran since epoch; a row whose message the builder changed since
// guard started keeps the builder's flags, which are no older than the
// fetched ones. It runs on ctx without its cancellation, bounded by
// keepTimeout, as keepRead does, and when that ends before wmu is free it
// logs that and writes nothing.
func (v *View) storeFlags(ctx context.Context, epoch uint64, guard *readGuard, hits []readHit) {
	if len(hits) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.a.keepTimeout)
	defer cancel()
	if err := v.a.lockContext(ctx); err != nil {
		v.logReadError("waiting to store refreshed flags failed", err)
		return
	}
	defer v.a.unlock()
	if v.a.epoch.Load() != epoch {
		return
	}
	for _, h := range hits {
		sum := h.msg.Summary
		if v.a.changedSince(guard, sum.Ref) {
			continue
		}
		if _, err := v.a.o.Store.SetFlags(ctx, sum.Ref, sum.Flags, sum.Labels); err != nil {
			v.logReadError("updating cached flags failed", err)
		}
	}
}

// keepReadTimeout bounds keepRead's bookkeeping, which runs detached from
// the caller's cancellation so a client that hangs up after the download
// still has its body charged and kept.
const keepReadTimeout = 5 * time.Second

// keepRead does the bookkeeping of a message of the view's account read
// from the server, fetched under the account's write epoch epoch while
// guard ran. It first counts len(Raw) against the account's budget for the
// UTC day, as every foreground download counts whether or not the cache
// keeps it (spec 4.4). Then storeRead keeps the row and body, marked read,
// and when it stored a new body the file's size is checked, which wakes
// the evictor when it is over max_size. It runs on ctx without its
// cancellation, bounded by keepReadTimeout. A message of another account is
// left alone.
func (v *View) keepRead(ctx context.Context, epoch uint64, guard *readGuard, msg *mailstore.Message) {
	store, account := v.a.o.Store, v.a.o.Account.Name
	if msg.Summary.Ref.Account != account {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.a.keepTimeout)
	defer cancel()
	day := v.a.o.Clock.Now().UTC().Format("2006-01-02")
	if _, err := store.AddBudget(ctx, account, day, int64(len(msg.Raw))); err != nil {
		v.logReadError("counting a read against the budget failed", err)
	}
	if v.a.beforeKeepWrite != nil {
		v.a.beforeKeepWrite()
	}
	if !v.storeRead(ctx, epoch, guard, msg) {
		return
	}
	if err := store.noteWrite(ctx, v.a.o.MaxSize()); err != nil {
		v.logReadError("checking the cache size failed", err)
	}
}

// storeRead stores msg's row, when the cache tracks its folder under the
// message's UIDVALIDITY and lacks the row, and its body, marked read. It
// writes under wmu, as the builder and backfill do, and only when no folder
// drop or rebuild ran since epoch and the folder's progress still admits the
// message, so a read never writes into a newer generation of the cache. A
// row the cache lacks is not made when the builder changed the message
// since guard started: an expunge must not be undone by a download that
// predates it. A body is stored only on a row the cache holds, and never
// changes for a UID. A row the cache holds without its body structure
// gets the attachments of the full read in the body's transaction, and
// keeps its flags and other columns (Store.PutFullBody). When ctx ends
// before wmu is free it logs that and stores nothing; keepRead has already
// charged the download.
// Folders the cache does not track, such as Trash or a Google label folder
// other than All Mail, are never stored. It reports whether it stored a new
// body.
func (v *View) storeRead(ctx context.Context, epoch uint64, guard *readGuard, msg *mailstore.Message) bool {
	store, account := v.a.o.Store, v.a.o.Account.Name
	r := msg.Summary.Ref
	if err := v.a.lockContext(ctx); err != nil {
		v.logReadError("waiting to store a read failed", err)
		return false
	}
	defer v.a.unlock()
	if v.a.epoch.Load() != epoch {
		return false
	}
	p, tracked, err := store.Progress(ctx, account, r.Folder)
	if err != nil {
		v.logReadError("reading cache progress failed", err)
		return false
	}
	if !tracked || p.UIDValidity != r.UIDValidity {
		return false
	}
	_, have, err := store.Row(ctx, r)
	if err != nil {
		v.logReadError("reading a cached row failed", err)
		return false
	}
	if !have {
		if v.a.changedSince(guard, r) {
			return false
		}
		if err := store.PutRows(ctx, []Row{messageRow(msg)}); err != nil {
			v.logReadError("storing a read row failed", err)
			return false
		}
	}
	stored, err := store.PutFullBody(ctx, r, Body{Raw: msg.Raw, TextClipped: msg.TextClipped}, true, fullReadOf(msg))
	if err != nil {
		v.logReadError("storing a read body failed", err)
		return false
	}
	return stored
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
// to the server or skips the write, and hands it to OnError. Cache errors
// are SQLite's and carry no message content.
func (v *View) logReadError(msg string, err error) {
	v.a.o.Logger.Warn(msg, "account", v.a.o.Account.Name, "err", err)
	v.a.noteErr(err)
}

// recoverRef finds where the message r named lives now, on an account
// other than Google: r's Message-ID, from its cached row or from
// ref_recovery after a UIDVALIDITY rebuild, matched against the account's
// cached rows other than r's own. Exactly one match is the answer; none or
// several are not. Google accounts recover by X-GM-MSGID in the store.
func (v *View) recoverRef(ctx context.Context, r ref.Ref) (ref.Ref, bool) {
	store, account := v.a.o.Store, v.a.o.Account
	if account.Provider.Gmail || r.Account != account.Name {
		return ref.Ref{}, false
	}
	id, ok, err := store.MessageIDOf(ctx, r)
	if err != nil {
		v.logReadError("looking up a Message-ID for recovery failed", err)
		return ref.Ref{}, false
	}
	if !ok || id == "" {
		return ref.Ref{}, false
	}
	found, err := store.FindByMessageID(ctx, account.Name, id)
	if err != nil {
		v.logReadError("finding a message by Message-ID failed", err)
		return ref.Ref{}, false
	}
	found = slices.DeleteFunc(found, func(c ref.Ref) bool {
		return c.Folder == r.Folder && c.UIDValidity == r.UIDValidity && c.UID == r.UID
	})
	if len(found) != 1 {
		return ref.Ref{}, false
	}
	return found[0], true
}

// Thread implements mailstore.Store's Thread through the cache, as
// ThreadFrom does, without saying which seed it used or where it came
// from.
func (v *View) Thread(ctx context.Context, r ref.Ref) ([]ref.Ref, error) {
	refs, _, _, err := v.ThreadFrom(ctx, r)
	return refs, err
}

// ThreadFrom returns the thread r belongs to, as the server's Thread
// would, the seed it threaded, and SourceCache or SourceServer for where
// its membership came from. The cache answers when it holds r's row and
// every folder a default search covers is fresh (cachedThread). Otherwise
// the server answers, and on an account other than Google a seed it calls
// stale is recovered by Message-ID once and its thread returned. The seed
// is then the recovered ref, else r, so the caller can tell which member
// is the seed.
func (v *View) ThreadFrom(ctx context.Context, r ref.Ref) (refs []ref.Ref, seed ref.Ref, source string, err error) {
	return v.thread(ctx, r, true)
}

func (v *View) thread(ctx context.Context, r ref.Ref, recovering bool) ([]ref.Ref, ref.Ref, string, error) {
	if refs, ok := v.cachedThread(ctx, r); ok {
		return refs, r, SourceCache, nil
	}
	refs, err := v.server.Thread(ctx, r)
	if err != nil && recovering && errors.Is(err, mailstore.ErrStaleRef) {
		if moved, ok := v.recoverRef(ctx, r); ok {
			return v.thread(ctx, moved, false)
		}
	}
	return refs, r, SourceServer, err
}

// cachedThread answers Thread from the cache when it holds r's row and
// every folder a default search covers is fresh: the cached rows of those
// folders in r's thread (threadRows), plus r itself, as the server's Thread
// scopes and seeds it. A row without a thread is a thread of one. A
// failure to list or check the folders is logged and leaves the answer to
// the server.
func (v *View) cachedThread(ctx context.Context, r ref.Ref) ([]ref.Ref, bool) {
	store, account := v.a.o.Store, v.a.o.Account.Name
	if r.Account != account {
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
	scoper, ok := v.server.(mailstore.Scoper)
	if !ok {
		return nil, false
	}
	folders, err := scoper.SearchFolders(ctx, "")
	if err != nil {
		// The error may carry the server's response text, so only a
		// fixed message is logged.
		v.a.o.Logger.Warn("listing the folders a thread covers failed", "account", account, "reason", reasonCode(err))
		return nil, false
	}
	fresh, _, _, err := v.a.Freshness(ctx, folders)
	if err != nil {
		v.logReadError("checking a thread's folders for freshness failed", err)
		return nil, false
	}
	if !fresh {
		return nil, false
	}
	rows, ok, err := v.threadRows(ctx, row)
	if !ok {
		return []ref.Ref{r}, true
	}
	if err != nil {
		v.logReadError("reading a cached thread failed", err)
		return nil, false
	}
	var refs []ref.Ref
	for _, tr := range rows {
		if slices.Contains(folders, tr.Folder) {
			refs = append(refs, tr.Ref())
		}
	}
	return withThreadSeed(refs, r), true
}

// threadRows returns the cached rows of the thread row belongs to, by the
// server's rule: on Google the rows with its X-GM-THRID (as its thread
// ID); elsewhere the rows naming its root Message-ID (RootThreadRows), as
// a standard server's Thread searches Message-ID, References and
// In-Reply-To for it. The bool is false when row has no thread to look up,
// which makes it a thread of one.
func (v *View) threadRows(ctx context.Context, row Row) ([]Row, bool, error) {
	store, account := v.a.o.Store, v.a.o.Account
	if account.Provider.Gmail {
		if row.ThreadID == "" {
			return nil, false, nil
		}
		rows, err := store.ThreadRows(ctx, account.Name, row.ThreadID)
		return rows, true, err
	}
	root := mailstore.RootMessageID(row.References, row.InReplyTo, row.MessageID)
	if root == "" {
		return nil, false, nil
	}
	rows, err := store.RootThreadRows(ctx, account.Name, root)
	return rows, true, err
}

// withThreadSeed adds seed to refs unless a ref already names the same
// message: the same folder and UID, or on Gmail the same X-GM-MSGID, as the
// server's Thread does.
func withThreadSeed(refs []ref.Ref, seed ref.Ref) []ref.Ref {
	for _, r := range refs {
		if (r.Folder == seed.Folder && r.UID == seed.UID) || (seed.GmailMsgID != 0 && r.GmailMsgID == seed.GmailMsgID) {
			return refs
		}
	}
	return append(refs, seed)
}
