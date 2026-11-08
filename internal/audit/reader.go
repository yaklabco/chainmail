package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Query selects audit rows.
type Query struct {
	// Since is the earliest At returned. The reader requires it; the
	// daemon fills the default.
	Since time.Time
	// Account empty means all accounts.
	Account string
	// Action empty means all actions.
	Action string
	// Client is a client kind, such as cli, mcp, daemon or rule. Empty
	// means every client. The caller validates it.
	Client string
	// Limit is 1 to 1000, validated by the caller.
	Limit int
	// BeforeID returns only rows with a smaller id. 0 means no bound.
	BeforeID int64
}

// Row is one audit row as read back.
type Row struct {
	ID        int64
	At        time.Time
	CallerUID uint32
	Client    string
	Account   string
	Action    string
	Targets   []string
	Decision  string
	Result    Result
	Reason    string
}

// Reader reads the audit table in state.db.
type Reader struct {
	db *sql.DB
}

// NewReader returns a Reader.
func NewReader(db *sql.DB) *Reader {
	return &Reader{db: db}
}

// Read returns up to q.Limit rows newest first and the id to pass as the
// next BeforeID, or 0 when no older row matches.
func (r *Reader) Read(ctx context.Context, q Query) (rows []Row, nextBeforeID int64, err error) {
	if q.Limit < 1 {
		return nil, 0, fmt.Errorf("audit: limit %d is below 1", q.Limit)
	}
	var b strings.Builder
	b.WriteString(`SELECT id, at, caller_uid, client, account, action, targets, decision, result, reason FROM audit WHERE at >= ?`)
	// at is stored as fixed-width UTC text, so it compares correctly as text.
	args := []any{q.Since.UTC().Format(timeLayout)}
	if q.Account != "" {
		b.WriteString(` AND account = ?`)
		args = append(args, q.Account)
	}
	if q.Action != "" {
		b.WriteString(` AND action = ?`)
		args = append(args, q.Action)
	}
	if q.Client != "" {
		b.WriteString(` AND client = ?`)
		args = append(args, q.Client)
	}
	if q.BeforeID != 0 {
		b.WriteString(` AND id < ?`)
		args = append(args, q.BeforeID)
	}
	// One extra row tells whether an older page exists.
	b.WriteString(` ORDER BY id DESC LIMIT ?`)
	args = append(args, q.Limit+1)

	res, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: reading: %w", err)
	}
	defer func() { _ = res.Close() }() // res.Err below reports any iteration error
	rows = make([]Row, 0, min(q.Limit, 100))
	more := false
	for res.Next() {
		if len(rows) == q.Limit {
			more = true
			break
		}
		row, err := scanRow(res)
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, row)
	}
	if err := res.Err(); err != nil {
		return nil, 0, fmt.Errorf("audit: reading: %w", err)
	}
	if more {
		nextBeforeID = rows[len(rows)-1].ID
	}
	return rows, nextBeforeID, nil
}

func scanRow(res *sql.Rows) (Row, error) {
	var (
		row             Row
		at, targets, rs string
	)
	if err := res.Scan(&row.ID, &at, &row.CallerUID, &row.Client, &row.Account, &row.Action,
		&targets, &row.Decision, &rs, &row.Reason); err != nil {
		return Row{}, fmt.Errorf("audit: reading row: %w", err)
	}
	t, err := time.Parse(timeLayout, at)
	if err != nil {
		return Row{}, fmt.Errorf("audit: row %d: parsing at: %w", row.ID, err)
	}
	row.At = t
	if err := json.Unmarshal([]byte(targets), &row.Targets); err != nil {
		return Row{}, fmt.Errorf("audit: row %d: decoding targets: %w", row.ID, err)
	}
	if row.Targets == nil {
		row.Targets = []string{}
	}
	row.Result = Result(rs)
	return row, nil
}
