package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/render"
)

// Row is one cached message header. Empty lists read back as nil.
type Row struct {
	Account, Folder        string
	UIDValidity, UID       uint32
	GmailMsgID, GmailThrID uint64 // 0 outside Google accounts
	InternalDate, Date     time.Time
	Size                   int64
	Subject                string
	From, To, Cc, Bcc      []mailstore.Address
	MessageID              string // without angle brackets
	InReplyTo, References  []string
	ThreadID               string
	Flags, Labels          []string
	HasAttachment          bool
	Attachments            []mailstore.Attachment
	// AttachmentsUnknown says the header was read without its body
	// structure, so HasAttachment is false and Attachments nil whatever the
	// message holds. A full read clears it. A degraded read of a row whose
	// attachments are known leaves them, and this, as they were.
	AttachmentsUnknown bool
}

// Ref returns the row's message reference.
func (r Row) Ref() ref.Ref {
	return ref.Ref{
		Account:     r.Account,
		Folder:      r.Folder,
		UIDValidity: r.UIDValidity,
		UID:         r.UID,
		GmailMsgID:  r.GmailMsgID,
	}
}

// Summary returns the row as a search summary. Preview and Sample are nil.
func (r Row) Summary() mailstore.Summary {
	return mailstore.Summary{
		Ref:                r.Ref(),
		Date:               r.Date,
		InternalDate:       r.InternalDate,
		From:               r.From,
		To:                 r.To,
		Subject:            r.Subject,
		Flags:              r.Flags,
		Size:               r.Size,
		HasAttachment:      r.HasAttachment,
		AttachmentsUnknown: r.AttachmentsUnknown,
		ThreadID:           r.ThreadID,
		MessageID:          r.MessageID,
		Labels:             r.Labels,
	}
}

// Message returns the row and body b as a message ready to render, as
// FetchMessages would. mailstore.Message has no Bcc, so none is set.
func (r Row) Message(b Body) *mailstore.Message {
	return &mailstore.Message{
		Summary:     r.Summary(),
		Cc:          r.Cc,
		MessageID:   r.MessageID,
		InReplyTo:   r.InReplyTo,
		References:  r.References,
		Raw:         b.Raw,
		TextClipped: b.TextClipped,
		Attachments: r.Attachments,
	}
}

// msgKey selects one messages row by its unique key; refArgs supplies the
// arguments. Column names are unqualified, so it also fits ref_recovery.
const msgKey = "account = ? AND folder = ? AND uidvalidity = ? AND uid = ?"

// msgKeyM is msgKey over messages aliased m, for joins.
const msgKeyM = "m.account = ? AND m.folder = ? AND m.uidvalidity = ? AND m.uid = ?"

func refArgs(r ref.Ref) []any {
	return []any{r.Account, r.Folder, int64(r.UIDValidity), int64(r.UID)}
}

// writeColumns are the messages columns PutRows writes, in rowArgs order.
// The first four are the unique key.
var writeColumns = []string{
	"account", "folder", "uidvalidity", "uid", "gmail_msgid", "gmail_thrid",
	"internal_date", "date", "size", "subject",
	"from_json", "to_json", "cc_json", "bcc_json",
	"message_id", "in_reply_to_json", "references_json", "thread_id",
	"flags_json", "labels_json", "has_attachment", "attachments_json",
	"subject_norm", "from_norm", "to_norm", "cc_norm", "bcc_norm",
	"flags_norm", "labels_norm", "attachments_norm", "cached_at", "attachments_unknown",
}

// attachmentColumns are the messages columns that describe attachments.
// A row read without its body structure leaves them as they are when the
// stored row's attachments are known.
var attachmentColumns = []string{"has_attachment", "attachments_json", "attachments_norm", "attachments_unknown"}

// upsertRow inserts a row or updates every column but the key, keeping the
// row's id (and so its body and failures), and returns the id. A row read
// without its body structure does not overwrite attachments the stored row
// knows: those take a parser regression or a server change to lose, and
// the old values are the better answer.
var upsertRow = func() string {
	set := make([]string, 0, len(writeColumns)-4)
	for _, c := range writeColumns[4:] {
		if slices.Contains(attachmentColumns, c) {
			set = append(set, c+" = CASE WHEN excluded.attachments_unknown = 1 AND messages.attachments_unknown = 0 THEN messages."+
				c+" ELSE excluded."+c+" END")
			continue
		}
		set = append(set, c+" = excluded."+c)
	}
	return "INSERT INTO messages (" + strings.Join(writeColumns, ", ") + ") VALUES (" +
		placeholders(len(writeColumns)) + ") ON CONFLICT (account, folder, uidvalidity, uid) DO UPDATE SET " +
		strings.Join(set, ", ") + " RETURNING id"
}()

// rowArgs returns r's values for writeColumns, stamped with cachedAt.
func rowArgs(r Row, cachedAt int64) ([]any, error) {
	from, err := jsonList(r.From)
	if err != nil {
		return nil, err
	}
	to, err := jsonList(r.To)
	if err != nil {
		return nil, err
	}
	cc, err := jsonList(r.Cc)
	if err != nil {
		return nil, err
	}
	bcc, err := jsonList(r.Bcc)
	if err != nil {
		return nil, err
	}
	inReplyTo, err := jsonList(r.InReplyTo)
	if err != nil {
		return nil, err
	}
	references, err := jsonList(r.References)
	if err != nil {
		return nil, err
	}
	flags, err := jsonList(r.Flags)
	if err != nil {
		return nil, err
	}
	labels, err := jsonList(r.Labels)
	if err != nil {
		return nil, err
	}
	attachments, err := jsonList(r.Attachments)
	if err != nil {
		return nil, err
	}
	return []any{
		r.Account, r.Folder, int64(r.UIDValidity), int64(r.UID), int64(r.GmailMsgID), int64(r.GmailThrID),
		r.InternalDate.Unix(), r.Date.Unix(), r.Size, r.Subject,
		from, to, cc, bcc,
		r.MessageID, inReplyTo, references, r.ThreadID,
		flags, labels, boolInt(r.HasAttachment), attachments,
		normSubject(r.Subject), normAddresses(r.From), normAddresses(r.To), normAddresses(r.Cc), normAddresses(r.Bcc),
		normList(r.Flags), normList(r.Labels), normAttachments(r.Attachments), cachedAt, boolInt(r.AttachmentsUnknown),
	}, nil
}

// jsonList encodes a list for a *_json column; an empty list is "[]".
func jsonList[T any](v []T) (string, error) {
	if len(v) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encoding a list column: %w", err)
	}
	return string(b), nil
}

// decodeList decodes a *_json column; an empty list is nil.
func decodeList[T any](s string) ([]T, error) {
	var out []T
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// lineFree turns CR and LF into spaces, so a value never splits one
// element of a matching column into two.
var lineFree = strings.NewReplacer("\r", " ", "\n", " ")

// normList lower-cases values and joins them with "\n", with a leading and
// trailing "\n", so LIKE '%\nx\n%' matches element x exactly. No values
// give "\n".
func normList(values []string) string {
	var b strings.Builder
	b.WriteByte('\n')
	for _, v := range values {
		b.WriteString(strings.ToLower(lineFree.Replace(v)))
		b.WriteByte('\n')
	}
	return b.String()
}

// normAddresses is normList of name + " " + email for each address.
func normAddresses(list []mailstore.Address) string {
	values := make([]string, len(list))
	for i, a := range list {
		values[i] = a.Name + " " + a.Email
	}
	return normList(values)
}

// normAttachments is normList of the attachment file names.
func normAttachments(list []mailstore.Attachment) string {
	values := make([]string, len(list))
	for i, a := range list {
		values[i] = a.Filename
	}
	return normList(values)
}

// normSubject is the lower-cased subject with CR and LF turned into spaces.
func normSubject(s string) string {
	return strings.ToLower(lineFree.Replace(s))
}

// ftsHeader returns r's FTS subject, names and addresses columns: every
// display name and every email of From, To, Cc and Bcc, space-joined.
func ftsHeader(r Row) (subject, names, addresses string) {
	var ns, as []string
	for _, list := range [][]mailstore.Address{r.From, r.To, r.Cc, r.Bcc} {
		for _, a := range list {
			if a.Name != "" {
				ns = append(ns, a.Name)
			}
			if a.Email != "" {
				as = append(as, a.Email)
			}
		}
	}
	return r.Subject, strings.Join(ns, " "), strings.Join(as, " ")
}

// bodyText is the FTS body column for raw: its whole rendered text. A body
// that does not parse indexes as empty; get reports the parse error itself.
func bodyText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	b, err := render.Render(raw, render.Options{Full: true})
	if err != nil {
		return ""
	}
	return b.Text
}

// writeFTS replaces the FTS row of message id with r's header terms and
// body, the FTS body column ("" without a body).
func writeFTS(ctx context.Context, tx *sql.Tx, id int64, r Row, body string) error {
	subject, names, addresses := ftsHeader(r)
	_, err := tx.ExecContext(ctx,
		"INSERT OR REPLACE INTO messages_fts (rowid, subject, names, addresses, body) VALUES (?, ?, ?, ?, ?)",
		id, subject, names, addresses, body)
	if err != nil {
		return fmt.Errorf("indexing message %d: %w", id, err)
	}
	return nil
}

// storedBodyText is the FTS body column for message id's cached body, or ""
// when it has none.
func storedBodyText(ctx context.Context, tx *sql.Tx, id int64) (string, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, "SELECT raw FROM bodies WHERE msg = ?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the body of message %d: %w", id, err)
	}
	return bodyText(raw), nil
}

// PutRows upserts rows in one transaction and writes their FTS header terms.
// An upsert of an existing key keeps its body and body FTS terms. A row
// written for a UID recorded as unreadable deletes the record: the message
// has been read.
func (s *Store) PutRows(ctx context.Context, rows []Row) error {
	_, err := s.putRows(ctx, rows, time.Time{})
	return err
}

// PutNewRows upserts rows as PutRows does and, in the same transaction,
// clears window_done on each folder that got a row with internal_date at
// or after windowStart and no body: the folder's prefetch window lacks
// that body until the prefetcher stores it. It reports whether any such
// row was written. A zero windowStart (no prefetch window) clears nothing.
func (s *Store) PutNewRows(ctx context.Context, rows []Row, windowStart time.Time) (bool, error) {
	return s.putRows(ctx, rows, windowStart)
}

// putRows is PutRows and PutNewRows.
func (s *Store) putRows(ctx context.Context, rows []Row, windowStart time.Time) (bool, error) {
	if len(rows) == 0 {
		return false, nil
	}
	now := s.now().Unix()
	args := make([][]any, len(rows))
	for i, r := range rows {
		if r.Account == "" || r.Folder == "" || r.UIDValidity == 0 || r.UID == 0 {
			return false, fmt.Errorf("putting rows: row %d lacks its account, folder, UIDVALIDITY or UID", i)
		}
		a, err := rowArgs(r, now)
		if err != nil {
			return false, fmt.Errorf("putting rows: %w", err)
		}
		args[i] = a
	}
	uncovered := false
	err := s.inTx(ctx, "putting rows", func(tx *sql.Tx) error {
		upsert, err := tx.PrepareContext(ctx, upsertRow)
		if err != nil {
			return err
		}
		defer func() { _ = upsert.Close() }() // closing a prepared statement only frees it
		// open holds the account and folder of each row inside the window
		// with no body.
		open := map[[2]string]bool{}
		for i, r := range rows {
			var id int64
			if err := upsert.QueryRowContext(ctx, args[i]...).Scan(&id); err != nil {
				return fmt.Errorf("writing %s UID %d: %w", r.Folder, r.UID, err)
			}
			body, err := storedBodyText(ctx, tx, id)
			if err != nil {
				return err
			}
			if err := writeFTS(ctx, tx, id, r, body); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM unreadable WHERE "+msgKey, refArgs(r.Ref())...); err != nil {
				return fmt.Errorf("forgetting %s UID %d as unreadable: %w", r.Folder, r.UID, err)
			}
			if windowStart.IsZero() || r.InternalDate.Before(windowStart) {
				continue
			}
			var has bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM bodies WHERE msg = ?)", id).Scan(&has); err != nil {
				return fmt.Errorf("checking the body of %s UID %d: %w", r.Folder, r.UID, err)
			}
			if !has {
				open[[2]string{r.Account, r.Folder}] = true
			}
		}
		for k := range open {
			if _, err := tx.ExecContext(ctx, "UPDATE folders SET window_done = 0 WHERE account = ? AND folder = ?", k[0], k[1]); err != nil {
				return fmt.Errorf("clearing window_done of %s: %w", k[1], err)
			}
		}
		uncovered = len(open) > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return uncovered, nil
}

// SetFlags updates a cached row's flags and labels; false when no row.
func (s *Store) SetFlags(ctx context.Context, r ref.Ref, flags, labels []string) (bool, error) {
	fj, err := jsonList(flags)
	if err != nil {
		return false, fmt.Errorf("setting flags: %w", err)
	}
	lj, err := jsonList(labels)
	if err != nil {
		return false, fmt.Errorf("setting flags: %w", err)
	}
	args := append([]any{fj, lj, normList(flags), normList(labels)}, refArgs(r)...)
	res, err := s.db.ExecContext(ctx,
		"UPDATE messages SET flags_json = ?, labels_json = ?, flags_norm = ?, labels_norm = ? WHERE "+msgKey, args...)
	if err != nil {
		return false, s.fail("setting flags", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, s.fail("setting flags", err)
	}
	return n > 0, nil
}

// rowColumns are the messages columns a rowScan reads, over alias m.
const rowColumns = `m.id, m.account, m.folder, m.uidvalidity, m.uid, m.gmail_msgid, m.gmail_thrid,
	m.internal_date, m.date, m.size, m.subject, m.from_json, m.to_json, m.cc_json, m.bcc_json,
	m.message_id, m.in_reply_to_json, m.references_json, m.thread_id, m.flags_json, m.labels_json,
	m.has_attachment, m.attachments_json, m.attachments_unknown`

// rowScan holds one messages row as stored, before decoding.
type rowScan struct {
	id                                       int64
	account, folder                          string
	uidValidity, uid, gmailMsgID, gmailThrID int64
	internalDate, date, size                 int64
	subject, from, to, cc, bcc               string
	messageID, inReplyTo, references         string
	threadID, flags, labels                  string
	hasAttachment                            int64
	attachments                              string
	attachmentsUnknown                       int64
}

func (sc *rowScan) dest() []any {
	return []any{
		&sc.id, &sc.account, &sc.folder, &sc.uidValidity, &sc.uid, &sc.gmailMsgID, &sc.gmailThrID,
		&sc.internalDate, &sc.date, &sc.size, &sc.subject, &sc.from, &sc.to, &sc.cc, &sc.bcc,
		&sc.messageID, &sc.inReplyTo, &sc.references, &sc.threadID, &sc.flags, &sc.labels,
		&sc.hasAttachment, &sc.attachments, &sc.attachmentsUnknown,
	}
}

func (sc *rowScan) row() (Row, error) {
	r := Row{
		Account:            sc.account,
		Folder:             sc.folder,
		UIDValidity:        uint32(sc.uidValidity),
		UID:                uint32(sc.uid),
		GmailMsgID:         uint64(sc.gmailMsgID),
		GmailThrID:         uint64(sc.gmailThrID),
		InternalDate:       fromUnix(sc.internalDate),
		Date:               fromUnix(sc.date),
		Size:               sc.size,
		Subject:            sc.subject,
		MessageID:          sc.messageID,
		ThreadID:           sc.threadID,
		HasAttachment:      sc.hasAttachment == 1,
		AttachmentsUnknown: sc.attachmentsUnknown == 1,
	}
	var err error
	if r.From, err = decodeList[mailstore.Address](sc.from); err != nil {
		return Row{}, sc.decodeErr("from_json", err)
	}
	if r.To, err = decodeList[mailstore.Address](sc.to); err != nil {
		return Row{}, sc.decodeErr("to_json", err)
	}
	if r.Cc, err = decodeList[mailstore.Address](sc.cc); err != nil {
		return Row{}, sc.decodeErr("cc_json", err)
	}
	if r.Bcc, err = decodeList[mailstore.Address](sc.bcc); err != nil {
		return Row{}, sc.decodeErr("bcc_json", err)
	}
	if r.InReplyTo, err = decodeList[string](sc.inReplyTo); err != nil {
		return Row{}, sc.decodeErr("in_reply_to_json", err)
	}
	if r.References, err = decodeList[string](sc.references); err != nil {
		return Row{}, sc.decodeErr("references_json", err)
	}
	if r.Flags, err = decodeList[string](sc.flags); err != nil {
		return Row{}, sc.decodeErr("flags_json", err)
	}
	if r.Labels, err = decodeList[string](sc.labels); err != nil {
		return Row{}, sc.decodeErr("labels_json", err)
	}
	if r.Attachments, err = decodeList[mailstore.Attachment](sc.attachments); err != nil {
		return Row{}, sc.decodeErr("attachments_json", err)
	}
	return r, nil
}

func (sc *rowScan) decodeErr(column string, err error) error {
	return fmt.Errorf("cached message %d %s: %w", sc.id, column, err)
}

// queryRower is *sql.DB or *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// loadRow returns the id and row cached for r. The bool is false when there
// is none.
func loadRow(ctx context.Context, q queryRower, r ref.Ref) (int64, Row, bool, error) {
	var sc rowScan
	err := q.QueryRowContext(ctx, "SELECT "+rowColumns+" FROM messages m WHERE "+msgKeyM, refArgs(r)...).Scan(sc.dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, Row{}, false, nil
	}
	if err != nil {
		return 0, Row{}, false, err
	}
	row, err := sc.row()
	if err != nil {
		return 0, Row{}, false, err
	}
	return sc.id, row, true, nil
}

// queryRows runs a SELECT of rowColumns and decodes its rows, in order. It
// closes the Rows before decoding, so the single connection is free again.
func (s *Store) queryRows(ctx context.Context, query string, args ...any) ([]Row, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var raw []rowScan
	for rows.Next() {
		var sc rowScan
		if err := rows.Scan(sc.dest()...); err != nil {
			return nil, err
		}
		raw = append(raw, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(raw))
	for i := range raw {
		r, err := raw[i].row()
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Row looks a message up by account, folder, UIDVALIDITY and UID.
func (s *Store) Row(ctx context.Context, r ref.Ref) (Row, bool, error) {
	_, row, ok, err := loadRow(ctx, s.db, r)
	if err != nil {
		return Row{}, false, s.fail("reading a row", err)
	}
	return row, ok, nil
}

// rowsChunk bounds the UIDs of one IN list, well under SQLite's limit on
// bound parameters.
const rowsChunk = 500

// Rows returns the cached rows of uids in one folder, in the order of uids, leaving out missing ones.
func (s *Store) Rows(ctx context.Context, src mailstore.Source, account string, uids []uint32) ([]Row, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	byUID := make(map[uint32]Row, len(uids))
	for chunk := range slices.Chunk(uids, rowsChunk) {
		args := make([]any, 0, 3+len(chunk))
		args = append(args, account, src.Folder, int64(src.UIDValidity))
		for _, u := range chunk {
			args = append(args, int64(u))
		}
		rows, err := s.queryRows(ctx, "SELECT "+rowColumns+
			" FROM messages m WHERE m.account = ? AND m.folder = ? AND m.uidvalidity = ? AND m.uid IN ("+
			placeholders(len(chunk))+")", args...)
		if err != nil {
			return nil, s.fail("reading rows", err)
		}
		for _, r := range rows {
			byUID[r.UID] = r
		}
	}
	out := make([]Row, 0, len(uids))
	for _, u := range uids {
		if r, ok := byUID[u]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// ThreadRows returns the account's cached rows with threadID, oldest first by Date then InternalDate.
// Rows that tie are ordered by folder and UID. An empty threadID has no rows.
func (s *Store) ThreadRows(ctx context.Context, account, threadID string) ([]Row, error) {
	if threadID == "" {
		return nil, nil
	}
	rows, err := s.queryRows(ctx, "SELECT "+rowColumns+
		" FROM messages m WHERE m.account = ? AND m.thread_id = ? ORDER BY m.date, m.internal_date, m.folder, m.uid",
		account, threadID)
	if err != nil {
		return nil, s.fail("reading a thread", err)
	}
	return rows, nil
}

// RootThreadRows returns the account's cached rows that a standard (not
// Google) server's Thread finds for the thread rooted at root: the rows
// whose Message-ID is root, or whose References or In-Reply-To hold root
// as a whole entry, bare or in angle brackets. A row joins whatever its own
// thread ID is. They are ordered as ThreadRows orders them. An empty root
// has no rows.
func (s *Store) RootThreadRows(ctx context.Context, account, root string) ([]Row, error) {
	if root == "" {
		return nil, nil
	}
	bracketed := "<" + root + ">"
	rows, err := s.queryRows(ctx, "SELECT "+rowColumns+` FROM messages m WHERE m.account = ? AND (
		m.message_id IN (?, ?)
		OR EXISTS (SELECT 1 FROM json_each(m.references_json) WHERE value IN (?, ?))
		OR EXISTS (SELECT 1 FROM json_each(m.in_reply_to_json) WHERE value IN (?, ?)))
		ORDER BY m.date, m.internal_date, m.folder, m.uid`,
		account, root, bracketed, root, bracketed, root, bracketed)
	if err != nil {
		return nil, s.fail("reading a thread by its root", err)
	}
	return rows, nil
}

// DeleteRow deletes a cached row with its body, body failure and FTS row,
// and the message's unreadable record: the message is gone either way. A
// missing row or record is not an error.
func (s *Store) DeleteRow(ctx context.Context, r ref.Ref) error {
	return s.inTx(ctx, "deleting a row", func(tx *sql.Tx) error {
		if err := dropMessages(ctx, tx, msgKey, refArgs(r)...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM unreadable WHERE "+msgKey, refArgs(r)...); err != nil {
			return fmt.Errorf("forgetting an unreadable message: %w", err)
		}
		return nil
	})
}

// dropMessages deletes the messages rows matching where, with their FTS
// rows; bodies and body failures go with them through ON DELETE CASCADE.
func dropMessages(ctx context.Context, tx *sql.Tx, where string, args ...any) error {
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM messages_fts WHERE rowid IN (SELECT id FROM messages WHERE "+where+")", args...); err != nil {
		return fmt.Errorf("deleting index rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE "+where, args...); err != nil {
		return fmt.Errorf("deleting rows: %w", err)
	}
	return nil
}

// DropFolder deletes a folder's rows (and bodies, failures, FTS rows), its
// progress row and its unreadable records. With keepRecovery, each row's
// Message-ID is first copied to ref_recovery.
func (s *Store) DropFolder(ctx context.Context, account, folder string, keepRecovery bool) error {
	now := s.now().Unix()
	return s.inTx(ctx, "dropping a folder", func(tx *sql.Tx) error {
		if keepRecovery {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR REPLACE INTO ref_recovery (account, folder, uidvalidity, uid, message_id, removed_at)
				 SELECT account, folder, uidvalidity, uid, message_id, ? FROM messages
				 WHERE account = ? AND folder = ? AND message_id != ''`,
				now, account, folder); err != nil {
				return fmt.Errorf("keeping Message-IDs: %w", err)
			}
		}
		if err := dropMessages(ctx, tx, "account = ? AND folder = ?", account, folder); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM folders WHERE account = ? AND folder = ?", account, folder); err != nil {
			return fmt.Errorf("deleting progress: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM unreadable WHERE account = ? AND folder = ?", account, folder); err != nil {
			return fmt.Errorf("deleting unreadable records: %w", err)
		}
		return nil
	})
}

// DropAccount deletes every row, progress row, recovery row and unreadable
// record of the account. Its download budget stays, so a rebuild cannot exceed the day's.
func (s *Store) DropAccount(ctx context.Context, account string) error {
	return s.inTx(ctx, "dropping an account", func(tx *sql.Tx) error {
		if err := dropMessages(ctx, tx, "account = ?", account); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM folders WHERE account = ?", account); err != nil {
			return fmt.Errorf("deleting progress: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM ref_recovery WHERE account = ?", account); err != nil {
			return fmt.Errorf("deleting recovery rows: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM unreadable WHERE account = ?", account); err != nil {
			return fmt.Errorf("deleting unreadable records: %w", err)
		}
		return nil
	})
}
