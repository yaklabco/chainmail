package cache

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
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
// searches here only when neither can happen.
func (v *View) Search(ctx context.Context, req mailstore.SearchRequest) ([]mailstore.SourceResult, error) {
	opts := v.a.o
	plan, err := Compile(req.Query, req.In, opts.Account.Provider.Gmail, opts.Clock.Now())
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
			return nil, err
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
				return nil, err
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
