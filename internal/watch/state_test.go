package watch

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"
)

// inTx runs fn in a transaction on conn and commits it.
func inTx(t *testing.T, conn *sql.DB, fn func(ctx context.Context, tx *sql.Tx) error) {
	t.Helper()
	tx, err := conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(t.Context(), tx); err != nil {
		_ = tx.Rollback() // the fn error is the one to report
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestFolderRoundTrip(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	s := NewState(conn)
	if _, ok, err := s.Folder(t.Context(), "a", "INBOX"); err != nil || ok {
		t.Fatalf("Folder before any write = ok %v, %v, want false", ok, err)
	}
	full := FolderRow{
		Account:       "a",
		Folder:        "INBOX",
		UIDValidity:   4294967295,
		UIDNext:       4294967295,
		HighestModSeq: 1<<63 - 1,
		CondStore:     true,
		Mode:          "idle",
		State:         StateWatching,
		LastSyncAt:    t0.Add(123456789 * time.Nanosecond),
		LastSweepAt:   t0.Add(-time.Hour),
		LastError:     "",
	}
	bare := FolderRow{Account: "a", Folder: "Archive", UIDValidity: 7, UIDNext: 1}
	other := FolderRow{Account: "b", Folder: "INBOX", Mode: "poll", State: StateError, LastError: "FOLDER_NOT_FOUND"}
	inTx(t, conn, func(ctx context.Context, tx *sql.Tx) error {
		for _, r := range []FolderRow{other, full, bare} {
			if err := PutFolder(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})
	got, ok, err := s.Folder(t.Context(), "a", "INBOX")
	if err != nil || !ok || !reflect.DeepEqual(got, full) {
		t.Errorf("Folder = %+v, %v, %v, want %+v", got, ok, err, full)
	}
	rows, err := s.Folders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := []FolderRow{bare, full, other}; !reflect.DeepEqual(rows, want) {
		t.Errorf("Folders =\n%+v\nwant\n%+v", rows, want)
	}

	// PutFolder replaces every column, zero values included.
	replaced := FolderRow{Account: "a", Folder: "INBOX", UIDValidity: 9, UIDNext: 2}
	inTx(t, conn, func(ctx context.Context, tx *sql.Tx) error { return PutFolder(ctx, tx, replaced) })
	if got, _, _ := s.Folder(t.Context(), "a", "INBOX"); !reflect.DeepEqual(got, replaced) {
		t.Errorf("Folder after replace = %+v, want %+v", got, replaced)
	}
}

func TestPutFolderRejectsBadMode(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	tx, err := conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }() // nothing to keep
	if err := PutFolder(t.Context(), tx, FolderRow{Account: "a", Folder: "INBOX", Mode: "push"}); err == nil || !strings.Contains(err.Error(), `invalid folder mode "push"`) {
		t.Errorf("err = %v, want a mode error", err)
	}
}

func TestSetStatus(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	s := NewState(conn)

	// A missing row is created with only the status columns set.
	if err := s.SetStatus(t.Context(), "a", "INBOX", "", StateReconnecting, ""); err != nil {
		t.Fatal(err)
	}
	want := FolderRow{Account: "a", Folder: "INBOX", State: StateReconnecting}
	if got, ok, err := s.Folder(t.Context(), "a", "INBOX"); err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("Folder = %+v, %v, %v, want %+v", got, ok, err, want)
	}

	// An existing row keeps its checkpoint.
	checkpoint := FolderRow{
		Account: "a", Folder: "INBOX", UIDValidity: 5, UIDNext: 40, HighestModSeq: 900, CondStore: true,
		Mode: "idle", State: StateWatching, LastSyncAt: t0, LastSweepAt: t0,
	}
	inTx(t, conn, func(ctx context.Context, tx *sql.Tx) error { return PutFolder(ctx, tx, checkpoint) })
	if err := s.SetStatus(t.Context(), "a", "INBOX", "poll", StateError, "FOLDER_NOT_FOUND"); err != nil {
		t.Fatal(err)
	}
	want = checkpoint
	want.Mode, want.State, want.LastError = "poll", StateError, "FOLDER_NOT_FOUND"
	if got, _, _ := s.Folder(t.Context(), "a", "INBOX"); !reflect.DeepEqual(got, want) {
		t.Errorf("Folder after SetStatus = %+v, want %+v", got, want)
	}

	if err := s.SetStatus(t.Context(), "a", "INBOX", "push", StateError, ""); err == nil {
		t.Error("SetStatus accepted mode push")
	}
}

func TestUIDWriters(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	s := NewState(conn)
	h := flagsHash([]string{`\Seen`}, nil)

	inTx(t, conn, func(ctx context.Context, tx *sql.Tx) error {
		if err := replaceUIDs(ctx, tx, "a", "INBOX", map[uint32]knownUID{1: {}, 2: {FlagsHash: h}, 3: {}}); err != nil {
			return err
		}
		return replaceUIDs(ctx, tx, "a", "Archive", map[uint32]knownUID{1: {}})
	})
	assertUIDs := func(step, folder string, want map[uint32]knownUID) {
		t.Helper()
		got, err := s.uids(t.Context(), "a", folder)
		if err != nil {
			t.Fatalf("%s: UIDs: %v", step, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: UIDs(%s) = %v, want %v", step, folder, got, want)
		}
	}
	assertUIDs("replace", "INBOX", map[uint32]knownUID{1: {}, 2: {FlagsHash: h}, 3: {}})

	// replaceUIDs drops every UID not in the new set, in that folder only.
	inTx(t, conn, func(ctx context.Context, tx *sql.Tx) error {
		return replaceUIDs(ctx, tx, "a", "INBOX", map[uint32]knownUID{3: {GmailMsgID: 1 << 62}, 4: {}})
	})
	assertUIDs("second replace", "INBOX", map[uint32]knownUID{3: {GmailMsgID: 1 << 62}, 4: {}})
	assertUIDs("second replace", "Archive", map[uint32]knownUID{1: {}})

	// putUIDs inserts new UIDs and updates known ones.
	inTx(t, conn, func(ctx context.Context, tx *sql.Tx) error {
		return putUIDs(ctx, tx, "a", "INBOX", map[uint32]knownUID{4: {FlagsHash: h}, 5: {GmailMsgID: 77}})
	})
	assertUIDs("put", "INBOX", map[uint32]knownUID{3: {GmailMsgID: 1 << 62}, 4: {FlagsHash: h}, 5: {GmailMsgID: 77}})

	// deleteUIDs removes the listed UIDs and ignores unknown ones.
	inTx(t, conn, func(ctx context.Context, tx *sql.Tx) error {
		return deleteUIDs(ctx, tx, "a", "INBOX", []uint32{3, 5, 99})
	})
	assertUIDs("delete", "INBOX", map[uint32]knownUID{4: {FlagsHash: h}})

	// Empty inputs are no-ops, and replaceUIDs with none clears the folder.
	inTx(t, conn, func(ctx context.Context, tx *sql.Tx) error {
		if err := putUIDs(ctx, tx, "a", "INBOX", nil); err != nil {
			return err
		}
		if err := deleteUIDs(ctx, tx, "a", "INBOX", nil); err != nil {
			return err
		}
		return replaceUIDs(ctx, tx, "a", "Archive", nil)
	})
	assertUIDs("empty", "INBOX", map[uint32]knownUID{4: {FlagsHash: h}})
	assertUIDs("empty", "Archive", map[uint32]knownUID{})
}

// A baseline of a large folder writes every row through one prepared
// statement in a single transaction.
func TestReplaceUIDsLargeFolder(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	const n = 20_000
	uids := make(map[uint32]knownUID, n)
	for uid := range uint32(n) {
		uids[uid+1] = knownUID{}
	}
	inTx(t, conn, func(ctx context.Context, tx *sql.Tx) error { return replaceUIDs(ctx, tx, "a", "INBOX", uids) })
	got, err := NewState(conn).uids(t.Context(), "a", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Errorf("%d UIDs stored, want %d", len(got), n)
	}
}

func TestFlagsHash(t *testing.T) {
	t.Parallel()
	base := flagsHash([]string{`\Seen`, `\Flagged`}, []string{`\Inbox`})
	// The value is stored in sync_uids, so it must never change between releases.
	if base != "222464454d1426243c33ca6b9fe139c9" {
		t.Errorf("flagsHash = %s, want the pinned value", base)
	}
	if empty := flagsHash(nil, nil); empty != "6e340b9cffb37a989ca544e6bb780a2c" {
		t.Errorf("flagsHash(nil, nil) = %s, want the pinned value", empty)
	}
	tests := []struct {
		name          string
		flags, labels []string
		same          bool
	}{
		{"flag order", []string{`\Flagged`, `\Seen`}, []string{`\Inbox`}, true},
		{"duplicate flags", []string{`\Seen`, `\Flagged`, `\Seen`}, []string{`\Inbox`}, true},
		{"duplicate labels", []string{`\Seen`, `\Flagged`}, []string{`\Inbox`, `\Inbox`}, true},
		{"flag removed", []string{`\Seen`}, []string{`\Inbox`}, false},
		{"label removed", []string{`\Seen`, `\Flagged`}, nil, false},
		{"label moved to flags", []string{`\Seen`, `\Flagged`, `\Inbox`}, nil, false},
		{"flags joined", []string{`\Seen\Flagged`}, []string{`\Inbox`}, false},
	}
	for _, tt := range tests {
		got := flagsHash(tt.flags, tt.labels)
		if (got == base) != tt.same {
			t.Errorf("%s: flagsHash = %s, base %s, want same %v", tt.name, got, base, tt.same)
		}
	}
	if flagsHash([]string{"a"}, nil) == flagsHash(nil, []string{"a"}) {
		t.Error("a flag and a label with the same name hash alike")
	}
	in := []string{`\Seen`, `\Flagged`}
	flagsHash(in, nil)
	if in[0] != `\Seen` {
		t.Error("flagsHash reordered its input")
	}
}
