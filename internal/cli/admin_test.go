package cli

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

// statusAt is a time on 2026-11-02 in UTC.
func statusAt(hour, minute int) *timestamppb.Timestamp {
	return timestamppb.New(time.Date(2026, 11, 2, hour, minute, 0, 0, time.UTC))
}

// admin status prints the synced folders under the daemon's details.
func TestStatusTableShowsSync(t *testing.T) {
	t.Parallel()
	r := &chainmailv1.GetStatusResponse{
		Version: "1.2.3", StartedAt: statusAt(8, 0), SecretNames: []string{"accounts/host", "tokens/gm"},
		Sync: []*chainmailv1.FolderSyncStatus{
			{Account: "gm", Folder: "[Gmail]/All Mail", Mode: "idle", State: "watching", LastSyncAt: statusAt(8, 58)},
			{Account: "host", Folder: "INBOX", Mode: "idle", State: "watching", Condstore: true, LastSyncAt: statusAt(8, 59)},
			{Account: "host", Folder: "Sent", Mode: "poll", State: "error", LastError: "FOLDER_NOT_FOUND"},
		},
	}
	var b strings.Builder
	if err := statusTable(&b, time.UTC, r); err != nil {
		t.Fatal(err)
	}
	golden(t, "admin_status_table", b.String())
}

// Without synced folders admin status prints what it always did, with no
// empty sync section.
func TestStatusTableWithoutSync(t *testing.T) {
	t.Parallel()
	r := &chainmailv1.GetStatusResponse{Version: "1.2.3", StartedAt: statusAt(8, 0), DevMode: true}
	var b strings.Builder
	if err := statusTable(&b, time.UTC, r); err != nil {
		t.Fatal(err)
	}
	want := "version   1.2.3\n" +
		"started   Mon, 02 Nov 2026 08:00 UTC\n" +
		"dev mode  true\n" +
		"secrets   \n"
	if b.String() != want {
		t.Errorf("statusTable =\n%q\nwant\n%q", b.String(), want)
	}
}

// Folder names can come from the server, so the sync table cleans them, and
// the error, as it does other server text.
func TestStatusSyncTableCleansText(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	err := statusSyncTable(&b, time.UTC, []*chainmailv1.FolderSyncStatus{
		{Account: "host", Folder: "a\tb\x1b[2J", Mode: "poll", State: "error", LastError: "FOLDER_NOT_FOUND\x07"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "ACCOUNT  FOLDER  MODE  STATE  LAST SYNC  ERROR\n" +
		"host     a b[2J  poll  error             FOLDER_NOT_FOUND\n"
	if b.String() != want {
		t.Errorf("statusSyncTable =\n%q\nwant\n%q", b.String(), want)
	}
}
