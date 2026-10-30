package imapstore

// Folder changes. They fail with mailstore.ErrUnsupported until written.

import (
	"context"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

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
