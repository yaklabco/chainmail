package watch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"maps"
	"slices"
	"time"
)

// Mode is how a folder is synced, as sync_folders records it. The API
// carries it as a plain string.
type Mode string

// Sync modes. A row no pass has written yet has the empty mode.
const (
	ModeIdle Mode = "idle"
	ModePoll Mode = "poll"
)

// FolderRow is one sync_folders row.
type FolderRow struct {
	Account       string
	Folder        string
	UIDValidity   uint32
	UIDNext       uint32
	HighestModSeq uint64
	CondStore     bool
	Mode          Mode   // "", ModeIdle or ModePoll
	State         Status // a status state, or ""
	LastSyncAt    time.Time
	LastSweepAt   time.Time
	LastError     string // an error reason such as FOLDER_NOT_FOUND, never server text
}

// knownUID is one sync_uids row.
type knownUID struct {
	GmailMsgID uint64
	FlagsHash  string // "" for CONDSTORE folders
}

// State reads and updates folder sync state in state.db. The writers that
// must commit with a pass's events (PutFolder and the UID writers) are
// functions over a caller's transaction instead.
type State struct {
	db *sql.DB
}

// NewState returns a State over db, which must hold the state schema.
func NewState(db *sql.DB) *State {
	return &State{db: db}
}

// folderColumns lists the sync_folders columns scanFolder reads, in order.
const folderColumns = `account, folder, uidvalidity, uidnext, highest_modseq, condstore,
	mode, state, last_sync_at, last_sweep_at, last_error`

// folderScan holds one sync_folders row as stored, before conversion.
type folderScan struct {
	account, folder                  string
	uidValidity, uidNext, modSeq     int64
	condStore                        int64
	mode, state, lastSync, lastSweep string
	lastError                        string
}

func (f *folderScan) dest() []any {
	return []any{
		&f.account, &f.folder, &f.uidValidity, &f.uidNext, &f.modSeq, &f.condStore,
		&f.mode, &f.state, &f.lastSync, &f.lastSweep, &f.lastError,
	}
}

func (f *folderScan) row() (FolderRow, error) {
	r := FolderRow{
		Account:       f.account,
		Folder:        f.folder,
		UIDValidity:   uint32(f.uidValidity),
		UIDNext:       uint32(f.uidNext),
		HighestModSeq: uint64(f.modSeq),
		CondStore:     f.condStore == 1,
		Mode:          Mode(f.mode),
		State:         Status(f.state),
		LastError:     f.lastError,
	}
	var err error
	if r.LastSyncAt, err = parseTime(f.lastSync); err != nil {
		return FolderRow{}, fmt.Errorf("folder %s/%s last_sync_at: %w", f.account, f.folder, err)
	}
	if r.LastSweepAt, err = parseTime(f.lastSweep); err != nil {
		return FolderRow{}, fmt.Errorf("folder %s/%s last_sweep_at: %w", f.account, f.folder, err)
	}
	return r, nil
}

// Folder returns the folder's row. The bool is false when it has none.
func (s *State) Folder(ctx context.Context, account, folder string) (FolderRow, bool, error) {
	var f folderScan
	err := s.db.QueryRowContext(ctx,
		"SELECT "+folderColumns+" FROM sync_folders WHERE account = ? AND folder = ?",
		account, folder).Scan(f.dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return FolderRow{}, false, nil
	}
	if err != nil {
		return FolderRow{}, false, fmt.Errorf("reading folder %s/%s: %w", account, folder, err)
	}
	r, err := f.row()
	if err != nil {
		return FolderRow{}, false, err
	}
	return r, true, nil
}

// Folders returns every folder row, ordered by account, then folder.
func (s *State) Folders(ctx context.Context) ([]FolderRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+folderColumns+" FROM sync_folders ORDER BY account, folder")
	if err != nil {
		return nil, fmt.Errorf("listing folders: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var raw []folderScan
	for rows.Next() {
		var f folderScan
		if err := rows.Scan(f.dest()...); err != nil {
			return nil, fmt.Errorf("listing folders: %w", err)
		}
		raw = append(raw, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing folders: %w", err)
	}
	out := make([]FolderRow, 0, len(raw))
	for i := range raw {
		r, err := raw[i].row()
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// uids returns the folder's known uids. It is empty, not nil, when there
// are none.
func (s *State) uids(ctx context.Context, account, folder string) (map[uint32]knownUID, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT uid, gmail_msgid, flags_hash FROM sync_uids WHERE account = ? AND folder = ?",
		account, folder)
	if err != nil {
		return nil, fmt.Errorf("reading UIDs of %s/%s: %w", account, folder, err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	out := make(map[uint32]knownUID)
	for rows.Next() {
		var (
			uid, msgID int64
			k          knownUID
		)
		if err := rows.Scan(&uid, &msgID, &k.FlagsHash); err != nil {
			return nil, fmt.Errorf("reading UIDs of %s/%s: %w", account, folder, err)
		}
		k.GmailMsgID = uint64(msgID)
		out[uint32(uid)] = k
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading UIDs of %s/%s: %w", account, folder, err)
	}
	return out, nil
}

// validMode reports whether mode is one sync_folders accepts.
func validMode(mode Mode) bool {
	switch mode {
	case "", ModeIdle, ModePoll:
		return true
	default:
		return false
	}
}

// SetStatus updates mode, state and last_error only, creating the row when missing.
func (s *State) SetStatus(ctx context.Context, account, folder string, mode Mode, state Status, lastError string) error {
	if !validMode(mode) {
		return fmt.Errorf("invalid folder mode %q", mode)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sync_folders (account, folder, mode, state, last_error) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (account, folder) DO UPDATE SET
		   mode = excluded.mode, state = excluded.state, last_error = excluded.last_error`,
		account, folder, string(mode), string(state), lastError)
	if err != nil {
		return fmt.Errorf("setting status of %s/%s: %w", account, folder, err)
	}
	return nil
}

// DropFolder deletes the folder's sync_folders row and its known UIDs in
// one transaction. A folder with no rows is not an error. The supervisor
// calls it when a folder it polled leaves the account's search scope, so
// the cache, which tracks the rows here, stops tracking it too.
func (s *State) DropFolder(ctx context.Context, account, folder string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning drop of %s/%s: %w", account, folder, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report

	if _, err := tx.ExecContext(ctx, "DELETE FROM sync_uids WHERE account = ? AND folder = ?", account, folder); err != nil {
		return fmt.Errorf("dropping UIDs of %s/%s: %w", account, folder, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sync_folders WHERE account = ? AND folder = ?", account, folder); err != nil {
		return fmt.Errorf("dropping folder %s/%s: %w", account, folder, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing drop of %s/%s: %w", account, folder, err)
	}
	return nil
}

// boolInt renders b as the 0 or 1 the condstore column holds.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// PutFolder writes every column of r in tx, inserting or replacing the row.
func PutFolder(ctx context.Context, tx *sql.Tx, r FolderRow) error {
	if !validMode(r.Mode) {
		return fmt.Errorf("invalid folder mode %q", r.Mode)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO sync_folders (`+folderColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (account, folder) DO UPDATE SET
		   uidvalidity = excluded.uidvalidity, uidnext = excluded.uidnext,
		   highest_modseq = excluded.highest_modseq, condstore = excluded.condstore,
		   mode = excluded.mode, state = excluded.state,
		   last_sync_at = excluded.last_sync_at, last_sweep_at = excluded.last_sweep_at,
		   last_error = excluded.last_error`,
		r.Account, r.Folder, int64(r.UIDValidity), int64(r.UIDNext), int64(r.HighestModSeq), boolInt(r.CondStore),
		string(r.Mode), string(r.State), formatTime(r.LastSyncAt), formatTime(r.LastSweepAt), r.LastError)
	if err != nil {
		return fmt.Errorf("writing folder %s/%s: %w", r.Account, r.Folder, err)
	}
	return nil
}

// replaceUIDs replaces the folder's known UIDs with uids in tx.
func replaceUIDs(ctx context.Context, tx *sql.Tx, account, folder string, uids map[uint32]knownUID) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM sync_uids WHERE account = ? AND folder = ?", account, folder); err != nil {
		return fmt.Errorf("clearing UIDs of %s/%s: %w", account, folder, err)
	}
	return writeUIDs(ctx, tx, account, folder, uids,
		"INSERT INTO sync_uids (account, folder, uid, gmail_msgid, flags_hash) VALUES (?, ?, ?, ?, ?)")
}

// putUIDs inserts or updates uids in the folder's known UIDs in tx.
func putUIDs(ctx context.Context, tx *sql.Tx, account, folder string, uids map[uint32]knownUID) error {
	return writeUIDs(ctx, tx, account, folder, uids,
		`INSERT INTO sync_uids (account, folder, uid, gmail_msgid, flags_hash) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (account, folder, uid) DO UPDATE SET
		   gmail_msgid = excluded.gmail_msgid, flags_hash = excluded.flags_hash`)
}

// writeUIDs runs the insert for every UID through one prepared statement,
// in ascending UID order, which suits the table's primary key.
func writeUIDs(ctx context.Context, tx *sql.Tx, account, folder string, uids map[uint32]knownUID, query string) error {
	if len(uids) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("preparing UID write for %s/%s: %w", account, folder, err)
	}
	defer func() { _ = stmt.Close() }() // closing a prepared statement only frees it
	for _, uid := range slices.Sorted(maps.Keys(uids)) {
		k := uids[uid]
		if _, err := stmt.ExecContext(ctx, account, folder, int64(uid), int64(k.GmailMsgID), k.FlagsHash); err != nil {
			return fmt.Errorf("writing UID %d of %s/%s: %w", uid, account, folder, err)
		}
	}
	return nil
}

// deleteUIDs removes uids from the folder's known UIDs in tx. UIDs it does
// not know are ignored.
func deleteUIDs(ctx context.Context, tx *sql.Tx, account, folder string, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, "DELETE FROM sync_uids WHERE account = ? AND folder = ? AND uid = ?")
	if err != nil {
		return fmt.Errorf("preparing UID delete for %s/%s: %w", account, folder, err)
	}
	defer func() { _ = stmt.Close() }() // closing a prepared statement only frees it
	for _, uid := range uids {
		if _, err := stmt.ExecContext(ctx, account, folder, int64(uid)); err != nil {
			return fmt.Errorf("deleting UID %d of %s/%s: %w", uid, account, folder, err)
		}
	}
	return nil
}

// flagsHashLen is how many hex characters flagsHash keeps.
const flagsHashLen = 32

// flagsHash is a stable hash of a message's flags and labels: hex SHA-256
// of the sorted, deduplicated flags, each followed by "\n", a 0x00 byte,
// then the sorted, deduplicated labels, each followed by "\n", truncated to
// 32 hex characters. The newlines keep ["ab"] and ["a", "b"] apart. The
// value is stored in sync_uids, so this encoding must never change.
func flagsHash(flags, labels []string) string {
	h := sha256.New()
	writeSet(h, flags)
	h.Write([]byte{0})
	writeSet(h, labels)
	return hex.EncodeToString(h.Sum(nil))[:flagsHashLen]
}

// writeSet writes the sorted, deduplicated values of set to h, each
// followed by a newline.
func writeSet(h hash.Hash, set []string) {
	sorted := slices.Compact(slices.Sorted(slices.Values(set)))
	for _, v := range sorted {
		h.Write([]byte(v))
		h.Write([]byte{'\n'})
	}
}
