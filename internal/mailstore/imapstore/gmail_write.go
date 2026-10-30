package imapstore

// Google versions of the Writer methods whose semantics differ there (spec
// 7.1, Google table). Move, Copy, Archive, Trash, Expunge and StoreLabels
// call them first on Google accounts. Until they are written, those calls
// fail with mailstore.ErrUnsupported.

import (
	"context"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

func (s *Store) gmailMove(_ context.Context, refs []ref.Ref, _ string) []mailstore.RefResult {
	return unsupported(refs)
}

func (s *Store) gmailCopy(_ context.Context, refs []ref.Ref, _ string) []mailstore.RefResult {
	return unsupported(refs)
}

func (s *Store) gmailArchive(_ context.Context, refs []ref.Ref) []mailstore.RefResult {
	return unsupported(refs)
}

func (s *Store) gmailTrash(_ context.Context, refs []ref.Ref) []mailstore.RefResult {
	return unsupported(refs)
}

func (s *Store) gmailExpunge(_ context.Context, refs []ref.Ref) []mailstore.RefResult {
	return unsupported(refs)
}

func (s *Store) gmailStoreLabels(_ context.Context, refs []ref.Ref, _, _ []string) []mailstore.RefResult {
	return unsupported(refs)
}
