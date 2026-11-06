package imapstore

import (
	"errors"
	"fmt"
	"math/bits"
	"slices"

	"github.com/yaklabco/chainmail/internal/mailstore"
)

// isolate fetches the messages uids name in folder with fetch, so that one
// message whose response chainmail cannot read costs that message and not
// the batch. fetch reads the messages it is given with their body structure
// when structure is set, and without it otherwise.
//
// When the FETCH fails with mailstore.ErrMalformed, which fetch marks with
// markFetch, isolate splits uids in halves and fetches each half on its
// own, down to the message at fault. The connection the failure came on is
// dead, so each try takes another one. A single message that still fails
// is fetched once more without its body structure, the part of a response
// most often malformed, and fetch marks what it reads then as having
// attachments unknown. A message that fails that way too is unreadable.
// Both are logged with the account, folder and UID. A malformed answer to
// opening the folder is not split on: it fails the batch. A malformed text
// sample drops the snippets (readSummaries) and is not split on either.
//
// Each failure costs a login, so the hunt stops after isolateBudget of
// them. The messages it has not reached by then are fetched in one go
// without their body structures. When that fails too, isolate returns the
// rows read with a plain ErrMalformed and no unreadable UIDs, so the caller
// backs off instead of recording a whole batch as unreadable.
//
// It returns the rows read, in no set order, and the UIDs of the
// unreadable messages, ascending. Any other error ends it, with the rows
// read so far.
func isolate[T any](s *Store, folder string, uids []uint32, fetch func(uids []uint32, structure bool) ([]T, error)) ([]T, []uint32, error) {
	h := &hunt[T]{s: s, folder: folder, fetch: fetch, budget: isolateBudget(len(uids))}
	if err := h.run(uids); err != nil {
		return h.rows, nil, err
	}
	if len(h.rest) > 0 {
		s.opts.Logger.Warn("too many messages in a form chainmail cannot read; reading the rest of the batch without their body structures",
			"account", s.acct.Name, "folder", folder, "messages", len(h.rest))
		rows, err := fetch(h.rest, false)
		h.rows = append(h.rows, rows...)
		switch {
		case splittable(err):
			return h.rows, nil, fmt.Errorf("%w: %d messages in %q, even without their body structures", mailstore.ErrMalformed, len(h.rest), folder)
		case err != nil:
			return h.rows, nil, err
		}
	}
	slices.Sort(h.unreadable)
	return h.rows, h.unreadable, nil
}

// isolateBudget is how many malformed answers, each costing a login, the
// hunt for the bad messages of a batch of n may meet: enough to find a few
// bad messages in any batch, and about 26 for a batch of 500.
func isolateBudget(n int) int { return 2*bits.Len(uint(n)) + 8 }

// hunt is the state of one isolate call.
type hunt[T any] struct {
	s      *Store
	folder string
	fetch  func(uids []uint32, structure bool) ([]T, error)
	// budget is how many more malformed answers the hunt may meet.
	budget     int
	rows       []T
	unreadable []uint32
	// rest are the UIDs the hunt left once its budget ran out.
	rest []uint32
}

// run fetches uids, splitting them on a malformed answer while the budget
// lasts and leaving them in rest once it is spent.
func (h *hunt[T]) run(uids []uint32) error {
	if h.budget <= 0 {
		h.rest = append(h.rest, uids...)
		return nil
	}
	rows, err := h.fetch(uids, true)
	if !splittable(err) || len(uids) == 0 {
		h.rows = append(h.rows, rows...)
		return err
	}
	h.budget--
	if len(uids) > 1 {
		half := len(uids) / 2
		if err := h.run(uids[:half]); err != nil {
			return err
		}
		return h.run(uids[half:])
	}
	if h.budget <= 0 {
		h.rest = append(h.rest, uids...)
		return nil
	}
	log := h.s.opts.Logger
	first := err
	rows, err = h.fetch(uids, false)
	switch {
	case err == nil:
		if len(rows) == 0 {
			return nil // gone since; nothing was read
		}
		log.Warn("read a message without its body structure, which the mail server sent in a form chainmail cannot read; its attachments are unknown",
			logAttrs(first, "account", h.s.acct.Name, "folder", h.folder, "uid", uids[0])...)
		h.rows = append(h.rows, rows...)
		return nil
	case splittable(err):
		h.budget--
		log.Warn("cannot read a message: the mail server's response to it is in a form chainmail cannot read",
			logAttrs(err, "account", h.s.acct.Name, "folder", h.folder, "uid", uids[0])...)
		h.unreadable = append(h.unreadable, uids[0])
		return nil
	default:
		h.rows = append(h.rows, rows...)
		return err
	}
}

// unreadableError is the UnreadableError for uids in folder, or nil when
// uids is empty.
func unreadableError(folder string, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	return &mailstore.UnreadableError{Folder: folder, UIDs: uids}
}

// fetchMalformed is ErrMalformed from the FETCH that reads a batch, which
// isolate splits the batch on. ErrMalformed from anything else, such as
// the answer to opening the folder, would fail every part of the batch
// alike, a login each, so it ends isolate as any other error does.
type fetchMalformed struct{ err error }

func (e *fetchMalformed) Error() string { return e.err.Error() }

func (e *fetchMalformed) Unwrap() error { return e.err }

// markFetch marks err, from the FETCH that reads a batch, as fetchMalformed
// when it is ErrMalformed, and returns any other error, nil included, as is.
func markFetch(err error) error {
	if !errors.Is(err, mailstore.ErrMalformed) {
		return err
	}
	return &fetchMalformed{err: err}
}

// splittable reports whether isolate can split a batch on err.
func splittable(err error) bool {
	_, ok := errors.AsType[*fetchMalformed](err)
	return ok
}
