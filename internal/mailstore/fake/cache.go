package fake

import (
	"context"
	"fmt"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

var (
	_ mailstore.HeaderFetcher = (*Store)(nil)
	_ mailstore.Scoper        = (*Store)(nil)
)

// FetchHeaders implements mailstore.HeaderFetcher: the summaries
// FetchSummaries gives, with Cc, Bcc, the Message-ID without angle
// brackets, In-Reply-To, References and attachments as FetchMessages gives
// them, in the order of uids and leaving out UIDs the folder lacks. No
// UIDs gives nil, nil without checking src, as imapstore does.
func (s *Store) FetchHeaders(ctx context.Context, src mailstore.Source, uids []uint32) ([]mailstore.Header, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginRead(ctx, "FetchHeaders"); err != nil {
		return nil, err
	}
	if len(uids) == 0 {
		return nil, nil
	}
	if v, ok := s.uidValidity[src.Folder]; !ok {
		return nil, fmt.Errorf("%w: no folder %q", mailstore.ErrNotFound, src.Folder)
	} else if v != src.UIDValidity {
		return nil, fmt.Errorf("%w: %s changed", mailstore.ErrStaleRef, src.Folder)
	}
	var out []mailstore.Header
	for _, uid := range uids {
		m := s.findLocked(src.Folder, uid)
		if m == nil {
			continue
		}
		out = append(out, mailstore.Header{
			Summary: s.summaryLocked(m, s.refInLocked(m, src.Folder), mailstore.SummaryOptions{}),
			Cc:      slices.Clone(m.Cc), Bcc: slices.Clone(m.Bcc), MessageID: trimID(m.MessageID),
			InReplyTo: slices.Clone(m.InReplyTo), References: slices.Clone(m.References),
			Attachments: attachments(m),
		})
	}
	return out, nil
}

// SearchFolders implements mailstore.Scoper with the folders Search picks.
// The fake keeps no folder list, so a folder AddFolder made is in the next
// answer.
func (s *Store) SearchFolders(ctx context.Context, in string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginRead(ctx, "SearchFolders"); err != nil {
		return nil, err
	}
	return s.sourcesLocked(in)
}
