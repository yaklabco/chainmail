package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

// gmailAllMail is where the fake Gmail account keeps its messages.
const gmailAllMail = "[Gmail]/All Mail"

// orgTargets decodes an audit row's targets.
func orgTargets(t *testing.T, row daemontest.AuditRow) []string {
	t.Helper()
	var out []string
	if err := json.Unmarshal([]byte(row.Targets), &out); err != nil {
		t.Fatalf("targets %q: %v", row.Targets, err)
	}
	return out
}

// orgRows returns the audit rows of action, in insertion order.
func orgRows(t *testing.T, h *daemontest.Harness, action string) []daemontest.AuditRow {
	t.Helper()
	var out []daemontest.AuditRow
	for _, r := range h.AuditRows(t) {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// strs renders refs in their canonical form.
func strs(refs ...ref.Ref) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.String()
	}
	return out
}

// checkOK fails the test unless every outcome succeeded, and returns the
// outcomes' new refs.
func checkOK(t *testing.T, refs []string, outs []*chainmailv1.RefOutcome) []string {
	t.Helper()
	if len(outs) != len(refs) {
		t.Fatalf("got %d outcomes for %d refs", len(outs), len(refs))
	}
	newRefs := make([]string, len(outs))
	for i, o := range outs {
		if o.GetRef() != refs[i] {
			t.Errorf("outcome %d ref = %q, want %q", i, o.GetRef(), refs[i])
		}
		if o.GetError() != nil {
			t.Errorf("outcome %d error = %v", i, o.GetError())
		}
		newRefs[i] = o.GetNewRef()
	}
	return newRefs
}

func mustParse(t *testing.T, s string) ref.Ref {
	t.Helper()
	r, err := ref.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return r
}

func TestOrganizeMove(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.AddFolder("Projects", mailstore.RoleNone)
	r := m.host.Add(fake.Message{Folder: "INBOX", Subject: "a"})
	refs := strs(r)
	resp, err := m.Client.Organize.Move(t.Context(), connect.NewRequest(&chainmailv1.MoveRequest{Refs: refs, Folder: "Projects"}))
	if err != nil {
		t.Fatal(err)
	}
	newRefs := checkOK(t, refs, resp.Msg.GetOutcomes())
	if got := mustParse(t, newRefs[0]); got.Folder != "Projects" {
		t.Errorf("new ref = %v, want one in Projects", got)
	}
	rows := orgRows(t, m.Harness, "move")
	if len(rows) != 1 || rows[0].Account != "host" || rows[0].Result != "ok" || rows[0].Client != "test" {
		t.Fatalf("audit = %+v", rows)
	}
	if got, want := orgTargets(t, rows[0]), append(slices.Clone(refs), "folder=Projects"); !slices.Equal(got, want) {
		t.Errorf("targets = %q, want %q", got, want)
	}
}

func TestOrganizeMoveRefusesBadFolder(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	refs := strs(m.host.Add(fake.Message{Folder: "INBOX"}))
	for _, folder := range []string{"", "a\x01b", strings.Repeat("f", 256)} {
		_, err := m.Client.Organize.Move(t.Context(), connect.NewRequest(&chainmailv1.MoveRequest{Refs: refs, Folder: folder}))
		if reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("folder %q: err = %v, want INVALID_ARGUMENT", folder, err)
		}
		_, err = m.Client.Organize.Copy(t.Context(), connect.NewRequest(&chainmailv1.CopyRequest{Refs: refs, Folder: folder}))
		if reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("copy folder %q: err = %v, want INVALID_ARGUMENT", folder, err)
		}
	}
	if n := m.host.Calls("Move") + m.host.Calls("Copy"); n != 0 {
		t.Errorf("store called %d times for refused requests", n)
	}
	rows := orgRows(t, m.Harness, "move")
	if len(rows) != 3 || rows[0].Result != "error" || rows[0].Reason != api.ReasonInvalidArgument || rows[0].Account != "host" {
		t.Fatalf("audit = %+v", rows)
	}
	if got := orgTargets(t, rows[1]); !slices.Equal(got, []string{refs[0], "(invalid)"}) {
		t.Errorf("targets = %q", got)
	}
}

func TestOrganizeRefusalAuditsEachAccount(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	host := m.host.Add(fake.Message{Folder: "INBOX"}).String()
	gm := m.gmail.Add(fake.Message{Folder: gmailAllMail}).String()
	refs := []string{host, "cm1:bad", gm}
	_, err := m.Client.Organize.SetFlags(t.Context(), connect.NewRequest(&chainmailv1.SetFlagsRequest{Refs: refs, Add: []string{`\Deleted`}}))
	if reasonOf(err) != api.ReasonInvalidArgument {
		t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
	}
	if n := m.host.Calls("StoreFlags") + m.gmail.Calls("StoreFlags"); n != 0 {
		t.Errorf("StoreFlags called %d times for a refused request", n)
	}
	rows := orgRows(t, m.Harness, "flags")
	if len(rows) != 3 {
		t.Fatalf("audit = %+v, want one row per account", rows)
	}
	want := []struct {
		account string
		targets []string
	}{
		{"", []string{"(invalid)"}},
		{"gmail", []string{gm}},
		{"host", []string{host}},
	}
	for i, w := range want {
		r := rows[i]
		if r.Account != w.account || r.Result != "error" || r.Reason != api.ReasonInvalidArgument || !slices.Equal(orgTargets(t, r), w.targets) {
			t.Errorf("row %d = %+v, want account %q targets %q", i, r, w.account, w.targets)
		}
	}
}

func TestOrganizeCopy(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.gmail.AddFolder("Work", mailstore.RoleNone)
	host := m.host.Add(fake.Message{Folder: "INBOX"})
	gm := m.gmail.Add(fake.Message{Folder: gmailAllMail, Labels: []string{`\Inbox`}})
	refs := strs(host, gm)
	for _, folder := range []string{"Archive", "Work"} {
		// Each account has only one of the two folders; the other ref fails
		// on its own.
		resp, err := m.Client.Organize.Copy(t.Context(), connect.NewRequest(&chainmailv1.CopyRequest{Refs: refs, Folder: folder}))
		if err != nil {
			t.Fatal(err)
		}
		outs := resp.Msg.GetOutcomes()
		ok, bad := outs[0], outs[1]
		if folder == "Work" {
			ok, bad = outs[1], outs[0]
		}
		if ok.GetError() != nil || bad.GetError().GetReason() != api.ReasonNotFound {
			t.Errorf("copy to %s: outcomes = %v", folder, outs)
		}
		if folder == "Archive" && mustParse(t, ok.GetNewRef()).Folder != "Archive" {
			t.Errorf("host copy new ref = %q", ok.GetNewRef())
		}
		// On Gmail a copy to a label keeps the ref, so new_ref stays empty.
		if folder == "Work" && ok.GetNewRef() != "" {
			t.Errorf("gmail copy new ref = %q, want empty", ok.GetNewRef())
		}
	}
	if rows := orgRows(t, m.Harness, "copy"); len(rows) != 4 {
		t.Errorf("audit rows = %+v, want one per account per call", rows)
	}
}

func TestOrganizeArchiveTrashExpunge(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	host := m.host.Add(fake.Message{Folder: "INBOX"})
	gm := m.gmail.Add(fake.Message{Folder: gmailAllMail, Labels: []string{`\Inbox`}})

	resp, err := m.Client.Organize.Archive(t.Context(), connect.NewRequest(&chainmailv1.ArchiveRequest{Refs: strs(host, gm)}))
	if err != nil {
		t.Fatal(err)
	}
	newRefs := checkOK(t, strs(host, gm), resp.Msg.GetOutcomes())
	if mustParse(t, newRefs[0]).Folder != "Archive" || newRefs[1] != "" {
		t.Errorf("archive new refs = %q", newRefs)
	}
	archived := mustParse(t, newRefs[0])

	tr, err := m.Client.Organize.Trash(t.Context(), connect.NewRequest(&chainmailv1.TrashRequest{Refs: strs(archived)}))
	if err != nil {
		t.Fatal(err)
	}
	trashed := mustParse(t, checkOK(t, strs(archived), tr.Msg.GetOutcomes())[0])
	if trashed.Folder != "Trash" {
		t.Errorf("trash new ref = %v", trashed)
	}

	ex, err := m.Client.Organize.Expunge(t.Context(), connect.NewRequest(&chainmailv1.ExpungeRequest{Refs: strs(trashed, gm)}))
	if err != nil {
		t.Fatal(err)
	}
	if got := checkOK(t, strs(trashed, gm), ex.Msg.GetOutcomes()); !slices.Equal(got, []string{"", ""}) {
		t.Errorf("expunge new refs = %q, want empty", got)
	}
	if res := m.host.FetchMessages(t.Context(), []ref.Ref{trashed}); res[0].Err == nil {
		t.Error("expunged message still readable")
	}
	for _, action := range []string{"archive", "trash", "expunge"} {
		for _, row := range orgRows(t, m.Harness, action) {
			if row.Result != "ok" {
				t.Errorf("%s audit = %+v", action, row)
			}
		}
	}
}

func TestOrganizeSetFlags(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{Folder: "INBOX", Flags: []string{`\Answered`}})
	refs := strs(r)
	req := &chainmailv1.SetFlagsRequest{Refs: refs, Add: []string{`\seen`, `\Flagged`, "$Label1", "work.todo"}, Remove: []string{`\Answered`}}
	resp, err := m.Client.Organize.SetFlags(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if got := checkOK(t, refs, resp.Msg.GetOutcomes()); got[0] != "" {
		t.Errorf("new ref = %q, want empty", got[0])
	}
	got := m.host.Flags(r)
	for _, want := range []string{`\Seen`, `\Flagged`, "$Label1", "work.todo"} {
		if !slices.Contains(got, want) {
			t.Errorf("flags = %q, missing %q", got, want)
		}
	}
	if slices.Contains(got, `\Answered`) {
		t.Errorf("flags = %q, still has \\Answered", got)
	}
	if rows := orgRows(t, m.Harness, "flags"); len(rows) != 1 || rows[0].Account != "host" || rows[0].Result != "ok" {
		t.Errorf("audit = %+v", rows)
	}
}

func TestOrganizeSetFlagsRefusesBadFlags(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	refs := strs(m.host.Add(fake.Message{Folder: "INBOX"}))
	tests := []struct {
		name        string
		add, remove []string
	}{
		{name: "deleted", add: []string{`\Deleted`}},
		{name: "deleted any case", add: []string{`\deleted`}},
		{name: "remove deleted", remove: []string{`\Deleted`}},
		{name: "recent", add: []string{`\Recent`}},
		{name: "unknown system flag", add: []string{`\Important`}},
		{name: "space in keyword", add: []string{"a b"}},
		{name: "empty keyword", add: []string{""}},
		{name: "long keyword", add: []string{strings.Repeat("k", 65)}},
		{name: "bracket in keyword", remove: []string{"a]"}},
		{name: "nothing to change"},
	}
	for _, tt := range tests {
		_, err := m.Client.Organize.SetFlags(t.Context(), connect.NewRequest(&chainmailv1.SetFlagsRequest{Refs: refs, Add: tt.add, Remove: tt.remove}))
		if reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("%s: err = %v, want INVALID_ARGUMENT", tt.name, err)
		}
	}
	_, err := m.Client.Organize.SetFlags(t.Context(), connect.NewRequest(&chainmailv1.SetFlagsRequest{Refs: refs, Add: []string{`\Deleted`}}))
	if d := api.Detail(err); !strings.Contains(d.GetHint(), "expunge") {
		t.Errorf(`\Deleted hint = %q, want it to point at expunge`, d.GetHint())
	}
	if n := m.host.Calls("StoreFlags"); n != 0 {
		t.Errorf("StoreFlags called %d times for refused requests", n)
	}
	if rows := orgRows(t, m.Harness, "flags"); len(rows) != len(tests)+1 || rows[0].Result != "error" || rows[0].Reason != api.ReasonInvalidArgument {
		t.Errorf("audit = %+v", rows)
	}
}

func TestOrganizeSetLabels(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.gmail.AddFolder("Work", mailstore.RoleNone)
	gm := m.gmail.Add(fake.Message{Folder: gmailAllMail, Labels: []string{`\Inbox`}})
	h1 := m.host.Add(fake.Message{Folder: "INBOX"})
	h2 := m.host.Add(fake.Message{Folder: "INBOX"})
	refs := strs(h1, gm, h2)
	resp, err := m.Client.Organize.SetLabels(t.Context(), connect.NewRequest(&chainmailv1.SetLabelsRequest{Refs: refs, Add: []string{"Work"}, Remove: []string{`\Inbox`}}))
	if err != nil {
		t.Fatal(err)
	}
	outs := resp.Msg.GetOutcomes()
	if len(outs) != 3 {
		t.Fatalf("outcomes = %v", outs)
	}
	if outs[1].GetError() != nil || outs[1].GetNewRef() != "" {
		t.Errorf("gmail outcome = %v", outs[1])
	}
	for _, i := range []int{0, 2} {
		if e := outs[i].GetError(); e.GetReason() != api.ReasonInvalidArgument || e.GetMessage() != "the account does not support that action" {
			t.Errorf("host outcome %d = %v, want INVALID_ARGUMENT", i, outs[i])
		}
	}
	rows := orgRows(t, m.Harness, "labels")
	if len(rows) != 2 {
		t.Fatalf("audit = %+v", rows)
	}
	if rows[0].Account != "gmail" || rows[0].Result != "ok" || rows[1].Account != "host" || rows[1].Result != "error" || rows[1].Reason != api.ReasonInvalidArgument {
		t.Errorf("audit = %+v", rows)
	}
	if got := orgTargets(t, rows[1]); !slices.Equal(got, strs(h1, h2)) {
		t.Errorf("host targets = %q", got)
	}
	for _, bad := range [][]string{{""}, {"a\nb"}, {strings.Repeat("l", 256)}} {
		_, err := m.Client.Organize.SetLabels(t.Context(), connect.NewRequest(&chainmailv1.SetLabelsRequest{Refs: refs, Add: bad}))
		if reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("label %q: err = %v, want INVALID_ARGUMENT", bad, err)
		}
	}
}

func TestOrganizeMixedAccountsKeepOrder(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	var refs []string
	for i := range 3 {
		refs = append(refs, m.host.Add(fake.Message{Folder: "INBOX", Subject: fmt.Sprint("h", i)}).String())
		refs = append(refs, m.gmail.Add(fake.Message{Folder: gmailAllMail, Labels: []string{`\Inbox`}}).String())
	}
	resp, err := m.Client.Organize.Trash(t.Context(), connect.NewRequest(&chainmailv1.TrashRequest{Refs: refs}))
	if err != nil {
		t.Fatal(err)
	}
	for i, nr := range checkOK(t, refs, resp.Msg.GetOutcomes()) {
		was, now := mustParse(t, refs[i]), mustParse(t, nr)
		if now.Account != was.Account || !strings.HasSuffix(now.Folder, "Trash") {
			t.Errorf("outcome %d: %v moved to %v", i, was, now)
		}
	}
	rows := orgRows(t, m.Harness, "trash")
	if len(rows) != 2 || rows[0].Account != "gmail" || rows[1].Account != "host" {
		t.Fatalf("audit = %+v, want one row per account", rows)
	}
	if got := orgTargets(t, rows[1]); !slices.Equal(got, []string{refs[0], refs[2], refs[4]}) {
		t.Errorf("host targets = %q", got)
	}
	if got := orgTargets(t, rows[0]); !slices.Equal(got, []string{refs[1], refs[3], refs[5]}) {
		t.Errorf("gmail targets = %q", got)
	}
}

func TestOrganizeBadRefFailsOnlyItself(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	good := m.host.Add(fake.Message{Folder: "INBOX"}).String()
	unknown := "cm1:nope:INBOX:1:1"
	refs := []string{"cm1:bad", good, unknown, "not a ref"}
	resp, err := m.Client.Organize.Archive(t.Context(), connect.NewRequest(&chainmailv1.ArchiveRequest{Refs: refs}))
	if err != nil {
		t.Fatal(err)
	}
	outs := resp.Msg.GetOutcomes()
	if len(outs) != 4 {
		t.Fatalf("outcomes = %v", outs)
	}
	for i, want := range []string{api.ReasonInvalidArgument, "", api.ReasonNotFound, api.ReasonInvalidArgument} {
		if outs[i].GetRef() != refs[i] || outs[i].GetError().GetReason() != want {
			t.Errorf("outcome %d = %v, want reason %q", i, outs[i], want)
		}
	}
	if strings.Contains(outs[2].GetError().GetMessage(), "nope") {
		t.Errorf("unknown account message = %q, repeats the request", outs[2].GetError().GetMessage())
	}
	rows := orgRows(t, m.Harness, "archive")
	if len(rows) != 3 {
		t.Fatalf("audit = %+v", rows)
	}
	byAccount := map[string]daemontest.AuditRow{}
	for _, r := range rows {
		byAccount[r.Account] = r
	}
	if r := byAccount["host"]; r.Result != "ok" || !slices.Equal(orgTargets(t, r), []string{good}) {
		t.Errorf("host audit = %+v", r)
	}
	if r := byAccount["nope"]; r.Result != "error" || r.Reason != api.ReasonNotFound {
		t.Errorf("unknown account audit = %+v", r)
	}
	if r := byAccount[""]; r.Result != "error" || r.Reason != api.ReasonInvalidArgument || !slices.Equal(orgTargets(t, r), []string{"(invalid)", "(invalid)"}) {
		t.Errorf("invalid refs audit = %+v", r)
	}
}

func TestOrganizeRefCount(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{Folder: "INBOX"}).String()
	for _, n := range []int{0, 501} {
		refs := make([]string, n)
		for i := range refs {
			refs[i] = r
		}
		_, err := m.Client.Organize.Trash(t.Context(), connect.NewRequest(&chainmailv1.TrashRequest{Refs: refs}))
		if reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("%d refs: err = %v, want INVALID_ARGUMENT", n, err)
		}
	}
	if n := m.host.Calls("Trash"); n != 0 {
		t.Errorf("Trash called %d times", n)
	}
	rows := orgRows(t, m.Harness, "trash")
	if len(rows) != 2 || rows[1].Result != "error" || !slices.Equal(orgTargets(t, rows[1]), []string{"refs=501"}) {
		t.Errorf("audit = %+v", rows)
	}
	refs := make([]string, 500)
	for i := range refs {
		refs[i] = r
	}
	resp, err := m.Client.Organize.Trash(t.Context(), connect.NewRequest(&chainmailv1.TrashRequest{Refs: refs}))
	if err != nil || len(resp.Msg.GetOutcomes()) != 500 {
		t.Errorf("500 refs: err = %v", err)
	}
}

func TestOrganizeFailingAccountFailsOnlyItsRefs(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	const secret = "SERVER-SAID-q7-do-not-leak"
	m.host.FailWrite("Trash", fmt.Errorf("%w: %s", mailstore.ErrUnavailable, secret))
	refs := []string{
		m.host.Add(fake.Message{Folder: "INBOX"}).String(),
		m.gmail.Add(fake.Message{Folder: gmailAllMail}).String(),
		m.host.Add(fake.Message{Folder: "INBOX"}).String(),
	}
	resp, err := m.Client.Organize.Trash(t.Context(), connect.NewRequest(&chainmailv1.TrashRequest{Refs: refs}))
	if err != nil {
		t.Fatal(err)
	}
	outs := resp.Msg.GetOutcomes()
	if outs[1].GetError() != nil || outs[1].GetNewRef() == "" {
		t.Errorf("gmail outcome = %v", outs[1])
	}
	for _, i := range []int{0, 2} {
		e := outs[i].GetError()
		if e.GetReason() != api.ReasonServerUnavailable || strings.Contains(e.GetMessage(), secret) || strings.Contains(e.GetHint(), secret) {
			t.Errorf("host outcome %d = %v", i, outs[i])
		}
	}
	rows := orgRows(t, m.Harness, "trash")
	if len(rows) != 2 || rows[0].Account != "gmail" || rows[0].Result != "ok" ||
		rows[1].Account != "host" || rows[1].Result != "error" || rows[1].Reason != api.ReasonServerUnavailable {
		t.Errorf("audit = %+v", rows)
	}
}

func TestOrganizeFolders(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	org := m.Client.Organize
	has := func(name string) bool {
		t.Helper()
		folders, err := m.host.ListFolders(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(folders, func(f mailstore.Folder) bool { return f.Name == name })
	}
	if _, err := org.CreateFolder(t.Context(), connect.NewRequest(&chainmailv1.CreateFolderRequest{Account: "host", Name: "Projects/2026"})); err != nil {
		t.Fatal(err)
	}
	if !has("Projects/2026") {
		t.Error("created folder is missing")
	}
	if _, err := org.RenameFolder(t.Context(), connect.NewRequest(&chainmailv1.RenameFolderRequest{Account: "host", From: "Projects/2026", To: "Projects/Done"})); err != nil {
		t.Fatal(err)
	}
	if has("Projects/2026") || !has("Projects/Done") {
		t.Error("rename did not move the folder")
	}
	if _, err := org.DeleteFolder(t.Context(), connect.NewRequest(&chainmailv1.DeleteFolderRequest{Account: "host", Name: "Projects/Done", Force: true})); err != nil {
		t.Fatal(err)
	}
	if has("Projects/Done") {
		t.Error("deleted folder is still there")
	}
	want := map[string][]string{
		"folders.create": {"Projects/2026"},
		"folders.rename": {"Projects/2026", "Projects/Done"},
		"folders.delete": {"Projects/Done", "force"},
	}
	for action, targets := range want {
		rows := orgRows(t, m.Harness, action)
		if len(rows) != 1 || rows[0].Account != "host" || rows[0].Result != "ok" || !slices.Equal(orgTargets(t, rows[0]), targets) {
			t.Errorf("%s audit = %+v, want targets %q", action, rows, targets)
		}
	}
}

func TestOrganizeFolderErrors(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	org := m.Client.Organize
	const secret = "SERVER-SAID-f3-do-not-leak"
	tests := []struct {
		name string
		call func() error
		want string
	}{
		{name: "empty name", want: api.ReasonInvalidArgument, call: func() error {
			_, err := org.CreateFolder(t.Context(), connect.NewRequest(&chainmailv1.CreateFolderRequest{Account: "host"}))
			return err
		}},
		{name: "control byte", want: api.ReasonInvalidArgument, call: func() error {
			_, err := org.CreateFolder(t.Context(), connect.NewRequest(&chainmailv1.CreateFolderRequest{Account: "host", Name: "a\x7fb"}))
			return err
		}},
		{name: "too long", want: api.ReasonInvalidArgument, call: func() error {
			_, err := org.CreateFolder(t.Context(), connect.NewRequest(&chainmailv1.CreateFolderRequest{Account: "host", Name: strings.Repeat("n", 256)}))
			return err
		}},
		{name: "bad rename target", want: api.ReasonInvalidArgument, call: func() error {
			_, err := org.RenameFolder(t.Context(), connect.NewRequest(&chainmailv1.RenameFolderRequest{Account: "host", From: "Archive", To: "a\tb"}))
			return err
		}},
		{name: "bad account name", want: api.ReasonInvalidArgument, call: func() error {
			_, err := org.CreateFolder(t.Context(), connect.NewRequest(&chainmailv1.CreateFolderRequest{Account: "Bad_Name", Name: "x"}))
			return err
		}},
		{name: "unknown account", want: api.ReasonNotFound, call: func() error {
			_, err := org.DeleteFolder(t.Context(), connect.NewRequest(&chainmailv1.DeleteFolderRequest{Account: "nope", Name: "x"}))
			return err
		}},
		{name: "exists", want: api.ReasonInvalidArgument, call: func() error {
			_, err := org.CreateFolder(t.Context(), connect.NewRequest(&chainmailv1.CreateFolderRequest{Account: "host", Name: "Archive"}))
			return err
		}},
		{name: "inbox refused", want: api.ReasonInvalidArgument, call: func() error {
			_, err := org.DeleteFolder(t.Context(), connect.NewRequest(&chainmailv1.DeleteFolderRequest{Account: "host", Name: "INBOX"}))
			return err
		}},
		{name: "server down", want: api.ReasonServerUnavailable, call: func() error {
			m.host.FailWrite("RenameFolder", fmt.Errorf("%w: %s", mailstore.ErrUnavailable, secret))
			_, err := org.RenameFolder(t.Context(), connect.NewRequest(&chainmailv1.RenameFolderRequest{Account: "host", From: "Archive", To: "Old"}))
			return err
		}},
	}
	for _, tt := range tests {
		err := tt.call()
		if reasonOf(err) != tt.want {
			t.Errorf("%s: err = %v, want %s", tt.name, err, tt.want)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("%s: err = %v, leaks the server's text", tt.name, err)
		}
	}
	for _, row := range m.AuditRows(t) {
		if row.Result != "error" {
			t.Errorf("audit = %+v, want error", row)
		}
		if strings.Contains(row.Targets, "\x7f") || strings.Contains(row.Targets, "nnnnnnnn") || strings.Contains(row.Targets+row.Account, "Bad_Name") {
			t.Errorf("audit targets %q record an invalid name", row.Targets)
		}
	}
}

// accountFanOut is the most accounts one organize call works on at once.
const accountFanOut = 8

// countingStore wraps a fake store and records how many Trash calls run on
// it, and across all stores, at once.
type countingStore struct {
	*fake.Store
	fan *fanout
}

// fanout counts concurrent calls. Each call waits until accountFanOut calls
// are in flight, so a bounded fan-out reaches its bound, then waits a little
// longer for every account, so an unbounded one shows up past the bound.
type fanout struct {
	mu       sync.Mutex
	inFlight map[string]int
	maxPer   map[string]int
	maxAll   int
	all      int
	accounts int
	full     chan struct{}
	allIn    chan struct{}
	fullOnce sync.Once
	allOnce  sync.Once
}

func newFanout(accounts int) *fanout {
	return &fanout{
		inFlight: map[string]int{},
		maxPer:   map[string]int{},
		accounts: accounts,
		full:     make(chan struct{}),
		allIn:    make(chan struct{}),
	}
}

func (c countingStore) Trash(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	acct := refs[0].Account
	f := c.fan
	f.mu.Lock()
	f.inFlight[acct]++
	f.all++
	f.maxPer[acct] = max(f.maxPer[acct], f.inFlight[acct])
	f.maxAll = max(f.maxAll, f.all)
	if f.all >= accountFanOut {
		f.fullOnce.Do(func() { close(f.full) })
	}
	if f.all >= f.accounts {
		f.allOnce.Do(func() { close(f.allIn) })
	}
	f.mu.Unlock()
	select {
	case <-f.full:
	case <-time.After(5 * time.Second):
	}
	select {
	case <-f.allIn:
	case <-time.After(100 * time.Millisecond):
	}
	defer func() {
		f.mu.Lock()
		f.inFlight[acct]--
		f.all--
		f.mu.Unlock()
	}()
	return c.Store.Trash(ctx, refs)
}

func TestOrganizeFanOutIsBounded(t *testing.T) {
	t.Parallel()
	const accounts = accountFanOut + 4
	var cfg strings.Builder
	stores := map[string]*fake.Store{}
	for i := range accounts {
		name := fmt.Sprintf("acct%d", i)
		fmt.Fprintf(&cfg, "[accounts.%s]\nprovider = \"hostinger\"\naddress = \"%s@example.com\"\n\n", name, name)
		stores[name] = fake.New(name, false)
	}
	fan := newFanout(accounts)
	h := daemontest.Start(t, cfg.String(), daemontest.Options{
		NewStore: func(a config.ResolvedAccount) mailstore.Store {
			return countingStore{Store: stores[a.Name], fan: fan}
		},
		Now: func() time.Time { return testNow },
	})
	var refs []string
	for range 3 {
		for i := range accounts {
			refs = append(refs, stores[fmt.Sprintf("acct%d", i)].Add(fake.Message{Folder: "INBOX"}).String())
		}
	}
	resp, err := h.Client.Organize.Trash(t.Context(), connect.NewRequest(&chainmailv1.TrashRequest{Refs: refs}))
	if err != nil {
		t.Fatal(err)
	}
	checkOK(t, refs, resp.Msg.GetOutcomes())
	for name, s := range stores {
		if n := s.Calls("Trash"); n != 1 {
			t.Errorf("%s: Trash called %d times, want one per account", name, n)
		}
	}
	fan.mu.Lock()
	defer fan.mu.Unlock()
	for name, n := range fan.maxPer {
		if n != 1 {
			t.Errorf("%s: most concurrent calls = %d, want 1", name, n)
		}
	}
	if fan.maxAll != accountFanOut {
		t.Errorf("most concurrent calls = %d, want the bound %d", fan.maxAll, accountFanOut)
	}
	if rows := orgRows(t, h, "trash"); len(rows) != accounts {
		t.Errorf("audit rows = %d, want %d", len(rows), accounts)
	}
}
