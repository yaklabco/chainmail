package fake

import (
	"errors"
	"slices"
	"testing"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// malformRig is a store with five INBOX messages, the second with an
// attachment. It returns their refs' UIDs.
func malformRig(t *testing.T) (*Store, []uint32, mailstore.Source) {
	t.Helper()
	s := New("host", false)
	var uids []uint32
	for i := range 5 {
		m := Message{Folder: "INBOX", Subject: "m", Date: cacheDay(20 + i)}
		if i == 1 {
			m.Attachments = []Attachment{{Filename: "a.pdf", ContentType: "application/pdf", Data: []byte("%PDF")}}
		}
		uids = append(uids, s.Add(m).UID)
	}
	return s, uids, mailstore.Source{Folder: "INBOX", UIDValidity: 1}
}

// A message with a bad body structure comes back without its attachments,
// marked AttachmentsUnknown. One with a bad envelope is left out, and the
// rest come back with an UnreadableError naming it, from FetchHeaders,
// FetchSummaries and a sync pass alike.
func TestMalformedMessages(t *testing.T) {
	t.Parallel()
	s, uids, src := malformRig(t)
	s.Malform(src.Folder, uids[1], BadStructure)
	s.Malform(src.Folder, uids[3], BadEnvelope)

	hs, err := s.FetchHeaders(t.Context(), src, uids)
	checkUnreadable(t, "FetchHeaders", err, uids[3])
	var got []uint32
	for _, h := range hs {
		got = append(got, h.Summary.Ref.UID)
		unknown := h.Summary.Ref.UID == uids[1]
		if h.Summary.AttachmentsUnknown != unknown || (unknown && (h.Attachments != nil || h.Summary.HasAttachment)) {
			t.Errorf("FetchHeaders UID %d: AttachmentsUnknown %v, attachments %v, has %v",
				h.Summary.Ref.UID, h.Summary.AttachmentsUnknown, h.Attachments, h.Summary.HasAttachment)
		}
	}
	if want := []uint32{uids[0], uids[1], uids[2], uids[4]}; !slices.Equal(got, want) {
		t.Errorf("FetchHeaders UIDs = %v, want %v", got, want)
	}

	sums, err := s.FetchSummaries(t.Context(), src, uids, mailstore.SummaryOptions{})
	checkUnreadable(t, "FetchSummaries", err, uids[3])
	if len(sums) != 4 || !sums[1].AttachmentsUnknown || sums[1].HasAttachment || sums[0].AttachmentsUnknown {
		t.Errorf("FetchSummaries = %+v, want 4 with the second AttachmentsUnknown", sums)
	}

	d, err := s.SyncFolder(t.Context(), "INBOX", mailstore.SyncRequest{From: mailstore.Checkpoint{UIDValidity: 1, UIDNext: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Unreadable, []uint32{uids[3]}) || len(d.New) != 4 || !d.New[1].AttachmentsUnknown {
		t.Errorf("SyncFolder New %d, Unreadable %v, want 4 new and %d unreadable", len(d.New), d.Unreadable, uids[3])
	}

	// A reader that now parses the message reads it whole.
	s.Malform(src.Folder, uids[3], WellFormed)
	s.Malform(src.Folder, uids[1], WellFormed)
	hs, err = s.FetchHeaders(t.Context(), src, uids)
	if err != nil || len(hs) != 5 || hs[1].Summary.AttachmentsUnknown || len(hs[1].Attachments) != 1 {
		t.Errorf("FetchHeaders after the fix = %d headers, %v", len(hs), err)
	}
}

// SetMalformedBatch makes a fetch of that many UIDs or more fail with a
// plain ErrMalformed and nothing read when it holds a message with a bad
// envelope, as imapstore does once its hunt runs out of logins. Smaller
// fetches isolate the message as usual.
func TestMalformedBatch(t *testing.T) {
	t.Parallel()
	s, uids, src := malformRig(t)
	s.Malform(src.Folder, uids[3], BadEnvelope)
	s.SetMalformedBatch(3)

	hs, err := s.FetchHeaders(t.Context(), src, uids[2:5])
	if !errors.Is(err, mailstore.ErrMalformed) || errors.As(err, new(*mailstore.UnreadableError)) || hs != nil {
		t.Errorf("FetchHeaders of 3 = %v, %v; want nothing and a plain ErrMalformed", hs, err)
	}
	if hs, err := s.FetchHeaders(t.Context(), src, uids[:3]); err != nil || len(hs) != 3 {
		t.Errorf("FetchHeaders of 3 well-formed = %d, %v; want 3 and no error", len(hs), err)
	}
	hs, err = s.FetchHeaders(t.Context(), src, uids[3:5])
	checkUnreadable(t, "FetchHeaders of 2", err, uids[3])
	if len(hs) != 1 {
		t.Errorf("FetchHeaders of 2 = %d headers, want 1", len(hs))
	}

	s.SetMalformedBatch(1)
	if hs, err := s.FetchHeaders(t.Context(), src, uids[3:4]); !errors.Is(err, mailstore.ErrMalformed) || errors.As(err, new(*mailstore.UnreadableError)) {
		t.Errorf("FetchHeaders of the one = %v, %v; want a plain ErrMalformed", hs, err)
	}
	s.SetMalformedBatch(0)
	_, err = s.FetchHeaders(t.Context(), src, uids)
	checkUnreadable(t, "FetchHeaders with no limit", err, uids[3])
}

func checkUnreadable(t *testing.T, call string, err error, uid uint32) {
	t.Helper()
	ue, ok := errors.AsType[*mailstore.UnreadableError](err)
	if !ok || ue.Folder != "INBOX" || !slices.Equal(ue.UIDs, []uint32{uid}) || !errors.Is(err, mailstore.ErrMalformed) {
		t.Errorf("%s error = %v, want an UnreadableError for UID %d in INBOX", call, err, uid)
	}
}
