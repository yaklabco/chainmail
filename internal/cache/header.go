package cache

import (
	"strconv"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// RowFromHeader is the cache row of a fetched header. GmailThrID is the
// Gmail thread ID parsed from Summary.ThreadID, which Gmail refs carry in
// lowercase hexadecimal; it is 0 on other accounts, whose ThreadID is a
// hash of the thread's root Message-ID, and when ThreadID does not parse.
func RowFromHeader(h mailstore.Header) Row {
	s := h.Summary
	r := Row{
		Account:       s.Ref.Account,
		Folder:        s.Ref.Folder,
		UIDValidity:   s.Ref.UIDValidity,
		UID:           s.Ref.UID,
		GmailMsgID:    s.Ref.GmailMsgID,
		InternalDate:  s.InternalDate,
		Date:          s.Date,
		Size:          s.Size,
		Subject:       s.Subject,
		From:          s.From,
		To:            s.To,
		Cc:            h.Cc,
		Bcc:           h.Bcc,
		MessageID:     h.MessageID,
		InReplyTo:     h.InReplyTo,
		References:    h.References,
		ThreadID:      s.ThreadID,
		Flags:         s.Flags,
		Labels:        s.Labels,
		HasAttachment: s.HasAttachment,
		Attachments:   h.Attachments,
	}
	if s.Ref.GmailMsgID != 0 && s.ThreadID != "" {
		if id, err := strconv.ParseUint(s.ThreadID, 16, 64); err == nil {
			r.GmailThrID = id
		}
	}
	return r
}
