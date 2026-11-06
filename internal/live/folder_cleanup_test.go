package live

// How the organize test removes its folder. It needs a daemon but no real
// account, so this file has no build tag, and its tests run with the unit
// suite against a daemon over fake stores.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/client"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// folderSettleTimeout bounds cleanup's retries of deleting the test
// folder, in case the server still counts the expunged mail in it.
const folderSettleTimeout = 30 * time.Second

func hint(err error) string { return api.Detail(err).GetHint() }

func reasonOf(err error) string { return api.Detail(err).GetReason() }

// pause waits before repeat n of a search, or until ctx is done.
func pause(ctx context.Context, n int) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(backoff(n)):
		return nil
	}
}

// deleteTestFolder deletes the test's folder without force, so it never
// takes mail the sweep did not find with it. A folder that is not there is
// fine: the create may not have taken effect, or the test deleted it
// already. While the server refuses, it runs resweep, which expunges only
// the test's own token mail, and tries again, for folderSettleTimeout.
func deleteTestFolder(ctx context.Context, c *client.Client, account, folder string, resweep func(context.Context) error) error {
	start := time.Now()
	var sweepErr error
	for n := 0; ; n++ {
		_, err := c.Organize.DeleteFolder(ctx, connect.NewRequest(&chainmailv1.DeleteFolderRequest{Account: account, Name: folder}))
		if err == nil || reasonOf(err) == api.ReasonNotFound {
			return nil
		}
		if time.Since(start) >= folderSettleTimeout || pause(ctx, n) != nil {
			return errors.Join(fmt.Errorf("DeleteFolder %s: %w (%s)", folder, err, hint(err)), sweepErr)
		}
		if resweep != nil {
			sweepErr = resweep(ctx)
		}
	}
}

// testFolderNames returns the names among folders that the organize test's
// generated folder name may have on the server: the name itself, or the
// name under a personal namespace prefix, one level and its delimiter, such
// as Hostinger's "INBOX.". The name is random and was checked absent before
// the create, so only the test's own create can have made one.
func testFolderNames(folders []string, generated string) []string {
	var out []string
	for _, f := range folders {
		prefix, ok := strings.CutSuffix(f, generated)
		if ok && (prefix == "" || namespacePrefix(prefix)) {
			out = append(out, f)
		}
	}
	return out
}

// namespacePrefix says p is one level and a delimiter, "." or "/", as a
// personal namespace prefix such as "INBOX." is.
func namespacePrefix(p string) bool {
	level, ok := strings.CutSuffix(p, ".")
	if !ok {
		level, ok = strings.CutSuffix(p, "/")
	}
	return ok && level != "" && !strings.ContainsAny(level, "./")
}

// removeTestFolder deletes the organize test's folder, without force.
// known is the name the server gave it. When it is "", the create's outcome
// is unknown: it may have taken effect under a namespace prefix, where
// DeleteFolder, which takes the server's name exactly, would answer
// NOT_FOUND for the generated name. So the account's folders are listed
// afresh, and each that testFolderNames finds is deleted. None found means
// the create never took effect.
func removeTestFolder(ctx context.Context, c *client.Client, account, generated, known string, resweep func(context.Context) error) error {
	if known != "" {
		return deleteTestFolder(ctx, c, account, known, resweep)
	}
	folders, err := folderNames(ctx, c, account)
	if err != nil {
		return fmt.Errorf("looking for %s after a create of unknown outcome: %w", generated, err)
	}
	var errs []error
	for _, name := range testFolderNames(folders, generated) {
		errs = append(errs, deleteTestFolder(ctx, c, account, name, resweep))
	}
	return errors.Join(errs...)
}

// folderNames lists the account's folders. A partial answer is an error,
// since the folder looked for may be among those it could not read.
func folderNames(ctx context.Context, c *client.Client, account string) ([]string, error) {
	resp, err := c.Message.ListFolders(ctx, connect.NewRequest(&chainmailv1.ListFoldersRequest{Accounts: []string{account}}))
	if err != nil {
		return nil, fmt.Errorf("ListFolders: %w (%s)", err, hint(err))
	}
	if p := resp.Msg.GetPartial(); len(p) > 0 {
		return nil, fmt.Errorf("ListFolders could not read %s: %s", account, p[0].GetReason())
	}
	var out []string
	for _, af := range resp.Msg.GetAccounts() {
		for _, f := range af.GetFolders() {
			out = append(out, f.GetName())
		}
	}
	return out, nil
}

func TestTestFolderNames(t *testing.T) {
	t.Parallel()
	const gen = "chainmail-test-abc123"
	tests := []struct {
		name    string
		folders []string
		want    []string
	}{
		{"exactly the name", []string{"INBOX", gen, "Archive"}, []string{gen}},
		{"under a dot prefix", []string{"INBOX", "INBOX.Sent", "INBOX." + gen}, []string{"INBOX." + gen}},
		{"under a slash prefix", []string{"INBOX", "INBOX/" + gen}, []string{"INBOX/" + gen}},
		{"created nowhere", []string{"INBOX", "INBOX.Sent"}, nil},
		{"another test's folder", []string{"INBOX.chainmail-test-other", "chainmail-test-abc1234"}, nil},
		{"deeper than a prefix", []string{"INBOX.Projects." + gen, "Projects/" + gen + "/x"}, nil},
		{"the name inside another", []string{"x" + gen, "INBOXx" + gen}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := testFolderNames(tt.folders, gen); !slices.Equal(got, tt.want) {
				t.Errorf("testFolderNames(%q) = %q, want %q", tt.folders, got, tt.want)
			}
		})
	}
}

const cleanupConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[policy]
allow_expunge = true
allow_folder_delete = true
`

// lostCreate is a fake store on a server with the personal namespace
// "INBOX.", whose CreateFolder creates the folder and then fails, as when
// the daemon's answer is lost after the server applied the CREATE. It
// remembers whether a DeleteFolder asked for force.
type lostCreate struct {
	*fake.Store
	forced atomic.Bool
}

func (l *lostCreate) CreateFolder(ctx context.Context, name string) (string, error) {
	if _, err := l.Store.CreateFolder(ctx, name); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%w: the connection dropped after CREATE", mailstore.ErrUnavailable)
}

func (l *lostCreate) DeleteFolder(ctx context.Context, name string, force bool) error {
	if force {
		l.forced.Store(true)
	}
	return l.Store.DeleteFolder(ctx, name, force)
}

// A create whose outcome the test never learned may have made the folder
// under the server's namespace prefix, where the generated name alone does
// not reach it. Cleanup finds it there and deletes it, and nothing else.
func TestRemoveTestFolderAfterALostCreate(t *testing.T) {
	t.Parallel()
	st := fake.New("host", false)
	st.SetNamespace("INBOX.", ".")
	for _, f := range []string{"INBOX.Keep", "INBOX.chainmail-test-other"} {
		st.AddFolder(f, mailstore.RoleNone)
	}
	kept := st.Add(fake.Message{Folder: "INBOX.Keep", Subject: "kept", MessageID: "<kept@example.com>", Date: time.Now()})
	lost := &lostCreate{Store: st}
	h := daemontest.Start(t, cleanupConfig, daemontest.Options{
		NewStore: func(config.ResolvedAccount) mailstore.Store { return lost },
	})
	c := h.Client
	const gen = "chainmail-test-lost1"
	_, err := c.Organize.CreateFolder(t.Context(), connect.NewRequest(&chainmailv1.CreateFolderRequest{Account: "host", Name: gen}))
	if err == nil {
		t.Fatal("CreateFolder succeeded, want the lost answer's error")
	}
	if !hasFolder(t, st, "INBOX."+gen) {
		t.Fatal("the fake did not create INBOX." + gen + "; the test proves nothing")
	}

	if err := removeTestFolder(t.Context(), c, "host", gen, "", nil); err != nil {
		t.Fatalf("removeTestFolder: %v", err)
	}
	if hasFolder(t, st, "INBOX."+gen) {
		t.Errorf("INBOX.%s is still there after cleanup", gen)
	}
	for _, f := range []string{"INBOX", "INBOX.Keep", "INBOX.chainmail-test-other", "Sent", "Trash"} {
		if !hasFolder(t, st, f) {
			t.Errorf("cleanup deleted %s, which the test did not create", f)
		}
	}
	if raw := st.RawMessage(kept); len(raw) == 0 {
		t.Error("cleanup removed mail in INBOX.Keep")
	}
	if lost.forced.Load() {
		t.Error("cleanup deleted with force")
	}
}

// A create that took no effect leaves nothing to find, which is not an error.
func TestRemoveTestFolderAfterACreateThatFailed(t *testing.T) {
	t.Parallel()
	st := fake.New("host", false)
	st.SetNamespace("INBOX.", ".")
	h := daemontest.Start(t, cleanupConfig, daemontest.Options{
		NewStore: func(config.ResolvedAccount) mailstore.Store { return st },
	})
	if err := removeTestFolder(t.Context(), h.Client, "host", "chainmail-test-none1", "", nil); err != nil {
		t.Fatalf("removeTestFolder: %v, want nil when the folder was never made", err)
	}
	if n := st.Calls("DeleteFolder"); n != 0 {
		t.Errorf("DeleteFolder calls = %d, want none for a folder that is not listed", n)
	}
}

// A name the server gave is deleted by that name, and a folder already gone
// is fine.
func TestRemoveTestFolderByTheServersName(t *testing.T) {
	t.Parallel()
	st := fake.New("host", false)
	st.SetNamespace("INBOX.", ".")
	h := daemontest.Start(t, cleanupConfig, daemontest.Options{
		NewStore: func(config.ResolvedAccount) mailstore.Store { return st },
	})
	c := h.Client
	const gen = "chainmail-test-known1"
	resp, err := c.Organize.CreateFolder(t.Context(), connect.NewRequest(&chainmailv1.CreateFolderRequest{Account: "host", Name: gen}))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := removeTestFolder(t.Context(), c, "host", gen, resp.Msg.GetName(), nil); err != nil {
			t.Fatalf("removeTestFolder: %v", err)
		}
	}
	if hasFolder(t, st, "INBOX."+gen) {
		t.Errorf("INBOX.%s is still there after cleanup", gen)
	}
}

func hasFolder(t *testing.T, st *fake.Store, name string) bool {
	t.Helper()
	list, err := st.ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(list, func(f mailstore.Folder) bool { return f.Name == name })
}
