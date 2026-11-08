package rules

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/watch"
)

// Refusals of what the rule tables cannot hold or read back. The daemon
// validates a definition fully with Definition.Validate before it writes,
// and these are the store's own floor for callers that skip that. Their
// text never holds rule text.
var (
	errStoreName      = &InvalidError{Msg: "the rule name is not valid", Hint: "use 1 to 63 lowercase letters, digits and hyphens, starting with a letter or digit"}
	errStoreActions   = &InvalidError{Msg: "a rule needs 1 to 16 actions", Hint: "pass at least one action and at most 16"}
	errStoreAction    = &InvalidError{Msg: "an action cannot be stored", Hint: "check each action's kind and arguments"}
	errStoreSchedule  = &InvalidError{Msg: "the schedule is not valid", Hint: "use every:<duration> from 15m to 30d, or at:HH:MM"}
	errStorePriority  = &InvalidError{Msg: "priority must be -1000000 to 1000000", Hint: "lower runs first, and the default is 100"}
	errStoreMaxPerRun = &InvalidError{Msg: "max_per_run must be 1 to 5000", Hint: "pass 0 for the default of 500"}
	errNameTaken      = &InvalidError{Msg: "a live rule already has that name", Hint: "choose another name, or rename or delete the other rule first"}
)

// errStoredRule is the fixed error for a stored rule, revision or time that
// does not decode. It never carries the stored text.
var errStoredRule = errors.New("rules: malformed stored rule")

// StoreOptions configures a Store.
type StoreOptions struct {
	// DB is state.db with migration 0006 applied.
	DB *sql.DB
	// Log is the daemon's one shared event log. Rule events are appended to it.
	Log *watch.Log
	// Now stamps changes, runs and act-once rows. Nil means time.Now.
	Now func() time.Time
	// Location is the wall clock at schedules run on. Nil means time.Local.
	Location *time.Location
	// Rand feeds rule IDs. Nil means crypto/rand. Creates may run at once,
	// so it must be safe for concurrent use.
	Rand io.Reader
}

// Store keeps rules, their revisions, their runs and the act-once table in
// state.db. Every change to a rule runs in one transaction that bumps the
// rule's own revision, writes one rule_revisions row holding the rule as
// JSON, and appends one rule event, so the log never shows a change that
// did not commit, nor misses one that did.
//
// state.db has one connection, so a Store method does all its work through
// one transaction and never calls another public method while it holds it.
// A Store is safe for concurrent use: the connection serializes callers.
type Store struct {
	db  *sql.DB
	log *watch.Log
	now func() time.Time
	loc *time.Location
	rnd io.Reader
}

// NewStore returns a Store over o.DB that appends rule events to o.Log.
func NewStore(o StoreOptions) *Store {
	s := &Store{db: o.DB, log: o.Log, now: o.Now, loc: o.Location, rnd: o.Rand}
	if s.now == nil {
		s.now = time.Now
	}
	if s.loc == nil {
		s.loc = time.Local
	}
	return s
}

// Revision is one stored change of a rule: the rule as it was right after
// the change, the change kind, who made it and when.
type Revision struct {
	Revision int64
	Change   string
	Rule     Rule
	Actor    string
	At       time.Time
}

// queryer is what *sql.DB and *sql.Tx share for reads.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// scanner is a *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// write runs fn in one transaction. fn may use only tx. It returns the rule
// events its change causes, or none for a write that changes no rule. The
// events are appended before the commit and the log is notified after it.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) ([]watch.Event, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rules: beginning a write: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report
	events, err := fn(tx)
	if err != nil {
		return err
	}
	if len(events) > 0 {
		if err := s.log.Append(ctx, tx, events); err != nil {
			return fmt.Errorf("rules: appending rule events: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rules: committing a write: %w", err)
	}
	if len(events) > 0 {
		s.log.Notify()
	}
	return nil
}

// read runs fn in one transaction, so everything fn reads is one view.
func (s *Store) read(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rules: beginning a read: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only; nothing to keep
	return fn(tx)
}

// ruleColumns are the rules columns, in the order scanRule reads them and
// ruleValues writes them.
const ruleColumns = "id, name, description, accounts, query, on_new, schedule, priority, stop, actions, " +
	"max_per_run, enabled, disabled_reason, revision, created_at, updated_at, deleted_at, created_by, " +
	"updated_by, next_run_at, last_run_at, failed_runs"

// ruleUpdate rewrites every column but id, in ruleColumns order, then the id.
const ruleUpdate = "UPDATE rules SET name = ?, description = ?, accounts = ?, query = ?, on_new = ?, " +
	"schedule = ?, priority = ?, stop = ?, actions = ?, max_per_run = ?, enabled = ?, disabled_reason = ?, " +
	"revision = ?, created_at = ?, updated_at = ?, deleted_at = ?, created_by = ?, updated_by = ?, " +
	"next_run_at = ?, last_run_at = ?, failed_runs = ? WHERE id = ?"

// storedRule is a rule as rule_revisions keeps it in JSON, and the shape
// scanRule fills from a row. Times are FormatTime text, "" for none.
type storedRule struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Accounts       []string `json:"accounts"`
	Query          string   `json:"query"`
	OnNew          bool     `json:"on_new"`
	Schedule       string   `json:"schedule"`
	Priority       int      `json:"priority"`
	Stop           bool     `json:"stop"`
	Actions        []Action `json:"actions"`
	MaxPerRun      int      `json:"max_per_run"`
	Enabled        bool     `json:"enabled"`
	DisabledReason string   `json:"disabled_reason"`
	Revision       int64    `json:"revision"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	DeletedAt      string   `json:"deleted_at"`
	CreatedBy      string   `json:"created_by"`
	UpdatedBy      string   `json:"updated_by"`
	NextRunAt      string   `json:"next_run_at"`
	LastRunAt      string   `json:"last_run_at"`
	FailedRuns     int      `json:"failed_runs"`
}

// toStored renders r for rule_revisions.
func toStored(r Rule) storedRule {
	return storedRule{
		ID: r.ID, Name: r.Name, Description: r.Description, Accounts: nonNilStrings(r.Accounts),
		Query: r.Query, OnNew: r.OnNew, Schedule: r.Schedule.String(), Priority: r.Priority, Stop: r.Stop,
		Actions: r.Actions, MaxPerRun: r.MaxPerRun, Enabled: r.Enabled, DisabledReason: r.DisabledReason,
		Revision: r.Revision, CreatedAt: dbTime(r.CreatedAt), UpdatedAt: dbTime(r.UpdatedAt),
		DeletedAt: dbTime(r.DeletedAt), CreatedBy: r.CreatedBy, UpdatedBy: r.UpdatedBy,
		NextRunAt: dbTime(r.NextRunAt), LastRunAt: dbTime(r.LastRunAt), FailedRuns: r.FailedRuns,
	}
}

// fromStored reads a rule back. Empty accounts and actions read as nil, the
// form canonical gives.
func fromStored(sr storedRule) (Rule, error) {
	sched, err := ParseSchedule(sr.Schedule)
	if err != nil {
		return Rule{}, errStoredRule
	}
	r := Rule{
		ID: sr.ID,
		Definition: Definition{
			Name: sr.Name, Description: sr.Description, Accounts: sr.Accounts, Query: sr.Query,
			OnNew: sr.OnNew, Schedule: sched, Priority: sr.Priority, Stop: sr.Stop, Actions: sr.Actions,
			MaxPerRun: sr.MaxPerRun, Enabled: sr.Enabled,
		},
		DisabledReason: sr.DisabledReason, Revision: sr.Revision,
		CreatedBy: sr.CreatedBy, UpdatedBy: sr.UpdatedBy, FailedRuns: sr.FailedRuns,
	}
	if len(r.Accounts) == 0 {
		r.Accounts = nil
	}
	if len(r.Actions) == 0 {
		r.Actions = nil
	}
	times := []struct {
		dst *time.Time
		src string
	}{
		{&r.CreatedAt, sr.CreatedAt},
		{&r.UpdatedAt, sr.UpdatedAt},
		{&r.DeletedAt, sr.DeletedAt},
		{&r.NextRunAt, sr.NextRunAt},
		{&r.LastRunAt, sr.LastRunAt},
	}
	for _, tm := range times {
		if *tm.dst, err = parseDBTime(tm.src); err != nil {
			return Rule{}, err
		}
	}
	return r, nil
}

// decodeRevision reads the rule a rule_revisions row holds.
func decodeRevision(body string) (Rule, error) {
	var sr storedRule
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		return Rule{}, errStoredRule
	}
	return fromStored(sr)
}

// scanRule reads one row of ruleColumns.
func scanRule(sc scanner) (Rule, error) {
	var (
		sr                        storedRule
		accounts, actions         string
		onNew, stop, enabled      int
		deleted, nextRun, lastRun sql.NullString
	)
	if err := sc.Scan(&sr.ID, &sr.Name, &sr.Description, &accounts, &sr.Query, &onNew, &sr.Schedule,
		&sr.Priority, &stop, &actions, &sr.MaxPerRun, &enabled, &sr.DisabledReason, &sr.Revision,
		&sr.CreatedAt, &sr.UpdatedAt, &deleted, &sr.CreatedBy, &sr.UpdatedBy, &nextRun, &lastRun,
		&sr.FailedRuns); err != nil {
		return Rule{}, fmt.Errorf("rules: reading a rule: %w", err)
	}
	if err := json.Unmarshal([]byte(accounts), &sr.Accounts); err != nil {
		return Rule{}, errStoredRule
	}
	as, err := DecodeActions(actions)
	if err != nil {
		return Rule{}, errStoredRule
	}
	sr.Actions = as
	sr.OnNew, sr.Stop, sr.Enabled = onNew != 0, stop != 0, enabled != 0
	sr.DeletedAt, sr.NextRunAt, sr.LastRunAt = deleted.String, nextRun.String, lastRun.String
	return fromStored(sr)
}

// ruleValues returns r's column values in ruleColumns order.
func ruleValues(r Rule) ([]any, error) {
	accounts, err := json.Marshal(nonNilStrings(r.Accounts))
	if err != nil {
		return nil, fmt.Errorf("rules: encoding accounts: %w", err)
	}
	actions, err := EncodeActions(r.Actions)
	if err != nil {
		return nil, fmt.Errorf("rules: encoding actions: %w", err)
	}
	return []any{
		r.ID, r.Name, r.Description, string(accounts), r.Query, sqlBool(r.OnNew), r.Schedule.String(),
		r.Priority, sqlBool(r.Stop), actions, r.MaxPerRun, sqlBool(r.Enabled), r.DisabledReason, r.Revision,
		dbTime(r.CreatedAt), dbTime(r.UpdatedAt), nullTime(r.DeletedAt), r.CreatedBy, r.UpdatedBy,
		nullTime(r.NextRunAt), nullTime(r.LastRunAt), r.FailedRuns,
	}, nil
}

// insertRule inserts r in tx.
func insertRule(ctx context.Context, tx *sql.Tx, r Rule) error {
	vals, err := ruleValues(r)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO rules ("+ruleColumns+") VALUES ("+placeholders(len(vals))+")", vals...); err != nil {
		return fmt.Errorf("rules: inserting a rule: %w", err)
	}
	return nil
}

// saveRule rewrites the row of r.ID with r in tx.
func saveRule(ctx context.Context, tx *sql.Tx, r Rule) error {
	vals, err := ruleValues(r)
	if err != nil {
		return err
	}
	args := make([]any, 0, len(vals))
	args = append(args, vals[1:]...)
	args = append(args, vals[0])
	if _, err := tx.ExecContext(ctx, ruleUpdate, args...); err != nil {
		return fmt.Errorf("rules: saving a rule: %w", err)
	}
	return nil
}

// findRule looks rule up through q: by ID when it passes ValidID, else by
// live name. Not found is ErrNotFound.
func findRule(ctx context.Context, q queryer, rule string) (Rule, error) {
	if ValidID(rule) {
		return findRuleByID(ctx, q, rule)
	}
	return scanFound(q.QueryRowContext(ctx,
		"SELECT "+ruleColumns+" FROM rules WHERE name = ? AND deleted_at IS NULL", rule))
}

// findRuleByID reads the rule with id, live or deleted, through q.
func findRuleByID(ctx context.Context, q queryer, id string) (Rule, error) {
	return scanFound(q.QueryRowContext(ctx, "SELECT "+ruleColumns+" FROM rules WHERE id = ?", id))
}

// scanFound scans one rule, turning no row into ErrNotFound.
func scanFound(row *sql.Row) (Rule, error) {
	r, err := scanRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	if err != nil {
		return Rule{}, err
	}
	return r, nil
}

// nameTaken reports whether a live rule other than exceptID has name.
func nameTaken(ctx context.Context, q queryer, name, exceptID string) (bool, error) {
	var taken int
	if err := q.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM rules WHERE name = ? AND deleted_at IS NULL AND id <> ?)",
		name, exceptID).Scan(&taken); err != nil {
		return false, fmt.Errorf("rules: checking a rule name: %w", err)
	}
	return taken != 0, nil
}

// recordChange reads rule id as stored in tx, writes its rule_revisions row
// for change, and returns the rule with its one rule event. The event holds
// the ID, name, change, revision, actor and reason, never rule text.
func recordChange(ctx context.Context, tx *sql.Tx, id, change, actor, reason string, at time.Time) (Rule, []watch.Event, error) {
	r, err := findRuleByID(ctx, tx, id)
	if err != nil {
		return Rule{}, nil, err
	}
	body, err := json.Marshal(toStored(r))
	if err != nil {
		return Rule{}, nil, fmt.Errorf("rules: encoding a revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO rule_revisions (rule_id, revision, change, rule, actor, at) VALUES (?, ?, ?, ?, ?, ?)",
		r.ID, r.Revision, change, string(body), actor, dbTime(at)); err != nil {
		return Rule{}, nil, fmt.Errorf("rules: recording a revision: %w", err)
	}
	e := watch.Event{
		At:   at,
		Kind: watch.KindRule,
		Rule: &watch.RuleChange{RuleID: r.ID, Name: r.Name, Change: change, Revision: r.Revision, Actor: actor, Reason: reason},
	}
	return r, []watch.Event{e}, nil
}

// commitChange saves r, which already carries its new revision, then
// records the change as recordChange does.
func commitChange(ctx context.Context, tx *sql.Tx, r Rule, change, actor, reason string, at time.Time) (Rule, []watch.Event, error) {
	if err := saveRule(ctx, tx, r); err != nil {
		return Rule{}, nil, err
	}
	return recordChange(ctx, tx, r.ID, change, actor, reason, at)
}

// canonical is d normalized, on copies of its slices, with empty accounts
// and actions as nil: the form a stored rule reads back as.
func canonical(d Definition) Definition {
	d.Accounts = slices.Clone(d.Accounts)
	d.Actions = cloneActions(d.Actions)
	d = d.Normalize()
	if len(d.Accounts) == 0 {
		d.Accounts = nil
	}
	if len(d.Actions) == 0 {
		d.Actions = nil
	}
	return d
}

// cloneActions copies as and each forward's address list.
func cloneActions(as []Action) []Action {
	if as == nil {
		return nil
	}
	out := make([]Action, len(as))
	for i, a := range as {
		a.To = slices.Clone(a.To)
		out[i] = a
	}
	return out
}

// checkStored refuses what the rules table cannot hold or read back.
func checkStored(d Definition) error {
	switch {
	case !ValidName(d.Name):
		return errStoreName
	case len(d.Actions) == 0 || len(d.Actions) > MaxActions:
		return errStoreActions
	case d.Priority < MinPriority || d.Priority > MaxPriority:
		return errStorePriority
	case d.MaxPerRun < 1 || d.MaxPerRun > MaxMaxPerRun:
		return errStoreMaxPerRun
	}
	for _, a := range d.Actions {
		if !a.Kind.Valid() {
			return errStoreAction
		}
	}
	// The stored forms must read back as d itself: an encoding that drops
	// a field, such as a label action's folder or an unknown schedule kind
	// rendered as none, would store a rule other than the one asked for.
	enc, err := EncodeActions(d.Actions)
	if err != nil {
		return errStoreAction
	}
	back, err := DecodeActions(enc)
	if err != nil || !slices.EqualFunc(back, d.Actions, sameAction) {
		return errStoreAction
	}
	sched, err := ParseSchedule(d.Schedule.String())
	if err != nil || sched != d.Schedule {
		return errStoreSchedule
	}
	return nil
}

// placeholders is n comma-separated question marks.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}

// sqlBool renders b as the 0 or 1 the integer flag columns hold.
func sqlBool(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nonNilStrings turns nil into an empty list, so JSON holds [] not null.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// dbTime renders t in the watch log's fixed-width UTC layout, "" for zero.
func dbTime(t time.Time) string { return watch.FormatTime(t) }

// nullTime stores the zero time as NULL, which the nullable time columns
// read as none.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return watch.FormatTime(t)
}

// parseDBTime reads a time written by dbTime. "" reads as the zero time.
func parseDBTime(s string) (time.Time, error) {
	t, err := watch.ParseTime(s)
	if err != nil {
		return time.Time{}, errStoredRule
	}
	return t, nil
}

// Get returns a rule by ID, live or deleted, or by live name. Not found is
// ErrNotFound.
func (s *Store) Get(ctx context.Context, rule string) (Rule, error) {
	var out Rule
	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = findRule(ctx, tx, rule)
		return err
	})
	if err != nil {
		return Rule{}, err
	}
	return out, nil
}
