package watch

import (
	"reflect"
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
		{Kind: KindStatus, State: StateAuthRequired, Detail: "run: chainmail auth login a"},
		{Kind: KindGap, Reason: GapUIDValidity},
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
}
