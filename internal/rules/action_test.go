package rules

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestActionKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind                      ActionKind
		valid, relocates, guarded bool
	}{
		{ActLabel, true, false, false},
		{ActUnlabel, true, false, false},
		{ActCopy, true, false, true},
		{ActFlag, true, false, false},
		{ActUnflag, true, false, false},
		{ActRead, true, false, false},
		{ActUnread, true, false, false},
		{ActLinkItem, true, false, false},
		{ActForward, true, false, true},
		{ActMove, true, true, false},
		{ActArchive, true, true, false},
		{ActTrash, true, true, false},
		{"expunge", false, false, false},
		{"delete_folder", false, false, false},
		{"Label", false, false, false},
		{"", false, false, false},
	}
	for _, tt := range tests {
		if got := tt.kind.Valid(); got != tt.valid {
			t.Errorf("%q.Valid() = %v, want %v", tt.kind, got, tt.valid)
		}
		if got := tt.kind.Relocates(); got != tt.relocates {
			t.Errorf("%q.Relocates() = %v, want %v", tt.kind, got, tt.relocates)
		}
		if got := tt.kind.Guarded(); got != tt.guarded {
			t.Errorf("%q.Guarded() = %v, want %v", tt.kind, got, tt.guarded)
		}
	}
}

func TestActionJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		action Action
		json   string
	}{
		{Action{Kind: ActLabel, Label: "Receipts"}, `{"label":"Receipts"}`},
		{Action{Kind: ActUnlabel, Label: "Later"}, `{"unlabel":"Later"}`},
		{Action{Kind: ActCopy, Folder: "Archive/2026"}, `{"copy":"Archive/2026"}`},
		{Action{Kind: ActMove, Folder: "Receipts"}, `{"move":"Receipts"}`},
		{Action{Kind: ActFlag}, `{"flag":{}}`},
		{Action{Kind: ActUnflag}, `{"unflag":{}}`},
		{Action{Kind: ActRead}, `{"read":{}}`},
		{Action{Kind: ActUnread}, `{"unread":{}}`},
		{Action{Kind: ActArchive}, `{"archive":{}}`},
		{Action{Kind: ActTrash}, `{"trash":{}}`},
		{Action{Kind: ActLinkItem, ItemID: "it_admin_work", Anchor: true}, `{"link_item":{"item":"it_admin_work","anchor":true}}`},
		{Action{Kind: ActLinkItem, ItemID: "it_admin_work"}, `{"link_item":{"item":"it_admin_work"}}`},
		{Action{Kind: ActForward, To: []string{"a@example.com", "b@example.com"}, Note: "see below"}, `{"forward":{"to":["a@example.com","b@example.com"],"note":"see below"}}`},
		{Action{Kind: ActForward, To: []string{"a@example.com"}}, `{"forward":{"to":["a@example.com"]}}`},
	}
	for _, tt := range tests {
		b, err := json.Marshal(tt.action)
		if err != nil {
			t.Errorf("Marshal(%+v): %v", tt.action, err)
			continue
		}
		if string(b) != tt.json {
			t.Errorf("Marshal(%+v) = %s, want %s", tt.action, b, tt.json)
		}
		var got Action
		if err := json.Unmarshal([]byte(tt.json), &got); err != nil {
			t.Errorf("Unmarshal(%s): %v", tt.json, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.action) {
			t.Errorf("Unmarshal(%s) = %+v, want %+v", tt.json, got, tt.action)
		}
	}
}

// Marshal writes only the kind's own arguments, so a stray field never
// reaches state.db.
func TestActionMarshalDropsOtherFields(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Action{Kind: ActArchive, Label: "x", Folder: "y", To: []string{"a@example.com"}})
	if err != nil || string(b) != `{"archive":{}}` {
		t.Errorf("Marshal = %s, %v; want {\"archive\":{}}", b, err)
	}
}

func TestActionMarshalUnknownKind(t *testing.T) {
	t.Parallel()
	_, err := json.Marshal(Action{Kind: "expunge"})
	wantInvalid(t, "unknown kind", err, "an action kind is not known")
}

func TestActionUnmarshalRefuses(t *testing.T) {
	t.Parallel()
	const (
		notObject = "an action is not an object with one key"
		unknown   = "an action kind is not known"
		shape     = "an action's argument has the wrong shape"
	)
	tests := []struct {
		name, json, msg string
	}{
		{"array", `["label","x"]`, notObject},
		{"string", `"archive"`, notObject},
		{"null", `null`, notObject},
		{"empty object", `{}`, notObject},
		{"two keys", `{"label":"x","archive":{}}`, notObject},
		{"same key twice", `{"label":"x","label":"y"}`, notObject},
		{"unknown kind", `{"` + ruleMarker + `":{}}`, unknown},
		{"expunge", `{"expunge":{}}`, unknown},
		{"label null", `{"label":null}`, shape},
		{"label number", `{"label":5}`, shape},
		{"label object", `{"label":{"name":"x"}}`, shape},
		{"move array", `{"move":["x"]}`, shape},
		{"archive string", `{"archive":"x"}`, shape},
		{"archive null", `{"archive":null}`, shape},
		{"archive with a key", `{"archive":{"folder":"x"}}`, shape},
		{"flag true", `{"flag":true}`, shape},
		{"link_item string", `{"link_item":"it_admin_work"}`, shape},
		{"link_item unknown key", `{"link_item":{"item":"it_admin_work","note":"x"}}`, shape},
		{"link_item anchor string", `{"link_item":{"item":"it_admin_work","anchor":"yes"}}`, shape},
		{"forward string", `{"forward":"a@example.com"}`, shape},
		{"forward to string", `{"forward":{"to":"a@example.com"}}`, shape},
		{"forward unknown key", `{"forward":{"to":["a@example.com"],"cc":["b@example.com"]}}`, shape},
	}
	for _, tt := range tests {
		var a Action
		err := json.Unmarshal([]byte(tt.json), &a)
		wantInvalid(t, tt.name, err, tt.msg)
	}
}

func TestEncodeDecodeActions(t *testing.T) {
	t.Parallel()
	as := []Action{
		{Kind: ActLabel, Label: "Receipts"},
		{Kind: ActLinkItem, ItemID: "it_01jcz8v2kq4f9w3m7xg5t6h8ya", Anchor: true},
		{Kind: ActForward, To: []string{"a@example.com"}, Note: "fyi"},
		{Kind: ActArchive},
	}
	s, err := EncodeActions(as)
	if err != nil {
		t.Fatalf("EncodeActions: %v", err)
	}
	const want = `[{"label":"Receipts"},{"link_item":{"item":"it_01jcz8v2kq4f9w3m7xg5t6h8ya","anchor":true}},{"forward":{"to":["a@example.com"],"note":"fyi"}},{"archive":{}}]`
	if s != want {
		t.Errorf("EncodeActions = %s\nwant %s", s, want)
	}
	got, err := DecodeActions(s)
	if err != nil {
		t.Fatalf("DecodeActions: %v", err)
	}
	if !reflect.DeepEqual(got, as) {
		t.Errorf("DecodeActions = %+v\nwant %+v", got, as)
	}
	if s, err := EncodeActions(nil); err != nil || s != "[]" {
		t.Errorf("EncodeActions(nil) = %q, %v; want []", s, err)
	}
	if got, err := DecodeActions("[]"); err != nil || len(got) != 0 {
		t.Errorf("DecodeActions([]) = %+v, %v; want none", got, err)
	}
}

func TestEncodeActionsUnknownKind(t *testing.T) {
	t.Parallel()
	_, err := EncodeActions([]Action{{Kind: ActRead}, {Kind: "expunge"}})
	wantInvalid(t, "encode", err, "an action kind is not known")
}

func TestDecodeActionsRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, msg string
	}{
		{"empty", ``, "the actions are not a JSON array of actions"},
		{"object", `{"archive":{}}`, "the actions are not a JSON array of actions"},
		{"null", `null`, "the actions are not a JSON array of actions"},
		{"truncated", `[{"archive":{}}`, "the actions are not a JSON array of actions"},
		{"trailing", `[{"archive":{}}] x`, "the actions are not a JSON array of actions"},
		{"bad action", `[{"archive":{}},{"` + ruleMarker + `":{}}]`, "an action kind is not known"},
		{"bad shape", `[{"label":5}]`, "an action's argument has the wrong shape"},
	}
	for _, tt := range tests {
		_, err := DecodeActions(tt.in)
		wantInvalid(t, tt.name, err, tt.msg)
	}
}
