package imapstore

import (
	"context"
	"fmt"
	"sync"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// Thread implements mailstore.Store.
func (s *Store) Thread(ctx context.Context, r ref.Ref) ([]ref.Ref, error) {
	if r.Account != s.acct.Name {
		return nil, fmt.Errorf("%w: the ref is for account %s", mailstore.ErrNotFound, r.Account)
	}
	if s.gmail() {
		return s.gmailThread(ctx, r)
	}
	return s.standardThread(ctx, r)
}

func (s *Store) gmailThread(ctx context.Context, r ref.Ref) ([]ref.Ref, error) {
	var threadID uint64
	seed := r
	err := s.withConn(ctx, r.Folder, func(c *conn) error {
		uid, b, err := s.fetchRef(ctx, c, r, &imap.FetchOptions{UID: true, GmailMsgID: true, GmailThreadID: true})
		if err != nil {
			return err
		}
		threadID = b.GmailThreadID
		seed = ref.Ref{Account: s.acct.Name, Folder: r.Folder, UIDValidity: c.uidValidity, UID: uid, GmailMsgID: b.GmailMsgID}
		return nil
	})
	if err != nil {
		return nil, err
	}
	folders, err := s.folderList(ctx, false)
	if err != nil {
		return nil, err
	}
	allMail := byRole(folders, mailstore.RoleAll)
	if allMail == "" {
		return nil, fmt.Errorf("%w: no All Mail folder", mailstore.ErrNotFound)
	}
	var out []ref.Ref
	err = s.withConn(ctx, allMail, func(c *conn) error {
		out = nil
		if err := s.open(ctx, c, allMail); err != nil {
			return err
		}
		uids, _, err := c.search(ctx, &imap.SearchCriteria{GmailThreadID: []uint64{threadID}}, false)
		if err != nil || len(uids) == 0 {
			return err
		}
		bufs, err := c.fetch(ctx, uidSet(uids), &imap.FetchOptions{UID: true, GmailMsgID: true})
		if err != nil {
			return err
		}
		for _, b := range bufs {
			out = append(out, ref.Ref{Account: s.acct.Name, Folder: allMail, UIDValidity: c.uidValidity, UID: uint32(b.UID), GmailMsgID: b.GmailMsgID})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return withSeed(out, seed), nil
}

func (s *Store) standardThread(ctx context.Context, r ref.Ref) ([]ref.Ref, error) {
	var root string
	err := s.withConn(ctx, r.Folder, func(c *conn) error {
		_, b, err := s.fetchRef(ctx, c, r, &imap.FetchOptions{
			UID: true, Envelope: true, BodySection: []*imap.FetchItemBodySection{referencesSection},
		})
		if err != nil {
			return err
		}
		root = rootOf(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if root == "" {
		return []ref.Ref{r}, nil
	}
	folders, err := s.folderList(ctx, false)
	if err != nil {
		return nil, err
	}
	sources, err := standardSources(folders, "")
	if err != nil {
		return nil, err
	}
	id := "<" + root + ">"
	header := func(key string) imap.SearchCriteria {
		return imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: key, Value: id}}}
	}
	criteria := &imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{
		header("Message-ID"),
		{Or: [][2]imap.SearchCriteria{{header("References"), header("In-Reply-To")}}},
	}}}
	found := make([][]ref.Ref, len(sources))
	errs := make([]error, len(sources))
	var wg sync.WaitGroup
	for i, folder := range sources {
		wg.Go(func() {
			errs[i] = s.withConn(ctx, folder, func(c *conn) error {
				found[i] = nil
				if err := s.open(ctx, c, folder); err != nil {
					return err
				}
				uids, _, err := c.search(ctx, criteria, false)
				for _, uid := range uids {
					found[i] = append(found[i], ref.Ref{Account: s.acct.Name, Folder: folder, UIDValidity: c.uidValidity, UID: uid})
				}
				return err
			})
		})
	}
	wg.Wait()
	return threadOf(ctx, found, errs, r)
}

// threadOf merges the matches found in each folder into the thread, seed
// included. A folder that failed on its own is left out, since the thread is
// still useful without it. A failure of the whole account fails the call
// instead of hiding behind a partial thread: a login refusal, the end of
// ctx, or every folder failing.
func threadOf(ctx context.Context, found [][]ref.Ref, errs []error, seed ref.Ref) ([]ref.Ref, error) {
	if err := accountFailure(ctx, errs); err != nil {
		return nil, err
	}
	var out []ref.Ref
	var first error
	failed := 0
	for i, refs := range found {
		if errs[i] != nil {
			failed++
			if first == nil {
				first = errs[i]
			}
			continue
		}
		out = append(out, refs...)
	}
	if failed > 0 && failed == len(found) {
		return nil, first
	}
	return withSeed(out, seed), nil
}

// withSeed adds seed to refs unless a ref already names the same message: the
// same folder and UID, or on Gmail the same X-GM-MSGID, since the seed may be
// a copy outside All Mail of a message the thread lists in All Mail.
func withSeed(refs []ref.Ref, seed ref.Ref) []ref.Ref {
	for _, r := range refs {
		if (r.Folder == seed.Folder && r.UID == seed.UID) || (seed.GmailMsgID != 0 && r.GmailMsgID == seed.GmailMsgID) {
			return refs
		}
	}
	return append(refs, seed)
}
