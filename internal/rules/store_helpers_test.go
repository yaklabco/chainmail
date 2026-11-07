package rules

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata" // the at-schedule tests name zones, so hosts without zoneinfo still run them

	"github.com/yaklabco/chainmail/internal/db"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/watch"
)

// storeActor makes every write unless a test names another actor.
var storeActor = items.Actor{UID: 1000, Client: "cli"}

// storeStart is where the rig's clock starts. Its first Now is one
// millisecond later.
var storeStart = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

// Rule text markers. storeDef and fullDef put them in every free-text field
// of a rule, and noRuleText checks that no event payload holds any of them.
const (
	markDescription = "ZQX-RULE-DESCRIPTION"
	markQuery       = "zqxquery"
	markLabel       = "ZQX-LABEL"
	markFolder      = "ZQX-FOLDER"
	markAddress     = "zqx-forward@example.com"
	markNote        = "ZQX-NOTE"
)

// storeClock moves one millisecond forward on every Now, so rules created
// one after another get IDs in creation order. Last reports the latest value
// handed out: the instant the most recent store call read.
type storeClock struct {
	mu   sync.Mutex
	last time.Time
}

func (c *storeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = c.last.Add(time.Millisecond)
	return c.last
}

func (c *storeClock) Last() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// storeRig is a Store over a fresh state.db, with the watch log it appends to.
type storeRig struct {
	store *Store
	db    *sql.DB
	log   *watch.Log
	clock *storeClock
}

// newStoreRig returns a rig whose at schedules run on UTC.
func newStoreRig(t *testing.T) *storeRig {
	t.Helper()
	return newStoreRigWith(t, nil)
}

// newStoreRigWith returns a rig whose StoreOptions edit may change first.
func newStoreRigWith(t *testing.T, edit func(*StoreOptions)) *storeRig {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	clock := &storeClock{last: storeStart}
	log := watch.NewLog(conn, clock.Now, nil)
	o := StoreOptions{DB: conn, Log: log, Now: clock.Now, Location: time.UTC}
	if edit != nil {
		edit(&o)
	}
	return &storeRig{store: NewStore(o), db: conn, log: log, clock: clock}
}

// storeDef is a definition with the defaults, one label action and rule
// text markers in the description, query and label.
func storeDef(name string) Definition {
	d := Defaults()
	d.Name = name
	d.Description = markDescription
	d.Accounts = nil
	d.Query = "from:" + markQuery + "@example.com"
	d.Actions = []Action{{Kind: ActLabel, Label: markLabel}}
	return d
}

// fullDef sets every field of a definition away from its default. Its
// accounts are out of order with a duplicate, which Normalize sorts and
// de-duplicates to gmail, work.
func fullDef(name string) Definition {
	d := storeDef(name)
	d.Accounts = []string{"work", "gmail", "work"}
	d.OnNew = true
	d.Schedule = Schedule{Kind: ScheduleEvery, Every: 6 * time.Hour}
	d.Priority = 7
	d.Stop = true
	d.Actions = []Action{
		{Kind: ActLabel, Label: markLabel},
		{Kind: ActLinkItem, ItemID: "it_01jtestitem", Anchor: true},
		{Kind: ActForward, To: []string{markAddress}, Note: markNote},
		{Kind: ActCopy, Folder: markFolder + "-copy"},
		{Kind: ActMove, Folder: markFolder},
	}
	d.MaxPerRun = 42
	d.Enabled = true
	return d
}

// create creates d as storeActor and fails the test on error.
func (r *storeRig) create(t *testing.T, d Definition) Rule {
	t.Helper()
	rule, err := r.store.Create(t.Context(), storeActor, d)
	if err != nil {
		t.Fatalf("Create %s: %v", d.Name, err)
	}
	return rule
}

// get looks rule up by ID or name and fails the test on error.
func (r *storeRig) get(t *testing.T, rule string) Rule {
	t.Helper()
	got, err := r.store.Get(t.Context(), rule)
	if err != nil {
		t.Fatalf("Get %s: %v", rule, err)
	}
	return got
}

// tail is the event log's tail. ruleChanges(t, tail) later reads what came after.
func (r *storeRig) tail(t *testing.T) int64 {
	t.Helper()
	v, err := r.log.Tail(t.Context())
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	return v
}

// ruleChanges returns the payloads of the rule events after position after.
// Every one must be a rule event with no account, folder or ref.
func (r *storeRig) ruleChanges(t *testing.T, after int64) []watch.RuleChange {
	t.Helper()
	page, err := r.log.Read(t.Context(), watch.Query{
		After:  after,
		Filter: watch.Filter{Kinds: []watch.Kind{watch.KindRule}},
		Limit:  1000,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var out []watch.RuleChange
	for _, e := range page.Events {
		if e.Kind != watch.KindRule || e.Rule == nil || e.Account != "" || e.Folder != "" || e.Ref != "" {
			t.Fatalf("event %d = %+v, want a rule event with no account, folder or ref", e.ID, e)
		}
		out = append(out, *e.Rule)
	}
	return out
}

// wantRuleChanges fails the test unless got is exactly want, in order.
func wantRuleChanges(t *testing.T, got []watch.RuleChange, want ...watch.RuleChange) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rule events =\n%+v\nwant\n%+v", got, want)
	}
}

// noRuleText fails the test when any stored event payload holds a rule
// text marker.
func (r *storeRig) noRuleText(t *testing.T) {
	t.Helper()
	rows, err := r.db.QueryContext(t.Context(), "SELECT payload FROM events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	marks := []string{markDescription, markQuery, markLabel, markFolder, markAddress, markNote}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		for _, mark := range marks {
			if strings.Contains(payload, mark) {
				t.Errorf("event payload %s holds rule text %q", payload, mark)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// lastRevision reads the newest rule_revisions row of id straight from the
// table, decoding its rule.
func (r *storeRig) lastRevision(t *testing.T, id string) Revision {
	t.Helper()
	var (
		rev      Revision
		body, at string
	)
	if err := r.db.QueryRowContext(t.Context(),
		"SELECT revision, change, rule, actor, at FROM rule_revisions WHERE rule_id = ? ORDER BY revision DESC LIMIT 1",
		id).Scan(&rev.Revision, &rev.Change, &body, &rev.Actor, &at); err != nil {
		t.Fatalf("reading the last revision of %s: %v", id, err)
	}
	rule, err := decodeRevision(body)
	if err != nil {
		t.Fatalf("decoding the last revision of %s: %v", id, err)
	}
	rev.Rule = rule
	if rev.At, err = watch.ParseTime(at); err != nil {
		t.Fatal(err)
	}
	return rev
}

// column reads one column of rule id as text, with "NULL" for NULL. col is
// a constant from the test, never input.
func (r *storeRig) column(t *testing.T, id, col string) string {
	t.Helper()
	var v string
	if err := r.db.QueryRowContext(t.Context(),
		"SELECT COALESCE(CAST("+col+" AS TEXT), 'NULL') FROM rules WHERE id = ?", id).Scan(&v); err != nil {
		t.Fatalf("reading %s of %s: %v", col, id, err)
	}
	return v
}

// wantColumns fails the test unless each named column of rule id holds the
// text given.
func (r *storeRig) wantColumns(t *testing.T, id string, want map[string]string) {
	t.Helper()
	for col, v := range want {
		if got := r.column(t, id, col); got != v {
			t.Errorf("rules.%s = %q, want %q", col, got, v)
		}
	}
}

// count runs a SELECT count(*) query and returns the count.
func (r *storeRig) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := r.db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// sameRule compares rules, with times compared by instant.
func sameRule(t *testing.T, got, want Rule) {
	t.Helper()
	times := []struct {
		name      string
		got, want time.Time
	}{
		{"CreatedAt", got.CreatedAt, want.CreatedAt},
		{"UpdatedAt", got.UpdatedAt, want.UpdatedAt},
		{"DeletedAt", got.DeletedAt, want.DeletedAt},
		{"NextRunAt", got.NextRunAt, want.NextRunAt},
		{"LastRunAt", got.LastRunAt, want.LastRunAt},
	}
	for _, tm := range times {
		if !tm.got.Equal(tm.want) {
			t.Errorf("rule %s %s = %v, want %v", want.ID, tm.name, tm.got, tm.want)
		}
	}
	got.CreatedAt, got.UpdatedAt, got.DeletedAt, got.NextRunAt, got.LastRunAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
	want.CreatedAt, want.UpdatedAt, want.DeletedAt, want.NextRunAt, want.LastRunAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rule %s =\n%+v\nwant\n%+v", want.ID, got, want)
	}
}

// wantRefused fails the test unless err is want.
func wantRefused(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// otherActor makes the writes a test tells apart from storeActor's.
var otherActor = items.Actor{UID: 1001, Client: "mcp"}

// storePtr returns a pointer to a copy of v, for Patch fields.
func storePtr[T any](v T) *T { return &v }

// exec runs a statement behind the Store's back.
func (r *storeRig) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := r.db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}
