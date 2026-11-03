package imapstore

import (
	"context"
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// headerItems is what FetchHeaders asks for: a summary's items without
// PREVIEW, and the References header on every provider, which a Gmail
// summary leaves out since X-GM-THRID gives its thread.
func (s *Store) headerItems() *imap.FetchOptions {
	o := s.summaryItems(false, false)
	o.BodySection = []*imap.FetchItemBodySection{referencesSection}
	return o
}

// FetchHeaders implements mailstore.HeaderFetcher. One UID FETCH asks for
// the batch, and its answer is read as it arrives, so a long batch is
// bounded by the server's silence rather than its total time. UIDs the
// answer leaves out on a folder the connection reused are asked for once
// more after the folder is opened afresh, as reads do, since a server may
// tell a session of new mail late. UID 0 names no message and is left out.
// Every item is read with PEEK or carries no body, so nothing sets \Seen.
func (s *Store) FetchHeaders(ctx context.Context, src mailstore.Source, uids []uint32) ([]mailstore.Header, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	var out []mailstore.Header
	err := s.withConn(ctx, src.Folder, func(c *conn) error {
		out = nil
		got := make(map[uint32]*imapclient.FetchMessageBuffer, len(uids))
		err := s.readIn(ctx, c, src.Folder, func() (bool, error) {
			if c.uidValidity != src.UIDValidity {
				return false, fmt.Errorf("%w: %s changed; search again", mailstore.ErrStaleRef, src.Folder)
			}
			missing := absent(uids, got)
			if len(missing) == 0 {
				return false, nil
			}
			var batch []*imapclient.FetchMessageBuffer
			if err := c.fetchEach(ctx, uidSet(missing), s.headerItems(), func(b *imapclient.FetchMessageBuffer) {
				batch = append(batch, b)
			}); err != nil {
				return false, err
			}
			for _, b := range batch {
				got[uint32(b.UID)] = b
			}
			return len(absent(uids, got)) > 0, nil
		})
		if err != nil {
			return err
		}
		for _, uid := range uids {
			if b, ok := got[uid]; ok {
				out = append(out, s.header(src.Folder, src.UIDValidity, b))
			}
		}
		return nil
	})
	return out, err
}

// absent returns the UIDs of uids that got holds no answer for, in order,
// each once, leaving out 0, which an IMAP UID set reads as "*".
func absent(uids []uint32, got map[uint32]*imapclient.FetchMessageBuffer) []uint32 {
	var out []uint32
	seen := make(map[uint32]bool, len(uids))
	for _, uid := range uids {
		if uid == 0 || seen[uid] || got[uid] != nil {
			continue
		}
		seen[uid] = true
		out = append(out, uid)
	}
	return out
}

// header is the Header of the message in b, read in folder under
// uidValidity: the summary FetchSummaries gives, and Cc, Bcc, Message-ID
// and In-Reply-To from the envelope, References from its header and the
// attachments from its body structure, as FetchMessages reads them.
func (s *Store) header(folder string, uidValidity uint32, b *imapclient.FetchMessageBuffer) mailstore.Header {
	h := mailstore.Header{
		Summary:     s.summary(folder, uidValidity, b),
		References:  msgIDList(b.FindBodySection(referencesSection), "References"),
		Attachments: attachmentsOf(b.BodyStructure),
	}
	if env := b.Envelope; env != nil {
		h.Cc, h.Bcc, h.MessageID, h.InReplyTo = addresses(env.Cc), addresses(env.Bcc), env.MessageID, env.InReplyTo
	}
	return h
}
