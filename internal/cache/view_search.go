package cache

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/render"
)

// Search answers a search as the server's Search would, from the cache:
// the folders the server searches for req.In, or exactly req.Bounds, each
// with its cached matches, UIDs highest first, in the server's folder
// order, with Source.UIDValidity from the folder's progress.
//
// A folder the cache holds no progress for contributes no result, and a
// query the cache can never answer completely (Plan.ServerOnly) matches
// nothing in every folder, so a forced cache search is incomplete but never
// passes a partial list off as the answer. The daemon routes automatic
// searches here only when neither can happen. A failed read of cache.db
// is ErrRead.
func (v *View) Search(ctx context.Context, req mailstore.SearchRequest) ([]mailstore.SourceResult, error) {
	opts := v.a.o
	plan, err := Compile(req.Query, req.In, opts.Account.Provider.Gmail, opts.Clock.Now(), req.ItemThreads)
	if err != nil {
		return nil, err
	}
	folders, err := v.searchScope(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make([]mailstore.SourceResult, 0, len(folders))
	for _, folder := range folders {
		p, held, err := opts.Store.Progress(ctx, opts.Account.Name, folder)
		if err != nil {
			return nil, readErr(err)
		}
		bound, bounded := req.Bounds[folder]
		if !held {
			if bounded {
				out = append(out, mailstore.SourceResult{
					Source: mailstore.Source{Folder: folder, UIDValidity: bound.UIDValidity},
					Err:    fmt.Errorf("%w: the cache does not hold the folder", mailstore.ErrNotFound),
				})
			}
			continue
		}
		res := mailstore.SourceResult{Source: mailstore.Source{Folder: folder, UIDValidity: p.UIDValidity}}
		switch {
		case bounded && bound.UIDValidity != p.UIDValidity:
			res.Err = fmt.Errorf("%w: the folder changed since the first page; search again", mailstore.ErrStaleRef)
		case plan.ServerOnly:
			// Whatever the cache could return here would not be the whole answer.
		default:
			uids, count, err := opts.Store.SearchUIDs(ctx, opts.Account.Name, res.Source, plan.Where, bound.Below, req.CountOnly)
			if err != nil {
				return nil, readErr(err)
			}
			res.UIDs, res.Count = uids, count
		}
		out = append(out, res)
	}
	return out, nil
}

// searchScope is the folders a search covers: the keys of req.Bounds,
// sorted, as the server takes them, or the folders the server's own Search
// would pick for req.In.
func (v *View) searchScope(ctx context.Context, req mailstore.SearchRequest) ([]string, error) {
	if req.Bounds != nil {
		return slices.Sorted(maps.Keys(req.Bounds)), nil
	}
	sc, ok := v.server.(mailstore.Scoper)
	if !ok {
		return nil, fmt.Errorf("%w: the account's store cannot say which folders a search covers", mailstore.ErrUnsupported)
	}
	return sc.SearchFolders(ctx, req.In)
}

// FetchSummaries returns the cached rows of uids in src as the server's
// FetchSummaries would: in the order of uids, leaving out UIDs the cache
// lacks, ErrStaleRef when the cache holds the folder under another
// UIDVALIDITY. With snippets, each row whose body is cached gets its
// snippet from it, as the rendered text's first characters; the rows
// without one get theirs from a single server FetchSummaries for those
// UIDs only. When that call fails, those rows come back with no snippet
// and the failure is logged, unless ctx has ended.
// A failed read of cache.db is ErrRead.
func (v *View) FetchSummaries(ctx context.Context, src mailstore.Source, uids []uint32, opts mailstore.SummaryOptions) ([]mailstore.Summary, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	store, account := v.a.o.Store, v.a.o.Account.Name
	p, held, err := store.Progress(ctx, account, src.Folder)
	switch {
	case err != nil:
		return nil, readErr(err)
	case !held:
		return nil, fmt.Errorf("%w: the cache does not hold the folder", mailstore.ErrNotFound)
	case p.UIDValidity != src.UIDValidity:
		return nil, fmt.Errorf("%w: the folder changed; search again", mailstore.ErrStaleRef)
	}
	rows, err := store.Rows(ctx, src, account, uids)
	if err != nil {
		return nil, readErr(err)
	}
	out := make([]mailstore.Summary, len(rows))
	for i, r := range rows {
		out[i] = r.Summary()
	}
	if !opts.Snippets {
		return out, nil
	}
	// at holds every row of a UID, since uids may repeat one.
	var missing []uint32
	at := map[uint32][]int{}
	for i := range out {
		b, held, err := store.Body(ctx, out[i].Ref)
		if err != nil {
			return nil, readErr(err)
		}
		if snippet, ok := cachedSnippet(b, held); ok {
			out[i].Preview = &snippet
			continue
		}
		uid := out[i].Ref.UID
		if len(at[uid]) == 0 {
			missing = append(missing, uid)
		}
		at[uid] = append(at[uid], i)
	}
	if len(missing) == 0 {
		return out, nil
	}
	sums, err := v.server.FetchSummaries(ctx, src, missing, mailstore.SummaryOptions{Snippets: true})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The cached rows still answer the page; only these snippets are lost.
		v.a.o.Logger.WarnContext(ctx, "the server could not fill the snippets the cache lacks; those rows have none", "folder", src.Folder, "rows", len(missing), "reason", reasonCode(err))
		return out, nil
	}
	for _, s := range sums {
		for _, i := range at[s.Ref.UID] {
			out[i].Preview, out[i].Sample = s.Preview, s.Sample
		}
	}
	return out, nil
}

// cachedSnippet is the snippet of a cached body b, held when the cache has
// one: the start of the text render.Render gives with Full, at most as many
// bytes as a server's text sample and cut on a character boundary. It is
// left unclipped, since the daemon's snippetOf clips a PREVIEW once; a
// preview clipped here would lose a space at the cut that the server's
// snippet keeps. ok is false when no body is held or it does not render;
// the server then supplies the snippet.
func cachedSnippet(b Body, held bool) (string, bool) {
	if !held {
		return "", false
	}
	body, err := render.Render(b.Raw, render.Options{Full: true})
	if err != nil {
		return "", false
	}
	text := body.Text
	if len(text) > mailstore.SampleBytes {
		cut := mailstore.SampleBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return text, true
}
