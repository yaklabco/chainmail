package mcpserver

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
)

// toolRPC maps every tool to the one RPC it is for.
var toolRPC = map[string]string{
	"list_accounts":       chainmailv1connect.AccountServiceListAccountsProcedure,
	"test_account":        chainmailv1connect.AccountServiceTestAccountProcedure,
	"list_folders":        chainmailv1connect.MessageServiceListFoldersProcedure,
	"search":              chainmailv1connect.MessageServiceSearchProcedure,
	"get_message":         chainmailv1connect.MessageServiceGetProcedure,
	"get_thread":          chainmailv1connect.MessageServiceGetThreadProcedure,
	"download_attachment": chainmailv1connect.MessageServiceDownloadAttachmentProcedure,
	"save_raw_message":    chainmailv1connect.MessageServiceGetRawProcedure,
	"send_email":          chainmailv1connect.ComposeServiceSendProcedure,
	"reply":               chainmailv1connect.ComposeServiceReplyProcedure,
	"forward":             chainmailv1connect.ComposeServiceForwardProcedure,
	"create_draft":        chainmailv1connect.ComposeServiceCreateDraftProcedure,
	"update_draft":        chainmailv1connect.ComposeServiceUpdateDraftProcedure,
	"send_draft":          chainmailv1connect.ComposeServiceSendDraftProcedure,
	"delete_draft":        chainmailv1connect.ComposeServiceDeleteDraftProcedure,
	"move":                chainmailv1connect.OrganizeServiceMoveProcedure,
	"copy":                chainmailv1connect.OrganizeServiceCopyProcedure,
	"archive":             chainmailv1connect.OrganizeServiceArchiveProcedure,
	"trash":               chainmailv1connect.OrganizeServiceTrashProcedure,
	"set_flags":           chainmailv1connect.OrganizeServiceSetFlagsProcedure,
	"set_labels":          chainmailv1connect.OrganizeServiceSetLabelsProcedure,
	"create_folder":       chainmailv1connect.OrganizeServiceCreateFolderProcedure,
	"rename_folder":       chainmailv1connect.OrganizeServiceRenameFolderProcedure,
	"expunge":             chainmailv1connect.OrganizeServiceExpungeProcedure,
	"delete_folder":       chainmailv1connect.OrganizeServiceDeleteFolderProcedure,
	"read_events":         chainmailv1connect.WatchServiceReadEventsProcedure,
	"cache_status":        chainmailv1connect.CacheServiceGetCacheStatusProcedure,
	"create_item":         chainmailv1connect.ItemsServiceCreateItemProcedure,
	"update_item":         chainmailv1connect.ItemsServiceUpdateItemProcedure,
	"get_item":            chainmailv1connect.ItemsServiceGetItemProcedure,
	"list_items":          chainmailv1connect.ItemsServiceListItemsProcedure,
	"delete_item":         chainmailv1connect.ItemsServiceDeleteItemProcedure,
	"restore_item":        chainmailv1connect.ItemsServiceRestoreItemProcedure,
	"edit_criteria":       chainmailv1connect.ItemsServiceEditCriteriaProcedure,
	"tick_criterion":      chainmailv1connect.ItemsServiceTickCriterionProcedure,
	"set_blocker":         chainmailv1connect.ItemsServiceSetBlockerProcedure,
	"link_thread":         chainmailv1connect.ItemsServiceLinkThreadProcedure,
	"unlink_thread":       chainmailv1connect.ItemsServiceUnlinkThreadProcedure,
}

// leftOut lists agent-facing RPCs with no tool, and why (spec section 4).
var leftOut = map[string]string{
	chainmailv1connect.AccountServiceProbeLimitsProcedure:      "an operator diagnostic that opens connections until the server refuses one",
	chainmailv1connect.ComposeServiceSendRawProcedure:          "sends arbitrary headers; agents compose with send_email",
	chainmailv1connect.ComposeServiceUploadAttachmentProcedure: "the compose tools stream attachment files with it; it is not a tool of its own",
	chainmailv1connect.WatchServiceSubscribeProcedure:          "a stream for long-running watchers; agents poll with read_events",
	chainmailv1connect.WatchServiceAckProcedure:                "read_events with a group acknowledges what it returns",
	chainmailv1connect.WatchServiceGetWatchStatusProcedure:     "an operator diagnostic",
	chainmailv1connect.WatchServiceListGroupsProcedure:         "group administration for operators",
	chainmailv1connect.WatchServiceDeleteGroupProcedure:        "group administration for operators",
	chainmailv1connect.CacheServiceRebuildCacheProcedure:       "an operator action that drops cached mail and downloads it again",
	chainmailv1connect.CacheServiceEvictCacheProcedure:         "an operator action; the daemon evicts bodies on its own",
}

// ignored lists request fields no tool sets, keyed "<procedure> <field path>", and why.
var ignored = map[string]string{
	chainmailv1connect.MessageServiceSearchProcedure + " query":    "the typed query tree; query (query_string) covers every operator",
	chainmailv1connect.ComposeServiceSendProcedure + " dry_run":    "returns raw MIME, which agents do not need; create_draft previews a message",
	chainmailv1connect.ComposeServiceSendProcedure + " draft":      "create_draft saves a new message to Drafts",
	chainmailv1connect.ComposeServiceReplyProcedure + " dry_run":   "as Send dry_run",
	chainmailv1connect.ComposeServiceForwardProcedure + " dry_run": "as Send dry_run",
}

// agentServices are the services the coverage test holds to account.
var agentServices = []protoreflect.ServiceDescriptor{
	chainmailv1.File_chainmail_v1_account_proto.Services().ByName("AccountService"),
	chainmailv1.File_chainmail_v1_message_proto.Services().ByName("MessageService"),
	chainmailv1.File_chainmail_v1_compose_proto.Services().ByName("ComposeService"),
	chainmailv1.File_chainmail_v1_organize_proto.Services().ByName("OrganizeService"),
	chainmailv1.File_chainmail_v1_watch_proto.Services().ByName("WatchService"),
	chainmailv1.File_chainmail_v1_cache_proto.Services().ByName("CacheService"),
	chainmailv1.File_chainmail_v1_items_proto.Services().ByName("ItemsService"),
}

func procedure(sd protoreflect.ServiceDescriptor, md protoreflect.MethodDescriptor) string {
	return "/" + string(sd.FullName()) + "/" + string(md.Name())
}

// fieldPaths lists the leaf field paths of md, descending into chainmail.v1
// messages (lists of them too) and stopping at ignored paths and cycles.
func fieldPaths(proc string, md protoreflect.MessageDescriptor, prefix string, seen map[protoreflect.FullName]bool, out *[]string) {
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		path := prefix + string(fd.Name())
		if _, ok := ignored[proc+" "+path]; ok {
			*out = append(*out, path)
			continue
		}
		if fd.Kind() == protoreflect.MessageKind && !fd.IsMap() && fd.Message().ParentFile().Package() == "chainmail.v1" && !seen[fd.Message().FullName()] {
			seen[fd.Message().FullName()] = true
			fieldPaths(proc, fd.Message(), path+".", seen, out)
			delete(seen, fd.Message().FullName())
			continue
		}
		*out = append(*out, path)
	}
}

// setPaths records the field paths m has set: every leaf, and every message
// or message list on the way to one.
func setPaths(m protoreflect.Message, prefix string, out map[string]bool) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		path := prefix + string(fd.Name())
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
			out[path] = true
			for i := range v.List().Len() {
				setPaths(v.List().Get(i).Message(), path+".", out)
			}
		case fd.Kind() == protoreflect.MessageKind && !fd.IsMap() && !fd.IsList():
			out[path] = true // so an ignored message field that a tool now sets is caught
			setPaths(v.Message(), path+".", out)
		default:
			out[path] = true
		}
		return true
	})
}

func TestEveryAgentRPCHasAToolOrAReason(t *testing.T) {
	t.Parallel()
	mapped := map[string]bool{}
	for _, p := range toolRPC {
		mapped[p] = true
	}
	known := map[string]bool{}
	for _, sd := range agentServices {
		methods := sd.Methods()
		for i := range methods.Len() {
			p := procedure(sd, methods.Get(i))
			known[p] = true
			_, out := leftOut[p]
			if mapped[p] == out {
				t.Errorf("%s: mapped=%t, left out=%t; want exactly one", p, mapped[p], out)
			}
		}
	}
	for p := range maps.Keys(leftOut) {
		if !known[p] {
			t.Errorf("left-out entry %s is not an RPC", p)
		}
	}
	e := newTestEnv(t, &fakeDaemon{ListAccounts: allowAll}, latest)
	if got, want := listToolNames(t, e.cs), slices.Sorted(maps.Keys(toolRPC)); !slices.Equal(got, want) {
		t.Errorf("tools/list = %v, toolRPC = %v", got, want)
	}
}

// coverageSamples gives each tool inputs that together set every mapped field.
func coverageSamples(file string) map[string][]map[string]any {
	content := map[string]any{
		"body": "hi", "attachments": []string{file}, "inline": []map[string]string{{"name": "c.png", "path": file}},
		"no_signature": true, "headers": map[string]string{"X-Tag": "1"},
	}
	with := func(base map[string]any, extra ...map[string]any) map[string]any {
		out := maps.Clone(base)
		for _, e := range extra {
			maps.Copy(out, e)
		}
		return out
	}
	recips := map[string]any{"to": []string{"a@x.test"}, "cc": []string{"c@x.test"}, "bcc": []string{"b@x.test"}}
	text := map[string]any{"body": "plain", "body_format": "text"}
	html := map[string]any{"body": "<p>x</p>", "body_format": "html", "html_text": "x"}
	return map[string][]map[string]any{
		"list_accounts":       {{}},
		"test_account":        {{"account": "work"}},
		"list_folders":        {{"accounts": []string{"work"}}},
		"search":              {{"query": "x", "accounts": []string{"work"}, "limit": 5, "cursor": "c1.x", "count_only": true, "snippets": true, "threads": true, "source": "cache"}},
		"get_message":         {{"refs": []string{testRef}, "full": true}},
		"get_thread":          {{"ref": testRef, "full": true}},
		"download_attachment": {{"ref": testRef, "part": "2"}},
		"save_raw_message":    {{"ref": testRef}},
		"send_email":          {with(content, recips, map[string]any{"account": "work", "subject": "s", "idempotency_key": "k"}), with(recips, text, map[string]any{"account": "work", "subject": "s"}), with(recips, html, map[string]any{"account": "work", "subject": "s"})},
		"reply":               {with(content, recips, map[string]any{"ref": testRef, "all": true, "draft": true, "idempotency_key": "k"}), with(text, map[string]any{"ref": testRef}), with(html, map[string]any{"ref": testRef})},
		"forward":             {with(content, recips, map[string]any{"ref": testRef, "no_attachments": true, "draft": true, "idempotency_key": "k"}), with(text, map[string]any{"ref": testRef, "to": []string{"a@x.test"}}), with(html, map[string]any{"ref": testRef, "to": []string{"a@x.test"}})},
		"create_draft":        {with(content, recips, map[string]any{"account": "work", "subject": "s"}), with(text, map[string]any{"account": "work"}), with(html, map[string]any{"account": "work"})},
		"update_draft":        {with(content, recips, map[string]any{"ref": testRef, "subject": "s"}), with(text, map[string]any{"ref": testRef}), with(html, map[string]any{"ref": testRef})},
		"send_draft":          {{"ref": testRef, "idempotency_key": "k"}},
		"delete_draft":        {{"ref": testRef}},
		"move":                {{"refs": []string{testRef}, "folder": "F"}},
		"copy":                {{"refs": []string{testRef}, "folder": "F"}},
		"archive":             {{"refs": []string{testRef}}},
		"trash":               {{"refs": []string{testRef}}},
		"set_flags":           {{"refs": []string{testRef}, "add": []string{"seen"}, "remove": []string{"flagged"}}},
		"set_labels":          {{"refs": []string{testRef}, "add": []string{"a"}, "remove": []string{"b"}}},
		"create_folder":       {{"account": "work", "name": "F"}},
		"rename_folder":       {{"account": "work", "from": "F", "to": "G"}},
		"expunge":             {{"refs": []string{testRef}}},
		"delete_folder":       {{"account": "work", "name": "F", "force": true}},
		"read_events": {
			{"group": "triage", "kinds": []string{"new", "flags"}, "accounts": []string{"work"}, "in": "inbox", "item": "it_a", "wait_seconds": 1, "limit": 5},
			{"cursor": 7},
		},
		"cache_status": {{"accounts": []string{"work"}}},
		"create_item": {{
			"type": "task", "title": "t", "description": "d", "state": "backlog", "area": "work", "priority": "p1",
			"start_date": "2026-11-01", "due_date": "2026-11-30", "estimate": 2.5, "parent_id": "it_admin_work",
			"criteria": []string{"c"}, "idempotency_key": "k",
		}},
		"update_item": {{
			"id": "it_a", "type": "story", "title": "t", "description": "d", "state": "in-progress", "area": "work",
			"priority": "p1", "start_date": "2026-11-01", "due_date": "2026-11-30", "estimate": 2.5, "parent_id": "it_b",
		}},
		"get_item": {{"id": "it_a"}},
		"list_items": {{
			"type": "task", "state": "backlog", "area": "work", "under": "it_a", "inbox": true, "ongoing": false,
			"include_deleted": true, "changed_since": 3, "limit": 10, "page_token": "p",
		}},
		"delete_item":  {{"id": "it_a", "cascade": true}},
		"restore_item": {{"id": "it_a", "cascade": true}},
		"edit_criteria": {{
			"item_id": "it_a", "remove": []string{"cr_a"}, "edit": []map[string]string{{"id": "cr_b", "text": "t"}},
			"order": []string{"cr_b"}, "add": []string{"n"},
		}},
		"tick_criterion": {{"item_id": "it_a", "criterion_id": "cr_a", "ticked": true, "evidence": "e"}},
		"set_blocker":    {{"item_id": "it_a", "blocked_by_id": "it_b", "linked": true}},
		"link_thread":    {{"item_id": "it_a", "ref": testRef, "anchor": true, "note": "n"}},
		"unlink_thread":  {{"item_id": "it_a", "account": "work", "thread_key": "1a2b"}},
	}
}

func TestEveryRequestFieldIsMappedOrIgnored(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "c.png")
	if err := os.WriteFile(file, []byte("\x89PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeDaemon{
		ListAccounts: allowAll,
		UploadAttachment: func(_ context.Context, st *connect.ClientStream[chainmailv1.UploadAttachmentRequest]) (*chainmailv1.UploadAttachmentResponse, error) {
			parts := 0
			for st.Receive() {
				parts++
			}
			return &chainmailv1.UploadAttachmentResponse{Handle: "h", Sha256: strings.Repeat("a", 64), Size: int64(parts)}, st.Err()
		},
	}
	e := newTestEnv(t, f, latest)
	samples := coverageSamples(file)
	set := map[string]map[string]bool{} // procedure -> field paths set
	for tool, proc := range toolRPC {
		if len(samples[tool]) == 0 {
			t.Errorf("no coverage sample for %s", tool)
			continue
		}
		set[proc] = map[string]bool{}
		for _, args := range samples[tool] {
			before := len(e.fake.Calls())
			e.call(t, tool, args) // unfaked RPCs answer Unimplemented; the request is recorded first
			calls := e.fake.Calls()[before:]
			hit := false
			for _, c := range calls {
				if c.Procedure == proc && c.Msg != nil {
					hit = true
					setPaths(c.Msg.ProtoReflect(), "", set[proc])
				}
			}
			if !hit {
				t.Errorf("%s did not call %s", tool, proc)
			}
		}
	}
	exists := map[string]bool{} // "<procedure> <field path>" of every leaf the RPCs have
	for _, sd := range agentServices {
		methods := sd.Methods()
		for i := range methods.Len() {
			md := methods.Get(i)
			proc := procedure(sd, md)
			if _, out := leftOut[proc]; out {
				continue
			}
			var paths []string
			fieldPaths(proc, md.Input(), "", map[protoreflect.FullName]bool{}, &paths)
			for _, p := range paths {
				exists[proc+" "+p] = true
				_, ign := ignored[proc+" "+p]
				switch {
				case set[proc][p] && ign:
					t.Errorf("%s %s is set by its tool but on the ignore list", proc, p)
				case !set[proc][p] && !ign:
					t.Errorf("%s %s is neither mapped by its tool nor ignored with a reason", proc, p)
				}
			}
		}
	}
	for k := range maps.Keys(ignored) {
		proc, _, _ := strings.Cut(k, " ")
		if _, ok := set[proc]; !ok {
			t.Errorf("ignore entry %q names an RPC with no tool", k)
		}
		if !exists[k] {
			t.Errorf("ignore entry %q names no request field", k)
		}
	}
}

// TestEveryExclusionHasAReason keeps the left-out RPCs and ignored fields
// honest: an entry must say why, or it is not an exclusion.
func TestEveryExclusionHasAReason(t *testing.T) {
	t.Parallel()
	for name, list := range map[string]map[string]string{"leftOut": leftOut, "ignored": ignored} {
		for k, why := range list {
			if strings.TrimSpace(why) == "" {
				t.Errorf("%s entry %q has no reason", name, k)
			}
		}
	}
}
