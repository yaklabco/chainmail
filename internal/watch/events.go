// Package watch keeps chainmail's sync state and its durable event log in
// state.db: the folder checkpoints and known UIDs that sync passes advance,
// and the events they append, read by watchers through named groups with
// Redis Streams semantics.
package watch

import (
	"encoding/json"
	"errors"
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
	// KindUnreadable is internal: new mail the pass could not read, which
	// the cache records. Only a Filter with Internal set matches it, so
	// the API never shows it.
	KindUnreadable Kind = "unreadable"
	// KindItem is a change to a work item. It has no account, folder or
	// ref, and its payload never holds item text. Readers opt in: a Filter
	// matches it only when Kinds names it or Items is set.
	KindItem Kind = "item"
	// KindItemMail is new mail on a thread linked to a work item, one event
	// per item and message. It carries the new event's account, folder, ref
	// and message. Readers opt in, as for KindItem.
	KindItemMail Kind = "item_mail"
)

// Status is a folder's sync state, as status events and sync_folders rows
// record it. The API and the stored JSON carry it as a plain string.
type Status string

// Status states.
const (
	StateBaselined Status = "baselined"
	// StateCatchingUp is reserved and never published in SP6: the engine
	// runs every catch-up batch inside one Pass.
	StateCatchingUp   Status = "catching_up"
	StateWatching     Status = "watching"
	StatePolling      Status = "polling"
	StateReconnecting Status = "reconnecting"
	StateAuthRequired Status = "auth_required"
	StateError        Status = "error"
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

// ItemChange is the payload of an item event. It never holds item text.
type ItemChange struct {
	// ItemID is the changed item's ID.
	ItemID string `json:"item_id"`
	// Type is the item's type after the change.
	Type string `json:"type"`
	// Revision is the global item revision the change was written at.
	Revision int64 `json:"revision"`
	// Op is what happened: created, updated, moved, deleted, restored,
	// linked, unlinked, criteria or blockers.
	Op string `json:"op"`
	// Fields names the changed fields, for updated and moved.
	Fields []string `json:"fields,omitempty"`
}

// Event is one entry in the log. Which optional fields are set depends on Kind.
type Event struct {
	ID       int64 // assigned by Append; 0 before
	At       time.Time
	Kind     Kind
	Account  string // "" for item and the trimmed gap
	Folder   string
	Ref      string
	Message  *Message    // new, item_mail
	Flags    []string    // flags
	Labels   []string    // flags
	State    Status      // status
	Detail   string      // status
	Reason   string      // gap
	NextID   int64       // gap
	Item     *ItemChange // item
	ItemID   string      // item_mail
	FromSelf bool        // item_mail: the message is from the account's own address
}

// The payload column holds one of these per kind, as JSON. expunged and
// unreadable store {}.
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
	itemPayload struct {
		Item *ItemChange `json:"item"`
	}
	itemMailPayload struct {
		ItemID   string   `json:"item_id"`
		FromSelf bool     `json:"from_self"`
		Message  *Message `json:"message"`
	}
)

// encodePayload renders the kind-specific fields of e as the payload column.
func encodePayload(e Event) (string, error) {
	var v any
	switch e.Kind {
	case KindNew:
		if e.Message == nil {
			return "", fmt.Errorf("new event for %s has no message", e.Ref)
		}
		v = newPayload{Message: e.Message}
	case KindFlags:
		v = flagsPayload{Flags: nonNil(e.Flags), Labels: nonNil(e.Labels)}
	case KindStatus:
		v = statusPayload{State: string(e.State), Detail: e.Detail}
	case KindGap:
		v = gapPayload{Reason: e.Reason, NextID: e.NextID}
	case KindItem:
		if e.Item == nil {
			return "", errors.New("item event has no change")
		}
		v = itemPayload{Item: e.Item}
	case KindItemMail:
		if e.Message == nil {
			return "", fmt.Errorf("item_mail event for %s has no message", e.Ref)
		}
		if e.ItemID == "" {
			return "", fmt.Errorf("item_mail event for %s has no item", e.Ref)
		}
		v = itemMailPayload{ItemID: e.ItemID, FromSelf: e.FromSelf, Message: e.Message}
	case KindExpunged, KindUnreadable:
		return "{}", nil
	default:
		return "", fmt.Errorf("unknown event kind %q", e.Kind)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encoding %s payload: %w", e.Kind, err)
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
			e.State, e.Detail = Status(p.State), p.Detail
		}
	case KindGap:
		var p gapPayload
		if err = json.Unmarshal(data, &p); err == nil {
			e.Reason, e.NextID = p.Reason, p.NextID
		}
	case KindItem:
		var p itemPayload
		if err = json.Unmarshal(data, &p); err == nil {
			e.Item = p.Item
		}
	case KindItemMail:
		var p itemMailPayload
		if err = json.Unmarshal(data, &p); err == nil {
			e.ItemID, e.FromSelf, e.Message = p.ItemID, p.FromSelf, p.Message
		}
	case KindExpunged, KindUnreadable:
	default:
		return fmt.Errorf("event %d has unknown kind %q", e.ID, e.Kind)
	}
	if err != nil {
		return fmt.Errorf("decoding event %d payload: %w", e.ID, err)
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
