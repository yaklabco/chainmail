package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// ActionKind names what an action does. Kinds are stored in the actions
// JSON, so their values never change.
type ActionKind string

// The action kinds.
const (
	ActLabel    ActionKind = "label"
	ActUnlabel  ActionKind = "unlabel"
	ActCopy     ActionKind = "copy"
	ActFlag     ActionKind = "flag"
	ActUnflag   ActionKind = "unflag"
	ActRead     ActionKind = "read"
	ActUnread   ActionKind = "unread"
	ActLinkItem ActionKind = "link_item"
	ActForward  ActionKind = "forward"
	ActMove     ActionKind = "move"
	ActArchive  ActionKind = "archive"
	ActTrash    ActionKind = "trash"
)

// Valid reports whether k is one of the twelve kinds.
func (k ActionKind) Valid() bool {
	switch k {
	case ActLabel, ActUnlabel, ActCopy, ActFlag, ActUnflag, ActRead, ActUnread,
		ActLinkItem, ActForward, ActMove, ActArchive, ActTrash:
		return true
	default:
		return false
	}
}

// Relocates reports whether k changes where a message lives: move, archive
// and trash. A rule holds at most one, last.
func (k ActionKind) Relocates() bool {
	return k == ActMove || k == ActArchive || k == ActTrash
}

// Guarded reports whether k is unsafe to repeat after a crash: copy and
// forward. The runner writes a pending row before a guarded action runs.
func (k ActionKind) Guarded() bool {
	return k == ActCopy || k == ActForward
}

// Action is one step of a rule. Kind says which fields are its arguments:
// Label for label and unlabel, Folder for copy and move, ItemID and Anchor
// for link_item, To and Note for forward. Other kinds take none.
type Action struct {
	Kind   ActionKind
	Label  string
	Folder string
	ItemID string
	Anchor bool
	To     []string
	Note   string
}

// Refusals of stored action JSON. The text is fixed: the JSON holds rule
// text.
var (
	errActionJSON  = &InvalidError{Msg: "an action is not an object with one key", Hint: `write an action as {"kind": argument}, such as {"label":"Receipts"} or {"archive":{}}`}
	errActionKind  = &InvalidError{Msg: "an action kind is not known", Hint: actionKindsHint}
	errActionShape = &InvalidError{Msg: "an action's argument has the wrong shape", Hint: `label, unlabel, copy and move take a string; link_item takes {"item","anchor"}; forward takes {"to","note"}; other kinds take {}`}
	errActionsJSON = &InvalidError{Msg: "the actions are not a JSON array of actions", Hint: `write actions as a JSON array, such as [{"archive":{}}]`}
)

// actionKindsHint lists every kind, for refusals of an unknown one.
const actionKindsHint = "kinds: label, unlabel, copy, flag, unflag, read, unread, link_item, forward, move, archive, trash"

// linkArg is link_item's argument in JSON.
type linkArg struct {
	Item   string `json:"item"`
	Anchor bool   `json:"anchor,omitempty"`
}

// forwardArg is forward's argument in JSON.
type forwardArg struct {
	To   []string `json:"to"`
	Note string   `json:"note,omitempty"`
}

// MarshalJSON writes a as an object with one key, the kind, whose value is
// the kind's argument: a string for label, unlabel, copy and move, an
// object for link_item and forward, and {} for the rest. Fields the kind
// does not take are left out. An unknown kind is an error.
func (a Action) MarshalJSON() ([]byte, error) {
	var arg any
	switch a.Kind {
	case ActLabel, ActUnlabel:
		arg = a.Label
	case ActCopy, ActMove:
		arg = a.Folder
	case ActLinkItem:
		arg = linkArg{Item: a.ItemID, Anchor: a.Anchor}
	case ActForward:
		to := a.To
		if to == nil {
			to = []string{}
		}
		arg = forwardArg{To: to, Note: a.Note}
	case ActFlag, ActUnflag, ActRead, ActUnread, ActArchive, ActTrash:
		arg = struct{}{}
	default:
		return nil, errActionKind
	}
	return json.Marshal(map[string]any{string(a.Kind): arg})
}

// UnmarshalJSON reads the form MarshalJSON writes. It refuses anything but
// an object with exactly one key, an unknown kind, and an argument of the
// wrong shape, with fixed text. It does not check the argument's values;
// Validate does.
func (a *Action) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errActionJSON
	}
	tok, err := dec.Token()
	if err != nil {
		return errActionJSON
	}
	key, ok := tok.(string)
	if !ok {
		return errActionJSON
	}
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return errActionJSON
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return errActionJSON
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errActionJSON
	}
	k := ActionKind(key)
	if !k.Valid() {
		return errActionKind
	}
	next := Action{Kind: k}
	switch k {
	case ActLabel, ActUnlabel:
		if next.Label, err = stringArg(raw); err != nil {
			return err
		}
	case ActCopy, ActMove:
		if next.Folder, err = stringArg(raw); err != nil {
			return err
		}
	case ActLinkItem:
		var l linkArg
		if err := objectArg(raw, &l); err != nil {
			return err
		}
		next.ItemID, next.Anchor = l.Item, l.Anchor
	case ActForward:
		var f forwardArg
		if err := objectArg(raw, &f); err != nil {
			return err
		}
		next.To, next.Note = f.To, f.Note
	default:
		var empty struct{}
		if err := objectArg(raw, &empty); err != nil {
			return err
		}
	}
	*a = next
	return nil
}

// stringArg decodes a JSON string, refusing null and every other type.
func stringArg(raw json.RawMessage) (string, error) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", errActionShape
	}
	return s, nil
}

// objectArg decodes a JSON object into v, refusing null, other types and
// keys v does not have.
func objectArg(raw json.RawMessage, v any) error {
	if len(raw) == 0 || raw[0] != '{' {
		return errActionShape
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		return errActionShape
	}
	return nil
}

// EncodeActions renders as as the JSON array the rules table stores. Nil
// renders as [].
func EncodeActions(as []Action) (string, error) {
	if as == nil {
		as = []Action{}
	}
	b, err := json.Marshal(as)
	if err != nil {
		var inv *InvalidError
		if errors.As(err, &inv) {
			return "", inv
		}
		return "", err
	}
	return string(b), nil
}

// DecodeActions reads what EncodeActions writes. Errors are fixed text.
func DecodeActions(s string) ([]Action, error) {
	if len(s) == 0 || s[0] != '[' {
		return nil, errActionsJSON
	}
	var as []Action
	if err := json.Unmarshal([]byte(s), &as); err != nil {
		var inv *InvalidError
		if errors.As(err, &inv) {
			return nil, inv
		}
		return nil, errActionsJSON
	}
	return as, nil
}
