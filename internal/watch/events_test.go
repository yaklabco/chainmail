package watch

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

func TestEncodePayload(t *testing.T) {
	t.Parallel()
	snippet := "hi there"
	tests := []struct {
		name string
		e    Event
		want string
	}{
		{
			name: "new",
			e: Event{Kind: KindNew, Message: &Message{
				Date:          time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
				InternalDate:  time.Date(2026, 11, 1, 9, 31, 0, 0, time.UTC),
				From:          []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
				Subject:       "hello",
				Flags:         []string{`\Seen`},
				Labels:        []string{`\Inbox`},
				Size:          1234,
				HasAttachment: true,
				ThreadID:      "abc",
				Snippet:       &snippet,
			}},
			want: `{"message":{"date":"2026-11-01T09:30:00Z","internal_date":"2026-11-01T09:31:00Z",` +
				`"from":[{"Name":"Ann","Email":"ann@example.com"}],"to":null,"subject":"hello","flags":["\\Seen"],` +
				`"labels":["\\Inbox"],"size":1234,"has_attachment":true,"thread_id":"abc","snippet":"hi there"}}`,
		},
		{name: "flags", e: Event{Kind: KindFlags, Flags: []string{`\Seen`}, Labels: []string{"Work"}}, want: `{"flags":["\\Seen"],"labels":["Work"]}`},
		{name: "flags empty", e: Event{Kind: KindFlags}, want: `{"flags":[],"labels":[]}`},
		{name: "status", e: Event{Kind: KindStatus, State: StateWatching, Detail: "idle"}, want: `{"state":"watching","detail":"idle"}`},
		{name: "gap", e: Event{Kind: KindGap, Reason: GapUIDValidity}, want: `{"reason":"uidvalidity","next_id":0}`},
		{name: "expunged", e: Event{Kind: KindExpunged}, want: `{}`},
		{name: "unreadable", e: Event{Kind: KindUnreadable}, want: `{}`},
		{
			name: "item",
			e:    Event{Kind: KindItem, Item: &ItemChange{ItemID: "it_01", Type: "task", Revision: 7, Op: "updated", Fields: []string{"title", "state"}}},
			want: `{"item":{"item_id":"it_01","type":"task","revision":7,"op":"updated","fields":["title","state"]}}`,
		},
		{
			name: "item without fields",
			e:    Event{Kind: KindItem, Item: &ItemChange{ItemID: "it_admin_work", Type: "initiative", Revision: 1, Op: "created"}},
			want: `{"item":{"item_id":"it_admin_work","type":"initiative","revision":1,"op":"created"}}`,
		},
		{
			name: "item_mail",
			e: Event{Kind: KindItemMail, ItemID: "it_01", FromSelf: true, Message: &Message{
				Date:     time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
				From:     []mailstore.Address{{Name: "Me", Email: "me@example.com"}},
				Subject:  "re: plan",
				ThreadID: "18c2",
			}},
			want: `{"item_id":"it_01","from_self":true,"message":{"date":"2026-11-01T09:30:00Z","internal_date":"0001-01-01T00:00:00Z",` +
				`"from":[{"Name":"Me","Email":"me@example.com"}],"to":null,"subject":"re: plan","flags":null,` +
				`"size":0,"has_attachment":false,"thread_id":"18c2"}}`,
		},
		{
			name: "item_mail from someone else",
			e:    Event{Kind: KindItemMail, ItemID: "it_01", Message: &Message{Subject: "s"}},
			want: `{"item_id":"it_01","from_self":false,"message":{"date":"0001-01-01T00:00:00Z","internal_date":"0001-01-01T00:00:00Z",` +
				`"from":null,"to":null,"subject":"s","flags":null,"size":0,"has_attachment":false}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := encodePayload(tt.e)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("payload =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestEncodePayloadRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, want string
		e          Event
	}{
		{name: "unknown kind", e: Event{Kind: "moved"}, want: `unknown event kind "moved"`},
		{name: "new without message", e: Event{Kind: KindNew, Ref: "r"}, want: "has no message"},
		{name: "item without change", e: Event{Kind: KindItem}, want: "item event has no change"},
		{name: "item_mail without message", e: Event{Kind: KindItemMail, Ref: "r", ItemID: "it_01"}, want: "item_mail event for r has no message"},
		{name: "item_mail without item", e: Event{Kind: KindItemMail, Ref: "r", Message: &Message{}}, want: "item_mail event for r has no item"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := encodePayload(tt.e); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// decodePayload restores exactly the fields encodePayload wrote, for every kind.
func TestDecodePayloadRoundTrip(t *testing.T) {
	t.Parallel()
	snippet := "preview"
	tests := []Event{
		{Kind: KindNew, Message: &Message{
			Date:          time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
			InternalDate:  time.Date(2026, 11, 1, 9, 31, 0, 0, time.UTC),
			From:          []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
			To:            []mailstore.Address{{Email: "me@example.com"}},
			Subject:       "hello",
			Flags:         []string{`\Seen`, "$Label1"},
			Labels:        []string{`\Inbox`, "Work"},
			Size:          2048,
			HasAttachment: true,
			ThreadID:      "18c2",
			Snippet:       &snippet,
		}},
		{Kind: KindNew, Message: &Message{Subject: "bare"}},
		{Kind: KindFlags, Flags: []string{`\Seen`}, Labels: []string{"Work"}},
		{Kind: KindFlags},
		{Kind: KindExpunged},
		{Kind: KindUnreadable},
		{Kind: KindStatus, State: StateAuthRequired, Detail: "run: chainmail auth login a"},
		{Kind: KindGap, Reason: GapUIDValidity},
		{Kind: KindItem, Item: &ItemChange{ItemID: "it_01", Type: "story", Revision: 12, Op: "moved", Fields: []string{"parent_id"}}},
		{Kind: KindItem, Item: &ItemChange{ItemID: "it_02", Type: "task", Revision: 13, Op: "linked"}},
		{Kind: KindItemMail, ItemID: "it_01", FromSelf: true, Message: &Message{
			Date:     time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
			From:     []mailstore.Address{{Name: "Me", Email: "me@example.com"}},
			Subject:  "re: plan",
			Labels:   []string{`\Inbox`},
			ThreadID: "18c2",
			Snippet:  &snippet,
		}},
		{Kind: KindItemMail, ItemID: "it_02", Message: &Message{Subject: "bare"}},
	}
	for _, want := range tests {
		t.Run(string(want.Kind), func(t *testing.T) {
			t.Parallel()
			payload, err := encodePayload(want)
			if err != nil {
				t.Fatal(err)
			}
			got := Event{Kind: want.Kind}
			if err := decodePayload(&got, payload); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("decoded %s:\n got %+v\nwant %+v", payload, got, want)
			}
		})
	}
}

func TestDecodePayloadRejects(t *testing.T) {
	t.Parallel()
	e := Event{ID: 7, Kind: KindStatus}
	if err := decodePayload(&e, "{"); err == nil || !strings.Contains(err.Error(), "decoding event 7 payload") {
		t.Errorf("bad JSON: err = %v, want a decoding error", err)
	}
	e = Event{ID: 8, Kind: "moved"}
	if err := decodePayload(&e, "{}"); err == nil || !strings.Contains(err.Error(), `unknown kind "moved"`) {
		t.Errorf("unknown kind: err = %v, want a kind error", err)
	}
	for _, kind := range []Kind{KindItem, KindItemMail} {
		e = Event{ID: 9, Kind: kind}
		if err := decodePayload(&e, "["); err == nil || !strings.Contains(err.Error(), "decoding event 9 payload") {
			t.Errorf("bad %s JSON: err = %v, want a decoding error", kind, err)
		}
	}
}

// An item event reaches every reader that opts in, outside any untrusted
// wrapping, so ItemChange must never carry item text. A new field here is a
// deliberate decision that updates this list.
func TestItemChangeHoldsNoText(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[ItemChange]()
	var got []string
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	want := []string{"ItemID", "Type", "Revision", "Op", "Fields"}
	if !slices.Equal(got, want) {
		t.Errorf("ItemChange fields = %v, want %v", got, want)
	}
}
