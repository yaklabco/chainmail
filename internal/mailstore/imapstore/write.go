package imapstore

// Writer methods of the IMAP store. They are stubs that return
// mailstore.ErrUnsupported until the write tasks replace them.

import (
	"context"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// Append implements mailstore.Writer.
func (s *Store) Append(context.Context, string, []string, time.Time, []byte) (ref.Ref, error) {
	return ref.Ref{}, mailstore.ErrUnsupported
}

// Move implements mailstore.Writer.
func (s *Store) Move(_ context.Context, refs []ref.Ref, _ string) []mailstore.RefResult {
	return unsupported(refs)
}

// Copy implements mailstore.Writer.
func (s *Store) Copy(_ context.Context, refs []ref.Ref, _ string) []mailstore.RefResult {
	return unsupported(refs)
}

// Archive implements mailstore.Writer.
func (s *Store) Archive(_ context.Context, refs []ref.Ref) []mailstore.RefResult {
	return unsupported(refs)
}

// Trash implements mailstore.Writer.
func (s *Store) Trash(_ context.Context, refs []ref.Ref) []mailstore.RefResult {
	return unsupported(refs)
}

// Expunge implements mailstore.Writer.
func (s *Store) Expunge(_ context.Context, refs []ref.Ref) []mailstore.RefResult {
	return unsupported(refs)
}

// StoreFlags implements mailstore.Writer.
func (s *Store) StoreFlags(_ context.Context, refs []ref.Ref, _, _ []string) []mailstore.RefResult {
	return unsupported(refs)
}

// StoreLabels implements mailstore.Writer.
func (s *Store) StoreLabels(_ context.Context, refs []ref.Ref, _, _ []string) []mailstore.RefResult {
	return unsupported(refs)
}

// CreateFolder implements mailstore.Writer.
func (s *Store) CreateFolder(context.Context, string) error {
	return mailstore.ErrUnsupported
}

// RenameFolder implements mailstore.Writer.
func (s *Store) RenameFolder(context.Context, string, string) error {
	return mailstore.ErrUnsupported
}

// DeleteFolder implements mailstore.Writer.
func (s *Store) DeleteFolder(context.Context, string, bool) error {
	return mailstore.ErrUnsupported
}

// FindByMessageID implements mailstore.Writer.
func (s *Store) FindByMessageID(context.Context, string, string) ([]ref.Ref, error) {
	return nil, mailstore.ErrUnsupported
}

// unsupported fails every ref with mailstore.ErrUnsupported.
func unsupported(refs []ref.Ref) []mailstore.RefResult {
	out := make([]mailstore.RefResult, len(refs))
	for i := range out {
		out[i].Err = mailstore.ErrUnsupported
	}
	return out
}
