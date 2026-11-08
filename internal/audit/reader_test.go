package audit

import (
	"database/sql"
	"slices"
	"testing"
	"time"
)

var readerBase = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// seedReader writes five rows across two accounts and three times, in id
// order 1 to 5, and returns a Reader over them.
func seedReader(t *testing.T) *Reader {
	t.Helper()
	conn := openDB(t)
	w := NewWriter(conn, nil)
	entries := []Entry{
		{At: readerBase, CallerUID: 501, Client: "cli", Account: "work", Action: "mail.send", Targets: []string{"a"}, Decision: "allowed", Result: ResultOK},
		{At: readerBase.Add(time.Hour), Client: "mcp", Account: "home", Action: "mail.read", Result: ResultOK},
		{At: readerBase.Add(time.Hour), Client: "cli", Account: "work", Action: "mail.read", Result: ResultError, Reason: "NOT_FOUND"},
		{At: readerBase.Add(2 * time.Hour), Client: "cli", Account: "home", Action: "mail.send", Targets: []string{"x", "y"}, Decision: "held", Result: ResultOK},
		{At: readerBase.Add(2 * time.Hour), Client: "cli", Account: "work", Action: "mail.send", Decision: "denied", Result: ResultError, Reason: "DENIED"},
	}
	for _, e := range entries {
		if err := w.Record(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	return NewReader(conn)
}

func ids(rows []Row) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func TestReadFilters(t *testing.T) {
	t.Parallel()
	r := seedReader(t)
	tests := []struct {
		name string
		q    Query
		want []int64
	}{
		{name: "all newest first", q: Query{Since: readerBase, Limit: 10}, want: []int64{5, 4, 3, 2, 1}},
		{name: "since excludes older", q: Query{Since: readerBase.Add(time.Hour), Limit: 10}, want: []int64{5, 4, 3, 2}},
		{name: "since between rows", q: Query{Since: readerBase.Add(90 * time.Minute), Limit: 10}, want: []int64{5, 4}},
		{name: "since after all", q: Query{Since: readerBase.Add(3 * time.Hour), Limit: 10}, want: []int64{}},
		{name: "account", q: Query{Since: readerBase, Account: "home", Limit: 10}, want: []int64{4, 2}},
		{name: "action", q: Query{Since: readerBase, Action: "mail.read", Limit: 10}, want: []int64{3, 2}},
		{name: "account and action", q: Query{Since: readerBase, Account: "work", Action: "mail.send", Limit: 10}, want: []int64{5, 1}},
		{name: "before id", q: Query{Since: readerBase, BeforeID: 3, Limit: 10}, want: []int64{2, 1}},
		{name: "since in another zone", q: Query{Since: readerBase.Add(90 * time.Minute).In(time.FixedZone("x", -5*60*60)), Limit: 10}, want: []int64{5, 4}},
		{name: "client", q: Query{Since: readerBase, Client: "mcp", Limit: 10}, want: []int64{2}},
		{name: "client with no rows", q: Query{Since: readerBase, Client: "rule", Limit: 10}, want: []int64{}},
		{name: "client and account", q: Query{Since: readerBase, Client: "cli", Account: "home", Limit: 10}, want: []int64{4}},
		{name: "client and action", q: Query{Since: readerBase, Client: "cli", Action: "mail.send", Limit: 10}, want: []int64{5, 4, 1}},
		{name: "client and before id", q: Query{Since: readerBase, Client: "cli", BeforeID: 4, Limit: 10}, want: []int64{3, 1}},
		{
			name: "client with every other filter",
			q:    Query{Since: readerBase.Add(time.Hour), Client: "cli", Account: "work", Action: "mail.read", Limit: 10},
			want: []int64{3},
		},
		{name: "empty client is every client", q: Query{Since: readerBase, Client: "", Limit: 10}, want: []int64{5, 4, 3, 2, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rows, next, err := r.Read(t.Context(), tt.q)
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(rows); !slices.Equal(got, tt.want) {
				t.Errorf("ids = %v, want %v", got, tt.want)
			}
			if next != 0 {
				t.Errorf("nextBeforeID = %d, want 0", next)
			}
		})
	}
}

func TestReadDecodesRow(t *testing.T) {
	t.Parallel()
	r := seedReader(t)
	rows, _, err := r.Read(t.Context(), Query{Since: readerBase, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	want := Row{
		ID: 4, At: readerBase.Add(2 * time.Hour), Client: "cli", Account: "home", Action: "mail.send",
		Targets: []string{"x", "y"}, Decision: "held", Result: ResultOK,
	}
	got := rows[1]
	if got.ID != want.ID || !got.At.Equal(want.At) || got.Client != want.Client || got.Account != want.Account ||
		got.Action != want.Action || !slices.Equal(got.Targets, want.Targets) || got.Decision != want.Decision ||
		got.Result != want.Result || got.Reason != want.Reason {
		t.Errorf("row = %+v, want %+v", got, want)
	}
	first := rows[4]
	if first.CallerUID != 501 || !slices.Equal(first.Targets, []string{"a"}) {
		t.Errorf("row 1 = %+v", first)
	}
	last := rows[0]
	if last.Result != ResultError || last.Reason != "DENIED" || last.Targets == nil || len(last.Targets) != 0 {
		t.Errorf("row 5 = %+v, want an error row with empty targets", last)
	}
}

func TestReadPages(t *testing.T) {
	t.Parallel()
	r := seedReader(t)
	rows, next, err := r.Read(t.Context(), Query{Since: readerBase, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(rows); !slices.Equal(got, []int64{5, 4}) || next != 4 {
		t.Fatalf("first page = %v next %d, want [5 4] next 4", got, next)
	}
	var seen []int64
	q := Query{Since: readerBase, Limit: 2}
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging did not end")
		}
		rows, next, err := r.Read(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, ids(rows)...)
		if next == 0 {
			break
		}
		q.BeforeID = next
	}
	if !slices.Equal(seen, []int64{5, 4, 3, 2, 1}) {
		t.Errorf("paged ids = %v, want each row once, newest first", seen)
	}
}

// A page that ends exactly on the last matching row reports no next page.
func TestReadExactPageHasNoNext(t *testing.T) {
	t.Parallel()
	r := seedReader(t)
	rows, next, err := r.Read(t.Context(), Query{Since: readerBase, Account: "home", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(rows); !slices.Equal(got, []int64{4, 2}) || next != 0 {
		t.Errorf("page = %v next %d, want [4 2] next 0", got, next)
	}
}

func TestReadRejectsBadTargets(t *testing.T) {
	t.Parallel()
	conn := openDB(t)
	insertRaw(t, conn, readerBase.Format(timeLayout), "not json")
	if _, _, err := NewReader(conn).Read(t.Context(), Query{Since: readerBase, Limit: 10}); err == nil {
		t.Error("a row with undecodable targets was accepted")
	}
}

func insertRaw(t *testing.T, conn *sql.DB, at, targets string) {
	t.Helper()
	_, err := conn.ExecContext(t.Context(),
		`INSERT INTO audit (at, caller_uid, client, action, targets, result) VALUES (?, 0, 'cli', 'x', ?, 'ok')`, at, targets)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadRequiresLimit(t *testing.T) {
	t.Parallel()
	if _, _, err := NewReader(openDB(t)).Read(t.Context(), Query{Since: readerBase}); err == nil {
		t.Error("a zero limit was accepted")
	}
}

// Paging under a client filter visits each of that client's rows once,
// and a page that ends on the client's last row reports no next page.
func TestReadPagesByClient(t *testing.T) {
	t.Parallel()
	r := seedReader(t)
	q := Query{Since: readerBase, Client: "cli", Limit: 2}
	rows, next, err := r.Read(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(rows); !slices.Equal(got, []int64{5, 4}) || next != 4 {
		t.Fatalf("first page = %v next %d, want [5 4] next 4", got, next)
	}
	q.BeforeID = next
	rows, next, err = r.Read(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(rows); !slices.Equal(got, []int64{3, 1}) || next != 0 {
		t.Errorf("second page = %v next %d, want [3 1] next 0, skipping the mcp row 2", got, next)
	}
	for _, row := range rows {
		if row.Client != "cli" {
			t.Errorf("row %d has client %q, want cli", row.ID, row.Client)
		}
	}
}
