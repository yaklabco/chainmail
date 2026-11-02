package watch_test

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

var (
	diffAt      = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	diffEarlier = diffAt.Add(-time.Hour)
)

// diffRef is the ref of a message in host's INBOX at UIDVALIDITY 7.
func diffRef(uid uint32, msgID uint64) string {
	return ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: 7, UID: uid, GmailMsgID: msgID}.String()
}

// diffSummary is a new message in host's INBOX at UIDVALIDITY 7.
func diffSummary(uid uint32, msgID uint64) mailstore.Summary {
	return mailstore.Summary{
		Ref:     ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: 7, UID: uid, GmailMsgID: msgID},
		Date:    diffEarlier,
		Subject: fmt.Sprintf("message %d", uid),
	}
}

// diffState is INBOX at UIDVALIDITY 7.
func diffState(uidNext uint32, modSeq uint64, condStore bool) mailstore.FolderState {
	return mailstore.FolderState{UIDValidity: 7, UIDNext: uidNext, HighestModSeq: modSeq, CondStore: condStore}
}

// diffCondRow is a CONDSTORE checkpoint at UIDNEXT 5 and HIGHESTMODSEQ 40,
// last synced and swept an hour before diffAt.
func diffCondRow() watch.FolderRow {
	return watch.FolderRow{
		Account: "host", Folder: "INBOX", UIDValidity: 7, UIDNext: 5, HighestModSeq: 40, CondStore: true,
		Mode: "idle", State: watch.StateWatching, LastSyncAt: diffEarlier, LastSweepAt: diffEarlier,
	}
}

// diffPlainRow is diffCondRow on a server without CONDSTORE.
func diffPlainRow() watch.FolderRow {
	r := diffCondRow()
	r.HighestModSeq = 0
	r.CondStore = false
	return r
}

// diffNext is prev advanced to st with UIDNEXT uidNext, synced at diffAt and
// last swept at sweptAt.
func diffNext(prev watch.FolderRow, st mailstore.FolderState, uidNext uint32, sweptAt time.Time) watch.FolderRow {
	prev.UIDValidity = st.UIDValidity
	prev.UIDNext = uidNext
	prev.HighestModSeq = st.HighestModSeq
	prev.CondStore = st.CondStore
	prev.LastSyncAt = diffAt
	prev.LastSweepAt = sweptAt
	return prev
}

// diffLine renders an event as one comparable line: its kind, ref and the
// fields its kind carries.
func diffLine(e watch.Event) string {
	switch e.Kind {
	case watch.KindGap:
		return fmt.Sprintf("gap %s next=%d", e.Reason, e.NextID)
	case watch.KindStatus:
		return "status " + e.State
	case watch.KindFlags:
		return fmt.Sprintf("flags %s %v %v", e.Ref, e.Flags, e.Labels)
	default:
		return string(e.Kind) + " " + e.Ref
	}
}

func diffLines(events []watch.Event) []string {
	var lines []string
	for _, e := range events {
		lines = append(lines, diffLine(e))
	}
	return lines
}

func newLine(uid uint32, msgID uint64) string { return "new " + diffRef(uid, msgID) }

func expungedLine(uid uint32, msgID uint64) string { return "expunged " + diffRef(uid, msgID) }

func flagsLine(uid uint32, msgID uint64, flags, labels []string) string {
	return fmt.Sprintf("flags %s %v %v", diffRef(uid, msgID), flags, labels)
}

type diffCase struct {
	name    string
	prev    watch.FolderRow
	known   map[uint32]watch.KnownUID
	delta   mailstore.Delta
	sweep   bool
	events  []string
	next    watch.FolderRow
	replace map[uint32]watch.KnownUID
	put     map[uint32]watch.KnownUID
	deleted []uint32
}

func runDiffCases(t *testing.T, tests []diffCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := watch.DiffInput{Account: "host", Folder: "INBOX", Sweep: tt.sweep, At: diffAt}
			got := watch.Diff(tt.prev, tt.known, tt.delta, in)
			if lines := diffLines(got.Events); !slices.Equal(lines, tt.events) {
				t.Errorf("events =\n  %s\nwant\n  %s", strings.Join(lines, "\n  "), strings.Join(tt.events, "\n  "))
			}
			for i, e := range got.Events {
				if e.ID != 0 || !e.At.Equal(diffAt) || e.Account != "host" || e.Folder != "INBOX" {
					t.Errorf("event %d = %+v, want ID 0, At %v, account host and folder INBOX", i, e, diffAt)
				}
			}
			if !reflect.DeepEqual(got.Next, tt.next) {
				t.Errorf("Next = %+v\nwant   %+v", got.Next, tt.next)
			}
			if !reflect.DeepEqual(got.Replace, tt.replace) {
				t.Errorf("Replace = %#v, want %#v", got.Replace, tt.replace)
			}
			if !reflect.DeepEqual(got.Put, tt.put) {
				t.Errorf("Put = %#v, want %#v", got.Put, tt.put)
			}
			if !reflect.DeepEqual(got.Delete, tt.deleted) {
				t.Errorf("Delete = %v, want %v", got.Delete, tt.deleted)
			}
		})
	}
}

// A rebased delta replaces the UID set silently: a status event, a gap when
// the folder had a checkpoint, and never new, flags or expunged events.
func TestDiffBaseline(t *testing.T) {
	t.Parallel()
	runDiffCases(t, []diffCase{
		{
			name:   "first baseline with CONDSTORE",
			delta:  mailstore.Delta{State: diffState(4, 30, true), Rebased: true, UIDs: []uint32{1, 2, 3}},
			events: []string{"status baselined"},
			next: watch.FolderRow{
				Account: "host", Folder: "INBOX", UIDValidity: 7, UIDNext: 4, HighestModSeq: 30, CondStore: true,
				State: watch.StateBaselined, LastSyncAt: diffAt, LastSweepAt: diffAt,
			},
			replace: map[uint32]watch.KnownUID{1: {}, 2: {}, 3: {}},
		},
		{
			name:  "UIDVALIDITY change emits a gap before the baseline",
			prev:  diffCondRow(),
			known: map[uint32]watch.KnownUID{1: {}, 4: {}},
			delta: mailstore.Delta{
				State:   mailstore.FolderState{UIDValidity: 9, UIDNext: 3, HighestModSeq: 12, CondStore: true},
				Rebased: true,
				UIDs:    []uint32{1, 2},
				Changed: []mailstore.FlagState{{UID: 2, GmailMsgID: 502}},
			},
			events: []string{"gap uidvalidity next=0", "status baselined"},
			next: watch.FolderRow{
				Account: "host", Folder: "INBOX", UIDValidity: 9, UIDNext: 3, HighestModSeq: 12, CondStore: true,
				Mode: "idle", State: watch.StateBaselined, LastSyncAt: diffAt, LastSweepAt: diffAt,
			},
			replace: map[uint32]watch.KnownUID{1: {}, 2: {GmailMsgID: 502}},
		},
		{
			// Gmail returns every message in Changed with its X-GM-MSGID even
			// under CONDSTORE. The msgids are kept; the flags are not hashed.
			name: "Gmail baseline with CONDSTORE takes message IDs from Changed",
			delta: mailstore.Delta{
				State:   diffState(3, 20, true),
				Rebased: true,
				UIDs:    []uint32{1, 2},
				Changed: []mailstore.FlagState{
					{UID: 1, GmailMsgID: 101, Flags: []string{`\Seen`}, Labels: []string{`\Inbox`}},
					{UID: 2, GmailMsgID: 102},
				},
			},
			events: []string{"status baselined"},
			next: watch.FolderRow{
				Account: "host", Folder: "INBOX", UIDValidity: 7, UIDNext: 3, HighestModSeq: 20, CondStore: true,
				State: watch.StateBaselined, LastSyncAt: diffAt, LastSweepAt: diffAt,
			},
			replace: map[uint32]watch.KnownUID{1: {GmailMsgID: 101}, 2: {GmailMsgID: 102}},
		},
		{
			name: "baseline without CONDSTORE stores flag hashes and ignores known UIDs",
			// UID 9 is known but gone: a baseline never reports it expunged.
			known: map[uint32]watch.KnownUID{9: {}},
			delta: mailstore.Delta{
				State:     diffState(3, 0, false),
				Rebased:   true,
				FullFlags: true,
				UIDs:      []uint32{1, 2},
				Changed: []mailstore.FlagState{
					{UID: 1, Flags: []string{`\Seen`}},
					{UID: 2, GmailMsgID: 802, Labels: []string{`\Inbox`}},
				},
			},
			events: []string{"status baselined"},
			next: watch.FolderRow{
				Account: "host", Folder: "INBOX", UIDValidity: 7, UIDNext: 3,
				State: watch.StateBaselined, LastSyncAt: diffAt, LastSweepAt: diffAt,
			},
			replace: map[uint32]watch.KnownUID{
				1: {FlagsHash: watch.FlagsHash([]string{`\Seen`}, nil)},
				2: {GmailMsgID: 802, FlagsHash: watch.FlagsHash(nil, []string{`\Inbox`})},
			},
		},
		{
			name:   "baseline of an empty folder still replaces the UID set",
			delta:  mailstore.Delta{State: diffState(1, 0, true), Rebased: true},
			events: []string{"status baselined"},
			next: watch.FolderRow{
				Account: "host", Folder: "INBOX", UIDValidity: 7, UIDNext: 1, CondStore: true,
				State: watch.StateBaselined, LastSyncAt: diffAt, LastSweepAt: diffAt,
			},
			replace: map[uint32]watch.KnownUID{},
		},
	})
}

// A delta from a folder with a checkpoint becomes expunged, flags and new
// events, UID changes and the next checkpoint.
func TestDiff(t *testing.T) {
	t.Parallel()
	seen := []string{`\Seen`}
	runDiffCases(t, []diffCase{
		{
			name:  "nothing changed",
			prev:  diffCondRow(),
			delta: mailstore.Delta{State: diffState(5, 40, true)},
			next:  diffNext(diffCondRow(), diffState(5, 40, true), 5, diffEarlier),
		},
		{
			name: "new mail with CONDSTORE",
			prev: diffCondRow(),
			// Known UIDs without d.UIDs never become expunges.
			known:  map[uint32]watch.KnownUID{1: {}, 2: {}, 3: {}, 4: {}},
			delta:  mailstore.Delta{State: diffState(7, 41, true), New: []mailstore.Summary{diffSummary(5, 905), diffSummary(6, 0)}},
			events: []string{newLine(5, 905), newLine(6, 0)},
			next:   diffNext(diffCondRow(), diffState(7, 41, true), 7, diffEarlier),
			put:    map[uint32]watch.KnownUID{5: {GmailMsgID: 905}, 6: {}},
		},
		{
			name: "CONDSTORE flags ignore UIDs at or above the old UIDNEXT",
			prev: diffCondRow(),
			delta: mailstore.Delta{
				State: diffState(7, 45, true),
				New:   []mailstore.Summary{diffSummary(5, 0), diffSummary(6, 0)},
				Changed: []mailstore.FlagState{
					{UID: 4, GmailMsgID: 904, Flags: seen, Labels: []string{`\Inbox`}},
					{UID: 2, Flags: []string{`\Flagged`}},
					{UID: 6, Flags: seen},
					{UID: 8, Flags: seen},
				},
			},
			events: []string{
				flagsLine(2, 0, []string{`\Flagged`}, nil),
				flagsLine(4, 904, seen, []string{`\Inbox`}),
				newLine(5, 0),
				newLine(6, 0),
			},
			next: diffNext(diffCondRow(), diffState(7, 45, true), 7, diffEarlier),
			put:  map[uint32]watch.KnownUID{5: {}, 6: {}},
		},
		{
			// A source that reports a message below the checkpoint as new
			// still yields one event for it, not new and flags both.
			name: "CONDSTORE flags skip UIDs in this pass's new mail",
			prev: diffCondRow(),
			delta: mailstore.Delta{
				State:   diffState(6, 42, true),
				New:     []mailstore.Summary{diffSummary(4, 0)},
				Changed: []mailstore.FlagState{{UID: 4, Flags: seen}, {UID: 3, Flags: []string{`\Answered`}}},
			},
			events: []string{flagsLine(3, 0, []string{`\Answered`}, nil), newLine(4, 0)},
			next:   diffNext(diffCondRow(), diffState(6, 42, true), 6, diffEarlier),
			put:    map[uint32]watch.KnownUID{4: {}},
		},
		{
			name: "full flags without CONDSTORE compare hashes",
			prev: diffPlainRow(),
			known: map[uint32]watch.KnownUID{
				1: {FlagsHash: watch.FlagsHash(nil, nil)},
				2: {FlagsHash: watch.FlagsHash(seen, nil)},
				3: {GmailMsgID: 303, FlagsHash: watch.FlagsHash(nil, []string{`\Inbox`})},
				4: {FlagsHash: watch.FlagsHash(seen, nil)},
			},
			delta: mailstore.Delta{
				State:     diffState(6, 0, false),
				FullFlags: true,
				UIDs:      []uint32{1, 2, 3, 4, 5},
				New:       []mailstore.Summary{diffSummary(5, 0)},
				Changed: []mailstore.FlagState{
					{UID: 5, Flags: seen}, // new in this pass, so not known
					{UID: 1, Flags: seen},
					{UID: 2, Flags: seen},
					{UID: 3},
					{UID: 4, Flags: seen},
				},
			},
			events: []string{flagsLine(1, 0, seen, nil), flagsLine(3, 303, nil, nil), newLine(5, 0)},
			next:   diffNext(diffPlainRow(), diffState(6, 0, false), 6, diffEarlier),
			put: map[uint32]watch.KnownUID{
				1: {FlagsHash: watch.FlagsHash(seen, nil)},
				3: {GmailMsgID: 303, FlagsHash: watch.FlagsHash(nil, nil)},
				5: {FlagsHash: watch.FlagsHash(nil, nil)},
			},
		},
		{
			name: "full flags without CONDSTORE find expunges with their Gmail message ID",
			prev: diffPlainRow(),
			known: map[uint32]watch.KnownUID{
				1: {FlagsHash: watch.FlagsHash(nil, nil)},
				2: {GmailMsgID: 202, FlagsHash: watch.FlagsHash(nil, nil)},
				3: {FlagsHash: watch.FlagsHash(nil, nil)},
			},
			delta: mailstore.Delta{
				State:     diffState(5, 0, false),
				FullFlags: true,
				UIDs:      []uint32{1, 3},
				Changed:   []mailstore.FlagState{{UID: 1}, {UID: 3}},
			},
			events:  []string{expungedLine(2, 202)},
			next:    diffNext(diffPlainRow(), diffState(5, 0, false), 5, diffEarlier),
			deleted: []uint32{2},
		},
		{
			name:  "sweep expunges only below the old UIDNEXT",
			prev:  diffCondRow(),
			sweep: true,
			// UID 6 is at or above UIDNEXT 5, so its absence proves nothing.
			known:   map[uint32]watch.KnownUID{1: {}, 2: {GmailMsgID: 202}, 3: {}, 6: {}},
			delta:   mailstore.Delta{State: diffState(5, 40, true), UIDs: []uint32{1, 3}},
			events:  []string{expungedLine(2, 202)},
			next:    diffNext(diffCondRow(), diffState(5, 40, true), 5, diffAt),
			deleted: []uint32{2},
		},
		{
			name:  "sweep of an emptied folder expunges every known UID below the old UIDNEXT",
			prev:  diffCondRow(),
			sweep: true,
			known: map[uint32]watch.KnownUID{1: {}, 3: {GmailMsgID: 303}, 6: {}},
			delta: mailstore.Delta{State: diffState(5, 41, true), UIDs: []uint32{}},
			events: []string{
				expungedLine(1, 0),
				expungedLine(3, 303),
			},
			next:    diffNext(diffCondRow(), diffState(5, 41, true), 5, diffAt),
			deleted: []uint32{1, 3},
		},
		{
			// Only a sweep or full flags make UIDs the whole folder, so an
			// empty UID set alone proves nothing.
			name:  "empty UIDs without a sweep or full flags expunge nothing",
			prev:  diffCondRow(),
			known: map[uint32]watch.KnownUID{1: {}, 3: {}},
			delta: mailstore.Delta{State: diffState(5, 40, true), UIDs: []uint32{}},
			next:  diffNext(diffCondRow(), diffState(5, 40, true), 5, diffEarlier),
		},
		{
			name: "a batch with more to come stops after its last new UID",
			prev: diffCondRow(),
			delta: mailstore.Delta{
				State: diffState(20, 50, true),
				New:   []mailstore.Summary{diffSummary(5, 0), diffSummary(6, 0)},
				More:  true,
			},
			events: []string{newLine(5, 0), newLine(6, 0)},
			next:   diffNext(diffCondRow(), diffState(20, 50, true), 7, diffEarlier),
			put:    map[uint32]watch.KnownUID{5: {}, 6: {}},
		},
		{
			name:  "events are ordered expunged, flags, new, each by UID",
			prev:  diffCondRow(),
			sweep: true,
			known: map[uint32]watch.KnownUID{1: {}, 2: {}, 3: {}, 4: {}},
			delta: mailstore.Delta{
				State:   diffState(7, 45, true),
				UIDs:    []uint32{1, 3, 4, 5, 6},
				New:     []mailstore.Summary{diffSummary(6, 0), diffSummary(5, 0)},
				Changed: []mailstore.FlagState{{UID: 3, Flags: seen}, {UID: 1, Flags: []string{`\Flagged`}}},
			},
			events: []string{
				expungedLine(2, 0),
				flagsLine(1, 0, []string{`\Flagged`}, nil),
				flagsLine(3, 0, seen, nil),
				newLine(5, 0),
				newLine(6, 0),
			},
			next:    diffNext(diffCondRow(), diffState(7, 45, true), 7, diffAt),
			put:     map[uint32]watch.KnownUID{5: {}, 6: {}},
			deleted: []uint32{2},
		},
	})
}

// A new event carries the search row summary of its message, with the
// snippet only when the renderer gives one, and shares no slice with it.
func TestDiffNewEventMessage(t *testing.T) {
	t.Parallel()
	preview := "preview text"
	summary := func() mailstore.Summary {
		return mailstore.Summary{
			Ref:           ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: 7, UID: 5, GmailMsgID: 905},
			Date:          diffEarlier,
			InternalDate:  diffEarlier.Add(time.Minute),
			From:          []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
			To:            []mailstore.Address{{Email: "me@example.com"}},
			Subject:       "Invoice 7",
			Flags:         []string{`\Seen`},
			Labels:        []string{`\Inbox`},
			Size:          2048,
			HasAttachment: true,
			ThreadID:      "18c2",
			Preview:       &preview,
		}
	}
	want := func(snippet *string) *watch.Message {
		return &watch.Message{
			Date:          diffEarlier,
			InternalDate:  diffEarlier.Add(time.Minute),
			From:          []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
			To:            []mailstore.Address{{Email: "me@example.com"}},
			Subject:       "Invoice 7",
			Flags:         []string{`\Seen`},
			Labels:        []string{`\Inbox`},
			Size:          2048,
			HasAttachment: true,
			ThreadID:      "18c2",
			Snippet:       snippet,
		}
	}
	tests := []struct {
		name    string
		snippet func(mailstore.Summary) string
		want    *watch.Message
	}{
		{name: "no renderer", want: want(nil)},
		{name: "empty snippet", snippet: func(mailstore.Summary) string { return "" }, want: want(nil)},
		{name: "snippet", snippet: func(s mailstore.Summary) string { return *s.Preview }, want: want(&preview)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := summary()
			in := watch.DiffInput{Account: "host", Folder: "INBOX", At: diffAt, Snippet: tt.snippet}
			got := watch.Diff(diffCondRow(), nil, mailstore.Delta{State: diffState(6, 40, true), New: []mailstore.Summary{s}}, in)
			if len(got.Events) != 1 {
				t.Fatalf("events = %d, want 1", len(got.Events))
			}
			e := got.Events[0]
			if e.Kind != watch.KindNew || e.Ref != s.Ref.String() {
				t.Errorf("event = %s %s, want new %s", e.Kind, e.Ref, s.Ref)
			}
			if !reflect.DeepEqual(e.Message, tt.want) {
				t.Errorf("Message = %+v\nwant      %+v", e.Message, tt.want)
			}
			s.Flags[0] = "changed"
			s.From[0].Name = "changed"
			s.Labels[0] = "changed"
			if !reflect.DeepEqual(e.Message, tt.want) {
				t.Errorf("Message shares a slice with the summary: %+v", e.Message)
			}
		})
	}
}
