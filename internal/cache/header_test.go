package cache

import (
	"reflect"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// TestRowFromHeader checks that every header field lands in its row field,
// and that GmailThrID is parsed from the hexadecimal ThreadID only on Gmail
// refs: a standard account's ThreadID is a ThreadKey hash, not a number.
func TestRowFromHeader(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	header := func(folder string, msgID uint64, thread string) mailstore.Header {
		return mailstore.Header{
			Summary: mailstore.Summary{
				Ref:  ref.Ref{Account: "acme", Folder: folder, UIDValidity: 7, UID: 42, GmailMsgID: msgID},
				Date: at, InternalDate: at.Add(time.Minute),
				From:    []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
				To:      []mailstore.Address{{Email: "me@example.com"}},
				Subject: "Invoice", Flags: []string{`\Seen`}, Size: 1200, HasAttachment: true,
				ThreadID: thread, Labels: []string{`\Inbox`},
			},
			Cc:          []mailstore.Address{{Name: "Cy", Email: "cy@example.com"}},
			Bcc:         []mailstore.Address{{Email: "hidden@example.com"}},
			MessageID:   "m1@example.com",
			InReplyTo:   []string{"m0@example.com"},
			References:  []string{"m0@example.com"},
			Attachments: []mailstore.Attachment{{Part: "2", Filename: "a.pdf", ContentType: "application/pdf", Size: 300}},
		}
	}
	row := func(folder string, msgID, thrID uint64, thread string) Row {
		return Row{
			Account: "acme", Folder: folder, UIDValidity: 7, UID: 42, GmailMsgID: msgID, GmailThrID: thrID,
			InternalDate: at.Add(time.Minute), Date: at, Size: 1200, Subject: "Invoice",
			From:      []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
			To:        []mailstore.Address{{Email: "me@example.com"}},
			Cc:        []mailstore.Address{{Name: "Cy", Email: "cy@example.com"}},
			Bcc:       []mailstore.Address{{Email: "hidden@example.com"}},
			MessageID: "m1@example.com", InReplyTo: []string{"m0@example.com"}, References: []string{"m0@example.com"},
			ThreadID: thread, Flags: []string{`\Seen`}, Labels: []string{`\Inbox`}, HasAttachment: true,
			Attachments: []mailstore.Attachment{{Part: "2", Filename: "a.pdf", ContentType: "application/pdf", Size: 300}},
		}
	}
	const allMail = "[Gmail]/All Mail"
	tests := []struct {
		name string
		in   mailstore.Header
		want Row
	}{
		{name: "standard", in: header("INBOX", 0, "5f3c"), want: row("INBOX", 0, 0, "5f3c")},
		{name: "gmail", in: header(allMail, 9001, "18c2a0f3e"), want: row(allMail, 9001, 0x18c2a0f3e, "18c2a0f3e")},
		{name: "gmail without a thread", in: header(allMail, 9001, ""), want: row(allMail, 9001, 0, "")},
		{name: "gmail thread that is not hex", in: header(allMail, 9001, "not-hex"), want: row(allMail, 9001, 0, "not-hex")},
	}
	degraded := header("INBOX", 0, "5f3c")
	degraded.Summary.AttachmentsUnknown, degraded.Summary.HasAttachment, degraded.Attachments = true, false, nil
	degradedRow := row("INBOX", 0, 0, "5f3c")
	degradedRow.AttachmentsUnknown, degradedRow.HasAttachment, degradedRow.Attachments = true, false, nil
	tests = append(tests, struct {
		name string
		in   mailstore.Header
		want Row
	}{name: "read without its body structure", in: degraded, want: degradedRow})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RowFromHeader(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RowFromHeader =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}
