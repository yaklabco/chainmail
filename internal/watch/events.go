// Package watch keeps chainmail's sync state and its durable event log in
// state.db: the folder checkpoints and known UIDs that sync passes advance,
// and the events they append, read by watchers through named groups with
// Redis Streams semantics.
package watch

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// Kind is an event kind.
type Kind string

// Event kinds, as stored in the events table.
const (
	KindNew      Kind = "new"
	KindFlags    Kind = "flags"
	KindExpunged Kind = "expunged"
	KindStatus   Kind = "status"
	KindGap      Kind = "gap"
)

// Status states.
const (
	StateBaselined    = "baselined"
	StateCatchingUp   = "catching_up"
	StateWatching     = "watching"
	StatePolling      = "polling"
	StateReconnecting = "reconnecting"
	StateAuthRequired = "auth_required"
	StateError        = "error"
)

// Gap reasons.
const (
	GapUIDValidity = "uidvalidity"
	GapTrimmed     = "trimmed"
)

// Message is the summary a new event carries. Every string in it came
// from the message and is untrusted.
type Message struct {
	Date          time.Time           `json:"date"`
	InternalDate  time.Time           `json:"internal_date"`
	From          []mailstore.Address `json:"from"`
	To            []mailstore.Address `json:"to"`
	Subject       string              `json:"subject"`
	Flags         []string            `json:"flags"`
	Labels        []string            `json:"labels,omitempty"`
	Size          int64               `json:"size"`
	HasAttachment bool                `json:"has_attachment"`
	ThreadID      string              `json:"thread_id,omitempty"`
	Snippet       *string             `json:"snippet,omitempty"`
}

// Event is one entry in the log. Which optional fields are set depends on Kind.
type Event struct {
	ID      int64 // assigned by Append; 0 before
	At      time.Time
	Kind    Kind
	Account string
	Folder  string
	Ref     string
	Message *Message // new
	Flags   []string // flags
	Labels  []string // flags
	State   string   // status
	Detail  string   // status
	Reason  string   // gap
	NextID  int64    // gap
}

// The payload column holds one of these per kind, as JSON. expunged stores {}.
type (
	newPayload struct {
		Message *Message `json:"message"`
	}
	flagsPayload struct {
		Flags  []string `json:"flags"`
		Labels []string `json:"labels"`
	}
	statusPayload struct {
		State  string `json:"state"`
		Detail string `json:"detail"`
	}
	gapPayload struct {
		Reason string `json:"reason"`
		NextID int64  `json:"next_id"`
	}
)

// encodePayload renders the kind-specific fields of e as the payload column.
func encodePayload(e Event) (string, error) {
	var v any
	switch e.Kind {
	case KindNew:
		if e.Message == nil {
			return "", fmt.Errorf("watch: new event for %s has no message", e.Ref)
		}
		v = newPayload{Message: e.Message}
	case KindFlags:
		v = flagsPayload{Flags: nonNil(e.Flags), Labels: nonNil(e.Labels)}
	case KindStatus:
		v = statusPayload{State: e.State, Detail: e.Detail}
	case KindGap:
		v = gapPayload{Reason: e.Reason, NextID: e.NextID}
	case KindExpunged:
		return "{}", nil
	default:
		return "", fmt.Errorf("watch: unknown event kind %q", e.Kind)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("watch: encoding %s payload: %w", e.Kind, err)
	}
	return string(b), nil
}

// decodePayload sets the kind-specific fields of e from the payload column.
// e.Kind must already be set.
func decodePayload(e *Event, payload string) error {
	data := []byte(payload)
	var err error
	switch e.Kind {
	case KindNew:
		var p newPayload
		if err = json.Unmarshal(data, &p); err == nil {
			e.Message = p.Message
		}
	case KindFlags:
		var p flagsPayload
		if err = json.Unmarshal(data, &p); err == nil {
			e.Flags, e.Labels = nilIfEmpty(p.Flags), nilIfEmpty(p.Labels)
		}
	case KindStatus:
		var p statusPayload
		if err = json.Unmarshal(data, &p); err == nil {
			e.State, e.Detail = p.State, p.Detail
		}
	case KindGap:
		var p gapPayload
		if err = json.Unmarshal(data, &p); err == nil {
			e.Reason, e.NextID = p.Reason, p.NextID
		}
	case KindExpunged:
	default:
		return fmt.Errorf("watch: event %d has unknown kind %q", e.ID, e.Kind)
	}
	if err != nil {
		return fmt.Errorf("watch: decoding event %d payload: %w", e.ID, err)
	}
	return nil
}

// nonNil returns s, or an empty slice when s is nil, so it encodes as [].
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// nilIfEmpty returns nil for an empty slice, so a decoded [] matches an
// event that never set the field.
func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
