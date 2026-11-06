package mcpserver

import (
	"context"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"testing"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

var update = flag.Bool("update", false, "rewrite testdata/tools_list.golden")

// allowAll is a ListAccounts answer that lists both gated tools. The golden
// is made with it, so it shows update_draft, send_draft and delete_draft as
// destructive, as they are while some account allows expunge.
func allowAll(context.Context, *chainmailv1.ListAccountsRequest) (*chainmailv1.ListAccountsResponse, error) {
	return &chainmailv1.ListAccountsResponse{Accounts: []*chainmailv1.Account{
		{Name: "work", Capabilities: &chainmailv1.AccountCapabilities{AllowExpunge: true, AllowFolderDelete: true}},
	}}, nil
}

func TestToolsListGolden(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, &fakeDaemon{ListAccounts: allowAll}, latest)
	res, err := e.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(res.Tools, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "tools_list.golden")
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("tools/list differs from %s; rerun with -update and review the diff\n%s", path, got)
	}
}

// TestAnnotationsMatchTheSpec pins the spec section 4 table. The table gives
// each tool's hints while no account allows expunge. While one does, the
// daemon expunges the old draft for good, so update_draft, send_draft and
// delete_draft are destructive then, and nothing else changes. read_events,
// added in SP6, is not read-only, by James's decision: it changes no mail,
// but a read with a group moves the group's acknowledged position, so it is
// not idempotent either. cache_status, added in SP7, reads only the
// daemon's own state, so like list_accounts it is not open world.
// The SP13 item tools change only the daemon's own state, so none is open
// world; deletes are soft, so none is destructive; create_item and
// edit_criteria, whose retried add appends again, are not idempotent.
func TestAnnotationsMatchTheSpec(t *testing.T) {
	t.Parallel()
	type h struct{ readOnly, destructive, idempotent, openWorld bool }
	table := map[string]h{
		"list_accounts": {true, false, true, false}, "test_account": {true, false, true, true},
		"list_folders": {true, false, true, true}, "search": {true, false, true, true},
		"get_message": {true, false, true, true}, "get_thread": {true, false, true, true},
		"download_attachment": {true, false, true, true}, "save_raw_message": {true, false, true, true},
		"send_email": {false, false, false, true}, "reply": {false, false, false, true}, "forward": {false, false, false, true},
		"create_draft": {false, false, false, true}, "update_draft": {false, false, false, true},
		"send_draft": {false, false, false, true}, "delete_draft": {false, false, true, true},
		"move": {false, false, true, true}, "copy": {false, false, false, true}, "archive": {false, false, true, true},
		"trash": {false, false, true, true}, "set_flags": {false, false, true, true}, "set_labels": {false, false, true, true},
		"create_folder": {false, false, true, true}, "rename_folder": {false, false, false, true},
		"expunge": {false, true, true, true}, "delete_folder": {false, true, true, true},
		"read_events":  {false, false, false, true},
		"cache_status": {true, false, true, false},
		"create_item":  {false, false, false, false},
		"update_item":  {false, false, true, false},
		"get_item":     {true, false, true, false}, "list_items": {true, false, true, false},
		"delete_item": {false, false, true, false}, "restore_item": {false, false, true, false},
		"edit_criteria": {false, false, false, false}, "tick_criterion": {false, false, true, false},
		"set_blocker": {false, false, true, false}, "link_thread": {false, false, true, false},
		"unlink_thread": {false, false, true, false},
	}
	for name, st := range map[string]struct {
		policy  *gatePolicy
		expunge bool
	}{
		"expunge off":     {policy: &gatePolicy{folderDelete: true}},
		"expunge allowed": {policy: &gatePolicy{expunge: true, folderDelete: true}, expunge: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := maps.Clone(table)
			if st.expunge {
				for _, n := range draftExpungers {
					w := want[n]
					w.destructive = true
					want[n] = w
				}
			} else {
				delete(want, "expunge") // listed only while policy allows it
			}
			e := newTestEnv(t, &fakeDaemon{ListAccounts: st.policy.listAccounts}, latest)
			res, err := e.cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Tools) != len(want) {
				t.Errorf("%d tools listed, want %d", len(res.Tools), len(want))
			}
			for _, tl := range res.Tools {
				a := tl.Annotations
				w, ok := want[tl.Name]
				switch {
				case !ok:
					t.Errorf("unexpected tool %s", tl.Name)
				case tl.Title == "" || a == nil || a.Title != tl.Title || a.DestructiveHint == nil || a.OpenWorldHint == nil:
					t.Errorf("%s: title or annotations missing: %+v", tl.Name, a)
				case (h{a.ReadOnlyHint, *a.DestructiveHint, a.IdempotentHint, *a.OpenWorldHint}) != w:
					t.Errorf("%s: hints %+v, want %+v", tl.Name, h{a.ReadOnlyHint, *a.DestructiveHint, a.IdempotentHint, *a.OpenWorldHint}, w)
				case tl.Description == "" || tl.InputSchema == nil || tl.OutputSchema == nil:
					t.Errorf("%s: description or schemas missing", tl.Name)
				}
			}
		})
	}
}
