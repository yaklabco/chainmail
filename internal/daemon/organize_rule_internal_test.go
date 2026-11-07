package daemon

import (
	"errors"
	"os"
	"slices"
	"testing"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
)

// testRuleTargets are the extra audit targets a rule's action carries.
var testRuleTargets = []string{"rule=ru_x", "run=7"}

// ruleAuditRows returns the audit rows of action, oldest first.
func ruleAuditRows(t *testing.T, s *Server, action string) []audit.Row {
	t.Helper()
	rows, _, err := audit.NewReader(s.opts.StateDB).Read(t.Context(), audit.Query{Action: action, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(rows)
	return rows
}

// TestBatchCallConstructors checks that each constructor builds the call
// its handler built before: the audit action, the refs as given, the
// targets, the check's verdict with its fixed text, and a run that calls
// the right Writer method with the checked arguments.
func TestBatchCallConstructors(t *testing.T) {
	t.Parallel()
	refs := []string{"ref-a", "ref-b"}
	tests := []struct {
		name     string
		call     batchCall
		action   string
		targets  []string
		checkErr string // "" means the check passes
		gmail    bool   // run against a Google fake
		op       string // the Writer method run calls
		after    func(t *testing.T, fk *fake.Store, r ref.Ref)
	}{
		{name: "move", call: moveCall(refs, "Archive"), action: "move", targets: []string{"folder=Archive"}, op: "Move"},
		{name: "move to no folder", call: moveCall(refs, ""), action: "move", targets: []string{invalidTarget}, checkErr: "the folder name is not valid"},
		{name: "copy", call: copyCall(refs, "Archive"), action: "copy", targets: []string{"folder=Archive"}, op: "Copy"},
		{name: "copy to a control byte", call: copyCall(refs, "a\x00b"), action: "copy", targets: []string{invalidTarget}, checkErr: "the folder name is not valid"},
		{name: "archive", call: archiveCall(refs), action: "archive", op: "Archive"},
		{name: "trash", call: trashCall(refs), action: "trash", op: "Trash"},
		{
			name: "flags", call: flagsCall(refs, []string{`\flagged`}, []string{`\SEEN`}), action: "flags", op: "StoreFlags",
			after: func(t *testing.T, fk *fake.Store, r ref.Ref) {
				t.Helper()
				if got := fk.Flags(r); !slices.Equal(got, []string{`\Flagged`}) {
					t.Errorf("flags = %q, want the checked [\\Flagged]", got)
				}
			},
		},
		{name: "no flags", call: flagsCall(refs, nil, nil), action: "flags", checkErr: "no flags to add or remove"},
		{name: "a bad keyword", call: flagsCall(refs, []string{"two words"}, nil), action: "flags", checkErr: "a keyword is not valid"},
		{name: `\Deleted`, call: flagsCall(refs, nil, []string{`\Deleted`}), action: "flags", checkErr: `\Deleted cannot be set or cleared`},
		{
			name: "labels", call: labelsCall(refs, []string{"Receipts"}, []string{`\Inbox`}), action: "labels", gmail: true, op: "StoreLabels",
			after: func(t *testing.T, fk *fake.Store, r ref.Ref) {
				t.Helper()
				res := fk.FetchMessages(t.Context(), []ref.Ref{r})
				if res[0].Err != nil {
					t.Fatal(res[0].Err)
				}
				if got := res[0].Message.Summary.Labels; !slices.Contains(got, "Receipts") || slices.Contains(got, `\Inbox`) {
					t.Errorf("labels = %q, want Receipts without \\Inbox", got)
				}
			},
		},
		{name: "no labels", call: labelsCall(refs, nil, nil), action: "labels", checkErr: "no labels to add or remove"},
		{name: "an empty label", call: labelsCall(refs, []string{"ok", ""}, nil), action: "labels", checkErr: "the label name is not valid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := tt.call
			if c.action != tt.action || !slices.Equal(c.refs, refs) || !slices.Equal(c.targets, tt.targets) {
				t.Fatalf("call = action %q refs %q targets %q, want %q %q %q", c.action, c.refs, c.targets, tt.action, refs, tt.targets)
			}
			var err error
			if c.check != nil {
				err = c.check()
			}
			if tt.checkErr != "" {
				ae, ok := errors.AsType[*argError](err)
				if !ok || ae.msg != tt.checkErr {
					t.Fatalf("check = %v, want the argError %q", err, tt.checkErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("check = %v, want nil", err)
			}
			fk := fake.New("host", tt.gmail)
			folder := "INBOX"
			if tt.gmail {
				folder = "[Gmail]/All Mail"
			}
			r := fk.Add(fake.Message{Folder: folder, Subject: "s", Flags: []string{`\Seen`}, Labels: []string{`\Inbox`}})
			res := c.run(t.Context(), fk, []ref.Ref{r})
			if len(res) != 1 || res[0].Err != nil {
				t.Fatalf("run = %+v, want one success", res)
			}
			if ops := fk.Ops(); !slices.Equal(ops, []string{tt.op}) {
				t.Errorf("Writer calls = %q, want [%s]", ops, tt.op)
			}
			if tt.after != nil {
				tt.after(t, fk, r)
			}
		})
	}
}

// TestBatchAsRunsAsTheRuleCaller runs one ref through batchAs as the rule
// caller, with no socket peer in the context, for each action a rule
// takes. The change lands on the fake store, and the account's one audit
// row carries client rule, the daemon's uid, the ref, the call's targets
// and then the rule's. A refused call is recorded the same way and
// touches no store.
func TestBatchAsRunsAsTheRuleCaller(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		account string
		call    func(r string) batchCall
		targets []string // after the ref, before the rule's
		reason  string   // "" means the call succeeds
		check   func(t *testing.T, fk *fake.Store, r ref.Ref, out *chainmailv1.RefOutcome)
	}{
		{
			name: "move", account: "host", call: func(r string) batchCall { return moveCall([]string{r}, "Archive") },
			targets: []string{"folder=Archive"}, check: ruleNewRefIn("Archive"),
		},
		{
			name: "copy", account: "host", call: func(r string) batchCall { return copyCall([]string{r}, "Archive") },
			targets: []string{"folder=Archive"}, check: ruleNewRefIn("Archive"),
		},
		{
			name: "flag", account: "host", call: func(r string) batchCall { return flagsCall([]string{r}, []string{`\Flagged`}, nil) },
			check: func(t *testing.T, fk *fake.Store, r ref.Ref, _ *chainmailv1.RefOutcome) {
				t.Helper()
				if !slices.Contains(fk.Flags(r), `\Flagged`) {
					t.Errorf("flags = %q, want \\Flagged", fk.Flags(r))
				}
			},
		},
		{
			name: "label", account: "gmail", call: func(r string) batchCall { return labelsCall([]string{r}, []string{"Receipts"}, nil) },
			check: func(t *testing.T, fk *fake.Store, r ref.Ref, _ *chainmailv1.RefOutcome) {
				t.Helper()
				res := fk.FetchMessages(t.Context(), []ref.Ref{r})
				if res[0].Err != nil || !slices.Contains(res[0].Message.Summary.Labels, "Receipts") {
					t.Errorf("fetch = %+v, want the Receipts label", res[0])
				}
			},
		},
		{name: "archive", account: "host", call: func(r string) batchCall { return archiveCall([]string{r}) }, check: ruleNewRefIn("Archive")},
		{name: "trash", account: "host", call: func(r string) batchCall { return trashCall([]string{r}) }, check: ruleNewRefIn("Trash")},
		{
			name: "a refused move", account: "host", call: func(r string) batchCall { return moveCall([]string{r}, "") },
			targets: []string{invalidTarget}, reason: api.ReasonInvalidArgument,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, fakes, _ := linkServer(t)
			fk := fakes[tt.account]
			folder := "INBOX"
			if tt.account == "gmail" {
				folder = "[Gmail]/All Mail"
			}
			r := fk.Add(fake.Message{Folder: folder, Date: routeDaysAgo(1), Subject: "receipt", Labels: []string{`\Inbox`}})
			c := tt.call(r.String())
			c.targets = append(c.targets, testRuleTargets...)
			outs, err := (&organizeService{s: s}).batchAs(t.Context(), ruleCaller(), c)
			if tt.reason != "" {
				if got := api.Detail(err).GetReason(); got != tt.reason {
					t.Fatalf("batchAs err = %v, want %s", err, tt.reason)
				}
				if ops := fk.Ops(); len(ops) != 0 {
					t.Errorf("Writer calls = %q, want none", ops)
				}
			} else {
				if err != nil {
					t.Fatalf("batchAs: %v", err)
				}
				if len(outs) != 1 || outs[0].GetError() != nil {
					t.Fatalf("outcomes = %v, want one success", outs)
				}
				tt.check(t, fk, r, outs[0])
			}
			rows := ruleAuditRows(t, s, c.action)
			if len(rows) != 1 {
				t.Fatalf("audit rows = %+v, want one", rows)
			}
			row := rows[0]
			wantResult := audit.ResultOK
			if tt.reason != "" {
				wantResult = audit.ResultError
			}
			if row.Client != ruleClient || row.CallerUID != uint32(os.Getuid()) || row.Account != tt.account ||
				row.Result != wantResult || row.Reason != tt.reason {
				t.Errorf("audit row = %+v, want client rule, uid %d, account %s, result %s %s", row, os.Getuid(), tt.account, wantResult, tt.reason)
			}
			want := slices.Concat([]string{r.String()}, tt.targets, testRuleTargets)
			if !slices.Equal(row.Targets, want) {
				t.Errorf("targets = %q, want %q", row.Targets, want)
			}
		})
	}
}

// ruleNewRefIn checks that an outcome's new ref is in folder.
func ruleNewRefIn(folder string) func(*testing.T, *fake.Store, ref.Ref, *chainmailv1.RefOutcome) {
	return func(t *testing.T, _ *fake.Store, _ ref.Ref, out *chainmailv1.RefOutcome) {
		t.Helper()
		nr, err := ref.Parse(out.GetNewRef())
		if err != nil || nr.Folder != folder {
			t.Errorf("new ref = %q (%v), want one in %s", out.GetNewRef(), err, folder)
		}
	}
}
