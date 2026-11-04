package watch

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
)

// supAutoSweep is the expunge_sweep the SyncAuto tests set: twice the 5m
// sync.poll, so the poller always waits exactly one sync.poll and resolves
// its set again on every second tick.
const supAutoSweep = 10 * time.Minute

// supDefaultScope is what a default search covers on the fake standard
// account: every folder but Trash and Junk, INBOX first, then by name.
var supDefaultScope = []string{"INBOX", "Archive", "Drafts", "Sent"}

// supAutoAccount is the hostinger account acme with sync.folders unset, as
// config.Parse resolves it: SyncFolders is the watch list and SyncAuto is set.
func supAutoAccount(t *testing.T, watch []string) config.ResolvedAccount {
	t.Helper()
	acct := supAccount(t, "hostinger", watch, slices.Clone(watch))
	acct.SyncAuto = true
	return acct
}

// supAutoRig is a rig for acct with expunge_sweep at supAutoSweep and, when
// logs is not nil, a text logger writing to it.
func supAutoRig(t *testing.T, fs *fake.Store, acct config.ResolvedAccount, logs *syncBuffer, tweak ...func(*SupervisorOptions)) *supRig {
	t.Helper()
	base := func(o *SupervisorOptions) {
		o.Settings.ExpungeSweep = supAutoSweep
		if logs != nil {
			o.Logger = slog.New(slog.NewTextHandler(logs, nil))
		}
	}
	return newSupRig(t, fs, acct, append([]func(*SupervisorOptions){base}, tweak...)...)
}

// syncRows returns acme's sync_folders rows by folder.
func (r *supRig) syncRows() map[string]FolderRow {
	r.t.Helper()
	rows, err := r.state.Folders(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	out := make(map[string]FolderRow, len(rows))
	for _, row := range rows {
		if row.Account == "acme" {
			out[row.Folder] = row
		}
	}
	return out
}

// pollTick advances the clock one sync.poll and waits until the poller has
// run the cycle that started and waits on the next one. It suits the
// SyncAuto rigs, whose poller always waits exactly one sync.poll.
func (r *supRig) pollTick(poll time.Duration) {
	r.t.Helper()
	r.clock.Advance(poll)
	r.clock.expectAfter(r.t, poll)
}

// With sync.folders unset, a non-Google account polls every folder a
// default search covers except its watched ones, on pooled connections,
// after a silent baseline. Trash and Junk are not polled, and INBOX stays
// watched.
func TestSupervisorSyncAutoPollsTheDefaultScope(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	fs.Add(supMsg("Sent", "old report"))
	acct := supAutoAccount(t, []string{"INBOX"})
	r := supAutoRig(t, fs, acct, nil)
	r.start()
	for _, folder := range []string{"Archive", "Drafts", "Sent"} {
		r.waitSeen(folder+" polling", supStatus(folder, StatePolling))
	}
	r.waitSeen("INBOX watching", supStatus("INBOX", StateWatching))
	r.clock.expectAfter(t, acct.SyncPoll)
	rows := r.syncRows()
	for folder, mode := range map[string]Mode{"INBOX": ModeIdle, "Archive": ModePoll, "Drafts": ModePoll, "Sent": ModePoll} {
		if row, ok := rows[folder]; !ok || row.Mode != mode {
			t.Errorf("%s row = %+v (present %v), want mode %q", folder, row, ok, mode)
		}
	}
	for _, folder := range []string{"Trash", "Junk"} {
		if _, ok := rows[folder]; ok {
			t.Errorf("%s has a sync row, but a default search does not cover it", folder)
		}
	}
	if n := supCount(r.all(), supStatus("INBOX", StatePolling)); n != 0 {
		t.Errorf("%d polling events for the watched INBOX, want 0", n)
	}
	if n := supCount(r.all(), supKind("Sent", KindNew)); n != 0 {
		t.Errorf("the baseline of Sent produced %d new events, want 0", n)
	}
	if n := fs.WatchConns(); n != 1 {
		t.Errorf("WatchConns() = %d, want 1: polled folders use the pool", n)
	}
	if n := fs.Calls("SearchFolders"); n != 1 {
		t.Errorf("SearchFolders ran %d times at session start, want 1", n)
	}
	if n := fs.Calls("ListFolders"); n != 1 {
		t.Errorf("ListFolders ran %d times at session start, want 1", n)
	}
	if n := r.held.Load(); n != 0 {
		t.Errorf("store held %d times between ticks, want 0", n)
	}
}

// A folder that appears is polled once the set is resolved again, every
// expunge_sweep and not sooner, and its first pass baselines it silently.
func TestSupervisorSyncAutoPicksUpANewFolder(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	acct := supAutoAccount(t, []string{})
	r := supAutoRig(t, fs, acct, nil)
	r.start()
	for _, folder := range supDefaultScope {
		r.waitSeen(folder+" polling", supStatus(folder, StatePolling))
	}
	r.clock.expectAfter(t, acct.SyncPoll)

	fs.AddFolder("Projects", mailstore.RoleNone)
	fs.Add(supMsg("Projects", "plan"))
	r.pollTick(acct.SyncPoll) // 5m: a poll, and no resolution yet
	if n := fs.Calls("SearchFolders"); n != 1 {
		t.Errorf("SearchFolders ran %d times before expunge_sweep passed, want 1", n)
	}
	if n := supCount(r.all(), supKind("Projects", KindStatus)); n != 0 {
		t.Errorf("Projects has status events before the set was resolved again:%s", supDump(r.all()))
	}

	r.clock.Advance(acct.SyncPoll) // 10m: the set is resolved again
	r.waitSeen("Projects polling", supStatus("Projects", StatePolling))
	r.clock.expectAfter(t, acct.SyncPoll)
	if n := fs.Calls("SearchFolders"); n != 2 {
		t.Errorf("SearchFolders ran %d times after one expunge_sweep, want 2", n)
	}
	if n := supCount(r.all(), supKind("Projects", KindNew)); n != 0 {
		t.Errorf("the baseline of Projects produced %d new events, want 0", n)
	}
	if row := r.row("Projects"); row.Mode != ModePoll {
		t.Errorf("Projects mode = %q, want poll", row.Mode)
	}

	fs.Add(supMsg("Projects", "kickoff"))
	r.clock.Advance(acct.SyncPoll)
	r.waitSeen("new event for kickoff", supNew("Projects", "kickoff"))
}

// Parking writes auth_required to the folders the poller resolved, which
// the configuration never named.
func TestSupervisorSyncAutoParksResolvedFolders(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	var required atomic.Bool
	acct := supAutoAccount(t, []string{})
	r := supAutoRig(t, fs, acct, nil, func(o *SupervisorOptions) {
		o.AuthRequired = func(string) (bool, string) { return required.Load(), supHint }
	})
	r.start()
	for _, folder := range supDefaultScope {
		r.waitSeen(folder+" polling", supStatus(folder, StatePolling))
	}
	r.clock.expectAfter(t, authRecheck)
	r.clock.expectAfter(t, acct.SyncPoll)

	required.Store(true)
	r.clock.Advance(authRecheck)
	r.waitSeen("account auth_required", supStatus("", StateAuthRequired))
	for _, folder := range supDefaultScope {
		if row := r.row(folder); row.State != StateAuthRequired || row.LastError != "AUTH_REQUIRED" {
			t.Errorf("%s row = state %q last_error %q, want auth_required and AUTH_REQUIRED", folder, row.State, row.LastError)
		}
	}
}

// A SyncAuto account whose first resolution fails starts from the
// configured set less its watched folders, which is empty for watch = []:
// the poller then waits for the next resolution, and polls what it finds.
func TestSupervisorSyncAutoWithNothingToPollWaitsForTheNextResolution(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	fs.Fail("SearchFolders", fmt.Errorf("%w: test outage", mailstore.ErrUnavailable))
	acct := supAutoAccount(t, []string{})
	r := supAutoRig(t, fs, acct, nil)
	r.start()
	r.clock.expectAfter(t, supAutoSweep)
	if rows := r.syncRows(); len(rows) != 0 {
		t.Errorf("rows = %v, want none before a resolution succeeds", rows)
	}

	fs.Fail("SearchFolders", nil)
	r.clock.Advance(supAutoSweep)
	for _, folder := range supDefaultScope {
		r.waitSeen(folder+" polling", supStatus(folder, StatePolling))
	}
}

// Config never sets SyncAuto on a Google preset, and the supervisor ignores
// it there too: the account watches All Mail and resolves nothing.
func TestSupervisorSyncAutoIgnoredOnGmail(t *testing.T) {
	t.Parallel()
	fs := supFake(true)
	acct := supAccount(t, "gmail", nil, nil)
	acct.SyncAuto = true
	r := newSupRig(t, fs, acct)
	r.start()
	r.waitFor("All Mail watching", supStatus(supAllMail, StateWatching))
	r.waitIdle(supAllMail)
	if n := fs.Calls("SearchFolders"); n != 0 {
		t.Errorf("SearchFolders ran %d times on a Gmail account, want 0", n)
	}
	rows := r.syncRows()
	if _, ok := rows[supAllMail]; !ok || len(rows) != 1 {
		t.Errorf("rows = %v, want only %s", rows, supAllMail)
	}
}

// Without SyncAuto the poller polls the configured folders only and never
// resolves a set.
func TestSupervisorWithoutSyncAutoPollsOnlyItsConfiguredFolders(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	acct := supAccount(t, "hostinger", nil, []string{"Sent"})
	r := newSupRig(t, fs, acct)
	r.start()
	r.waitFor("Sent polling", supStatus("Sent", StatePolling))
	r.clock.expectAfter(t, acct.SyncPoll)
	for _, method := range []string{"SearchFolders", "ListFolders"} {
		if n := fs.Calls(method); n != 0 {
			t.Errorf("%s ran %d times without SyncAuto, want 0", method, n)
		}
	}
	rows := r.syncRows()
	if _, ok := rows["Sent"]; !ok || len(rows) != 1 {
		t.Errorf("rows = %v, want only Sent", rows)
	}
}

// A folder that disappears is dropped when the set is resolved again: its
// sync_folders row and UIDs are deleted before it would be polled, so it
// never reports FOLDER_NOT_FOUND, and no later poll writes it back.
func TestSupervisorSyncAutoDropsARemovedFolder(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	fs.AddFolder("Projects", mailstore.RoleNone)
	fs.Add(supMsg("Projects", "plan"))
	acct := supAutoAccount(t, []string{})
	r := supAutoRig(t, fs, acct, nil)
	r.start()
	r.waitSeen("Projects polling", supStatus("Projects", StatePolling))
	r.clock.expectAfter(t, acct.SyncPoll)
	if uids, err := r.state.uids(t.Context(), "acme", "Projects"); err != nil || len(uids) != 1 {
		t.Fatalf("Projects UIDs = %v, %v; want the baselined one", uids, err)
	}
	r.pollTick(acct.SyncPoll) // 5m: an ordinary poll

	if err := fs.DeleteFolder(t.Context(), "Projects", true); err != nil {
		t.Fatal(err)
	}
	r.pollTick(acct.SyncPoll) // 10m: the set is resolved again and Projects dropped
	if _, ok, err := r.state.Folder(t.Context(), "acme", "Projects"); err != nil || ok {
		t.Errorf("Projects row after it disappeared: present %v, err %v; want gone", ok, err)
	}
	if uids, err := r.state.uids(t.Context(), "acme", "Projects"); err != nil || len(uids) != 0 {
		t.Errorf("Projects UIDs after it disappeared = %v, %v; want none", uids, err)
	}
	r.pollTick(acct.SyncPoll) // 15m: another poll writes nothing back
	if _, ok, err := r.state.Folder(t.Context(), "acme", "Projects"); err != nil || ok {
		t.Errorf("Projects row a poll later: present %v, err %v; want gone", ok, err)
	}
	if n := supCount(r.all(), supStatus("Projects", StateError)); n != 0 {
		t.Errorf("%d error events for the dropped Projects, want 0", n)
	}
	for _, folder := range supDefaultScope {
		if row := r.row(folder); row.Mode != ModePoll {
			t.Errorf("%s mode = %q, want poll", folder, row.Mode)
		}
	}
}

// dropUnsynced deletes the account's rows for folders neither watched nor
// in the resolved set, whatever left them: a folder gone while the daemon
// was down, or one an earlier sync.folders named. Watched folders, folders
// in the set and other accounts' rows stay, and a dropped folder is
// forgotten, so a parking does not write its row back.
func TestSupervisorDropUnsynced(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	r := supAutoRig(t, fs, supAutoAccount(t, []string{"INBOX"}), nil)
	ctx := t.Context()
	for _, row := range []struct {
		account, folder string
		mode            Mode
	}{
		{"acme", "INBOX", ModeIdle},
		{"acme", "Sent", ModePoll},
		{"acme", "Old", ModePoll},
		{"acme", "Trash", ModePoll},
		{"other", "Old", ModePoll},
	} {
		if err := r.state.SetStatus(ctx, row.account, row.folder, row.mode, StatePolling, ""); err != nil {
			t.Fatal(err)
		}
	}
	r.sup.know("Old", ModePoll)
	r.sup.dropUnsynced(ctx, []string{"Sent"})
	rows, err := r.state.Folders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.Account+"/"+row.Folder)
	}
	if want := []string{"acme/INBOX", "acme/Sent", "other/Old"}; !slices.Equal(got, want) {
		t.Errorf("rows = %q, want %q", got, want)
	}
	for _, st := range r.sup.knownFolders() {
		if st.name == "Old" {
			t.Error("Old is still known after it was dropped, so a parking would write its row back")
		}
	}
}

// supNoScoper is the fake without its SearchFolders, as a store that does
// not implement mailstore.Scoper. Embedding the two interfaces promotes
// only their methods.
type supNoScoper struct {
	mailstore.Store
	mailstore.Watcher
}

// When the set cannot be resolved the poller keeps polling the last one,
// drops nothing, and logs a fixed reason without the store's text. The
// next resolution that succeeds picks the changes up.
func TestSupervisorSyncAutoKeepsTheLastSetWhenResolvingFails(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	var logs syncBuffer
	acct := supAutoAccount(t, []string{})
	r := supAutoRig(t, fs, acct, &logs)
	r.start()
	for _, folder := range supDefaultScope {
		r.waitSeen(folder+" polling", supStatus(folder, StatePolling))
	}
	r.clock.expectAfter(t, acct.SyncPoll)

	fs.Fail("SearchFolders", fmt.Errorf("%w: server says hello", mailstore.ErrUnavailable))
	fs.AddFolder("Projects", mailstore.RoleNone)
	r.pollTick(acct.SyncPoll) // 5m
	r.pollTick(acct.SyncPoll) // 10m: resolving fails
	if n := fs.Calls("SearchFolders"); n != 2 {
		t.Errorf("SearchFolders ran %d times, want 2", n)
	}
	if n := supCount(r.all(), supKind("Projects", KindStatus)); n != 0 {
		t.Errorf("Projects was polled although the set could not be resolved:%s", supDump(r.all()))
	}
	rows := r.syncRows()
	for _, folder := range supDefaultScope {
		if _, ok := rows[folder]; !ok {
			t.Errorf("%s row dropped after a failed resolution", folder)
		}
	}
	got := logs.String()
	want := `level=WARN msg="resolving the folders to poll failed; polling the last set" account=acme reason=unavailable`
	if !strings.Contains(got, want) {
		t.Errorf("logs =\n%s\nwant a line with\n%s", got, want)
	}
	if strings.Contains(got, "hello") {
		t.Errorf("logs carry the store's text:\n%s", got)
	}

	fs.Add(supMsg("Sent", "during"))
	fs.Fail("SearchFolders", nil)
	r.clock.Advance(acct.SyncPoll) // 15m: the last set is still polled
	r.waitSeen("new event for during", supNew("Sent", "during"))
	r.clock.expectAfter(t, acct.SyncPoll)
	if n := supCount(r.all(), supKind("Projects", KindStatus)); n != 0 {
		t.Errorf("Projects was polled before the next resolution:%s", supDump(r.all()))
	}
	r.clock.Advance(acct.SyncPoll) // 20m: resolved again
	r.waitSeen("Projects polling", supStatus("Projects", StatePolling))
}

// A store without mailstore.Scoper cannot resolve the set: the poller
// polls the configured set less the watched folders, never lists folders
// for it, and logs a fixed reason.
func TestSupervisorSyncAutoWithoutScoper(t *testing.T) {
	t.Parallel()
	fs := supFake(false)
	var logs syncBuffer
	acct := supAutoAccount(t, []string{})
	acct.SyncFolders = []string{"Sent"} // hand-built, so the fallback has a folder to poll
	r := supAutoRig(t, fs, acct, &logs)
	r.store = supNoScoper{Store: fs, Watcher: fs}
	r.start()
	r.waitFor("Sent polling", supStatus("Sent", StatePolling))
	r.clock.expectAfter(t, acct.SyncPoll)
	rows := r.syncRows()
	if _, ok := rows["Sent"]; !ok || len(rows) != 1 {
		t.Errorf("rows = %v, want only Sent", rows)
	}
	if n := fs.Calls("ListFolders"); n != 0 {
		t.Errorf("ListFolders ran %d times for a store that cannot resolve the set, want 0", n)
	}
	want := `level=WARN msg="resolving the folders to poll failed; polling the last set" account=acme reason=no_scoper`
	if got := logs.String(); !strings.Contains(got, want) {
		t.Errorf("logs =\n%s\nwant a line with\n%s", got, want)
	}
}
