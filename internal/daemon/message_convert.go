package daemon

import (
	"slices"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/render"
)

// untrusted wraps one line of message-derived text.
func untrusted(s string) *chainmailv1.Untrusted {
	return &chainmailv1.Untrusted{Untrusted: render.CleanHeader(s)}
}

func untrustedList(ss []string) []*chainmailv1.Untrusted {
	out := make([]*chainmailv1.Untrusted, len(ss))
	for i, s := range ss {
		out[i] = untrusted(s)
	}
	return out
}

func addressesProto(list []mailstore.Address) []*chainmailv1.Address {
	out := make([]*chainmailv1.Address, len(list))
	for i, a := range list {
		out[i] = &chainmailv1.Address{Name: untrusted(a.Name), Email: untrusted(a.Email)}
	}
	return out
}

func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func rowFromSummary(sum mailstore.Summary, snippets bool, threadCount int) *chainmailv1.MessageRow {
	row := &chainmailv1.MessageRow{
		Ref: sum.Ref.String(), Account: sum.Ref.Account, Folder: sum.Ref.Folder,
		Date: timestamp(sum.Date), InternalDate: timestamp(sum.InternalDate),
		From: addressesProto(sum.From), To: addressesProto(sum.To), Subject: untrusted(sum.Subject),
		Flags: sum.Flags, Size: sum.Size, HasAttachment: sum.HasAttachment, ThreadId: sum.ThreadID, Labels: sum.Labels,
		ThreadCount: uint32(threadCount),
	}
	if snippets {
		row.Snippet = &chainmailv1.Untrusted{Untrusted: snippetOf(sum)}
	}
	return row
}

// snippetOf renders a row's snippet from the server's PREVIEW, or from the
// sampled start of its text part.
func snippetOf(sum mailstore.Summary) string {
	switch {
	case sum.Preview != nil:
		return render.Clip(*sum.Preview)
	case sum.Sample != nil:
		return render.Snippet(sum.Sample.MediaType, sum.Sample.Charset, sum.Sample.Encoding, sum.Sample.Data)
	default:
		return ""
	}
}

// messageProto builds the API form of msg. A message can be shared by
// several results, so slices are copied rather than aliased.
func messageProto(msg *mailstore.Message, body render.Body) *chainmailv1.Message {
	sum := msg.Summary
	out := &chainmailv1.Message{
		Ref: sum.Ref.String(), Account: sum.Ref.Account, Folder: sum.Ref.Folder, Date: timestamp(sum.Date),
		From: addressesProto(sum.From), To: addressesProto(sum.To), Cc: addressesProto(msg.Cc),
		Subject: untrusted(sum.Subject), MessageId: untrusted(msg.MessageID),
		InReplyTo: untrustedList(msg.InReplyTo), References: untrustedList(msg.References),
		Flags: slices.Clone(sum.Flags), Labels: slices.Clone(sum.Labels), ThreadId: sum.ThreadID,
		Body: &chainmailv1.Body{
			Untrusted: render.CleanText(body.Text), Truncated: body.Truncated || msg.TextClipped,
			QuotedTrimmed: body.QuotedTrimmed, TrimmedBytes: int64(body.TrimmedBytes),
		},
	}
	for _, a := range msg.Attachments {
		out.Attachments = append(out.Attachments, &chainmailv1.Attachment{Part: a.Part, Filename: untrusted(a.Filename), ContentType: mediaType(a.ContentType), Size: a.Size})
	}
	return out
}
