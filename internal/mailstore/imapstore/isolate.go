package imapstore

import (
	"errors"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// isolate fetches the messages uids name in folder with fetch, so that one
// message whose response chainmail cannot read costs that message and not
// the batch. fetch reads the messages it is given with their body structure
// when structure is set, and without it otherwise.
//
// When fetch fails with mailstore.ErrMalformed, isolate splits uids in
// halves and fetches each half on its own, down to the message at fault.
// The connection the failure came on is dead, so each try takes another
// one. A single message that still fails is fetched once more without its
// body structure, the part of a response most often malformed, and fetch
// marks what it reads then as having attachments unknown. A message that
// fails that way too is unreadable. Both are logged with the account,
// folder and UID.
//
// It returns the rows read, in no set order, and the UIDs of the
// unreadable messages, ascending. Any other error ends it, with the rows
// read so far.
func isolate[T any](s *Store, folder string, uids []uint32, fetch func(uids []uint32, structure bool) ([]T, error)) ([]T, []uint32, error) {
	rows, unreadable, err := bisect(s, folder, uids, fetch)
	slices.Sort(unreadable)
	return rows, unreadable, err
}

// bisect is isolate, with the unreadable UIDs in the order found.
func bisect[T any](s *Store, folder string, uids []uint32, fetch func(uids []uint32, structure bool) ([]T, error)) ([]T, []uint32, error) {
	rows, err := fetch(uids, true)
	if err == nil || !errors.Is(err, mailstore.ErrMalformed) || len(uids) == 0 {
		return rows, nil, err
	}
	if len(uids) > 1 {
		half := len(uids) / 2
		var out []T
		var unreadable []uint32
		for _, part := range [][]uint32{uids[:half], uids[half:]} {
			rows, bad, err := bisect(s, folder, part, fetch)
			out, unreadable = append(out, rows...), append(unreadable, bad...)
			if err != nil {
				return out, unreadable, err
			}
		}
		return out, unreadable, nil
	}
	log := s.opts.Logger
	rows, err = fetch(uids, false)
	switch {
	case err == nil:
		log.Warn("read a message without its body structure, which the mail server sent in a form chainmail cannot read; its attachments are unknown",
			"account", s.acct.Name, "folder", folder, "uid", uids[0])
		return rows, nil, nil
	case errors.Is(err, mailstore.ErrMalformed):
		log.Warn("cannot read a message: the mail server's response to it is in a form chainmail cannot read",
			"account", s.acct.Name, "folder", folder, "uid", uids[0])
		return nil, slices.Clone(uids), nil
	default:
		return rows, nil, err
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
