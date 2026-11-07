package cache

import (
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/query"
)

// matchSources are the folders the match tests search unless a case says
// otherwise.
var matchSources = []mailstore.Source{
	{Folder: "Archive", UIDValidity: testUIDValidity},
	{Folder: "INBOX", UIDValidity: testUIDValidity},
}

// matchOldest is every row of matchSources in OldestFirst order: by
// arrival, then folder, then UID.
var matchOldest = []string{"INBOX/1", "Archive/1", "INBOX/2", "INBOX/3", "Archive/2"}

// matchKey names a row as folder/uid.
func matchKey(r Row) string { return r.Folder + "/" + strconv.FormatUint(uint64(r.UID), 10) }

// keysOf names rows in order.
func keysOf(rows []Row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, matchKey(r))
	}
	return out
}

// seedMatch stores the match fixture and returns its rows by matchKey. In
// testAccount under testUIDValidity:
//
//	INBOX/1    5h ago  from ann    thread t1
//	INBOX/2    3h ago  from ann    thread t2  label \Sent         body zebra
//	INBOX/3    3h ago  from ann    thread t3                      body zebra
//	Archive/1  3h ago  from ann    thread t1  labels \Draft, work
//	Archive/2  1h ago  from carol  thread t2                      body zebra
//	Sent/1     2h ago  from ann    thread t9
//
// and two rows a search of matchSources never returns: INBOX/9 under
// UIDVALIDITY 6, and INBOX/10 of the account other.
func seedMatch(t *testing.T) (*Store, map[string]Row) {
	t.Helper()
	s, _ := openStore(t)
	row := func(folder string, uid uint32, hoursAgo int, thread string, labels ...string) Row {
		r := testRow(folder, uid)
		r.InternalDate = t0.Add(-time.Duration(hoursAgo) * time.Hour)
		r.ThreadID = thread
		r.Labels = labels
		return r
	}
	carol := row("Archive", 2, 1, "t2")
	carol.From = []mailstore.Address{{Name: "Carol", Email: "carol@example.org"}}
	stale := row("INBOX", 9, 4, "t9")
	stale.UIDValidity = 6
	other := row("INBOX", 10, 4, "t9")
	other.Account = "other"
	rows := []Row{
		row("INBOX", 1, 5, "t1"),
		row("INBOX", 2, 3, "t2", `\Sent`),
		row("INBOX", 3, 3, "t3"),
		row("Archive", 1, 3, "t1", `\Draft`, "work"),
		carol,
		row("Sent", 1, 2, "t9"),
		stale,
		other,
	}
	putRows(t, s, rows...)
	byKey := make(map[string]Row, len(rows))
	for _, r := range rows {
		byKey[matchKey(r)] = r
	}
	for _, k := range []string{"INBOX/2", "INBOX/3", "Archive/2"} {
		putBody(t, s, byKey[k].Ref(), rawMessage("Report", "the zebra crossed"), false)
	}
	return s, byKey
}

// matchKeys runs MatchRows and names the rows it returns.
func matchKeys(t *testing.T, s *Store, account string, w Where, o MatchOptions) []string {
	t.Helper()
	rows, err := s.MatchRows(t.Context(), account, w, o)
	if err != nil {
		t.Fatalf("MatchRows(%+v, %+v): %v", w, o, err)
	}
	return keysOf(rows)
}

// compileMatch parses q, splits off in: and account: as the daemon does,
// and compiles the rest for a standard account at t0.
func compileMatch(t *testing.T, q string, items map[string][]string) Where {
	t.Helper()
	n, err := query.Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	scope, rest := query.Split(n)
	p, err := Compile(rest, scope.In, false, t0, items)
	if err != nil {
		t.Fatalf("Compile(%q): %v", q, err)
	}
	return p.Where
}

func newestFirst() []string {
	out := slices.Clone(matchOldest)
	slices.Reverse(out)
	return out
}

// An empty Where matches every row of the sources, in either order, with
// every column read back.
func TestMatchRowsOrder(t *testing.T) {
	t.Parallel()
	s, byKey := seedMatch(t)
	tests := []struct {
		name  string
		order MatchOrder
		want  []string
	}{
		{name: "oldest first", order: OldestFirst, want: matchOldest},
		{name: "newest first", order: NewestFirst, want: newestFirst()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := s.MatchRows(t.Context(), testAccount, Where{}, MatchOptions{Sources: matchSources, Order: tt.order, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			if got := keysOf(rows); !slices.Equal(got, tt.want) {
				t.Fatalf("rows = %v, want %v", got, tt.want)
			}
			for _, r := range rows {
				if want := byKey[matchKey(r)]; !reflect.DeepEqual(r, want) {
					t.Errorf("row %s =\n%+v\nwant\n%+v", matchKey(r), r, want)
				}
			}
		})
	}
}

// Paging with the cursor of each page's last row visits every row once,
// in order, across folders and equal arrival times.
func TestMatchRowsPages(t *testing.T) {
	t.Parallel()
	s, _ := seedMatch(t)
	tests := []struct {
		name  string
		order MatchOrder
		want  []string
	}{
		{name: "oldest first", order: OldestFirst, want: matchOldest},
		{name: "newest first", order: NewestFirst, want: newestFirst()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				got   []string
				after MatchCursor
				pages int
			)
			for ; pages < 10; pages++ {
				rows, err := s.MatchRows(t.Context(), testAccount, Where{}, MatchOptions{
					Sources: matchSources, Order: tt.order, After: after, Limit: 2,
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) > 2 {
					t.Fatalf("page %d holds %d rows, want at most 2", pages, len(rows))
				}
				if len(rows) == 0 {
					break
				}
				got = append(got, keysOf(rows)...)
				after = CursorOf(rows[len(rows)-1])
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("paged rows = %v, want %v", got, tt.want)
			}
			if pages != 3 {
				t.Errorf("read %d non-empty pages, want 3", pages)
			}
		})
	}
}

// The cursor is zero only when every field is, and CursorOf takes a row's
// arrival, folder and UID.
func TestMatchCursor(t *testing.T) {
	t.Parallel()
	if !(MatchCursor{}).IsZero() {
		t.Error("the zero cursor is not zero")
	}
	for _, c := range []MatchCursor{{InternalDate: t0}, {Folder: "INBOX"}, {UID: 1}} {
		if c.IsZero() {
			t.Errorf("%+v reports zero", c)
		}
	}
	r := testRow("INBOX", 4)
	if got, want := CursorOf(r), (MatchCursor{InternalDate: r.InternalDate, Folder: "INBOX", UID: 4}); got != want {
		t.Errorf("CursorOf = %+v, want %+v", got, want)
	}
}

// Sources pin the account, each folder and its UIDVALIDITY, and UIDs
// restricts each folder to the UIDs listed for it.
func TestMatchRowsScope(t *testing.T) {
	t.Parallel()
	s, _ := seedMatch(t)
	inbox := []mailstore.Source{{Folder: "INBOX", UIDValidity: testUIDValidity}}
	tests := []struct {
		name    string
		account string
		sources []mailstore.Source
		uids    map[string][]uint32
		want    []string
	}{
		{name: "no sources", account: testAccount, sources: nil, want: nil},
		{name: "one folder", account: testAccount, sources: inbox, want: []string{"INBOX/1", "INBOX/2", "INBOX/3"}},
		{name: "a folder outside the default", account: testAccount, sources: []mailstore.Source{{Folder: "Sent", UIDValidity: testUIDValidity}}, want: []string{"Sent/1"}},
		{name: "stale UIDVALIDITY", account: testAccount, sources: []mailstore.Source{{Folder: "INBOX", UIDValidity: 99}}, want: nil},
		{name: "older UIDVALIDITY", account: testAccount, sources: []mailstore.Source{{Folder: "INBOX", UIDValidity: 6}}, want: []string{"INBOX/9"}},
		{name: "other account", account: "other", sources: inbox, want: []string{"INBOX/10"}},
		{name: "UIDs of one folder", account: testAccount, sources: matchSources, uids: map[string][]uint32{"INBOX": {1, 3}}, want: []string{"INBOX/1", "INBOX/3"}},
		{name: "UIDs of both folders", account: testAccount, sources: matchSources, uids: map[string][]uint32{"INBOX": {3, 99}, "Archive": {2}}, want: []string{"INBOX/3", "Archive/2"}},
		{name: "empty UID map", account: testAccount, sources: matchSources, uids: map[string][]uint32{}, want: nil},
		{name: "empty UID list", account: testAccount, sources: matchSources, uids: map[string][]uint32{"INBOX": {}}, want: nil},
		{name: "UIDs of a folder outside the sources", account: testAccount, sources: matchSources, uids: map[string][]uint32{"Sent": {1}}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchKeys(t, s, tt.account, Where{}, MatchOptions{Sources: tt.sources, UIDs: tt.uids, Limit: 100})
			if !slices.Equal(got, tt.want) {
				t.Errorf("rows = %v, want %v", got, tt.want)
			}
		})
	}
}

// Compiled plans match only their rows, with their arguments bound in
// placeholder order beside the UID restriction, the FTS term and the
// cursor.
func TestMatchRowsCompiledPlans(t *testing.T) {
	t.Parallel()
	s, byKey := seedMatch(t)
	items := map[string][]string{"it_abc": {"t2"}}
	tests := []struct {
		name  string
		q     string
		uids  map[string][]uint32
		after string // matchKey of the cursor row, "" for none
		want  []string
	}{
		{name: "from", q: "from:carol", want: []string{"Archive/2"}},
		{name: "item", q: "item:it_abc", want: []string{"INBOX/2", "Archive/2"}},
		{name: "text", q: "zebra", want: []string{"INBOX/2", "INBOX/3", "Archive/2"}},
		{name: "from, item and text", q: "from:ann item:it_abc zebra", want: []string{"INBOX/2"}},
		{name: "text in a batch", q: "zebra", uids: map[string][]uint32{"INBOX": {3}, "Archive": {1, 2}}, want: []string{"INBOX/3", "Archive/2"}},
		{name: "every argument", q: "from:ann zebra", uids: map[string][]uint32{"INBOX": {2, 3}, "Archive": {1, 2}}, after: "INBOX/2", want: []string{"INBOX/3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := MatchOptions{Sources: matchSources, UIDs: tt.uids, Limit: 100}
			if tt.after != "" {
				o.After = CursorOf(byKey[tt.after])
			}
			got := matchKeys(t, s, testAccount, compileMatch(t, tt.q, items), o)
			if !slices.Equal(got, tt.want) {
				t.Errorf("%q: rows = %v, want %v", tt.q, got, tt.want)
			}
		})
	}
}

// Limit bounds the rows returned, and a limit outside 1 to 1000, an
// unknown order or a malformed MATCH expression is an error.
func TestMatchRowsLimit(t *testing.T) {
	t.Parallel()
	s, _ := seedMatch(t)
	for _, tt := range []struct {
		limit int
		want  []string
	}{
		{limit: 1, want: matchOldest[:1]},
		{limit: 3, want: matchOldest[:3]},
		{limit: 1000, want: matchOldest},
	} {
		got := matchKeys(t, s, testAccount, Where{}, MatchOptions{Sources: matchSources, Limit: tt.limit})
		if !slices.Equal(got, tt.want) {
			t.Errorf("limit %d: rows = %v, want %v", tt.limit, got, tt.want)
		}
	}
	refused := []struct {
		name string
		w    Where
		o    MatchOptions
	}{
		{name: "limit 0", o: MatchOptions{Sources: matchSources, Limit: 0}},
		{name: "negative limit", o: MatchOptions{Sources: matchSources, Limit: -1}},
		{name: "limit 1001", o: MatchOptions{Sources: matchSources, Limit: 1001}},
		{name: "unknown order", o: MatchOptions{Sources: matchSources, Order: MatchOrder(7), Limit: 10}},
		{name: "limit 0 without sources", o: MatchOptions{}},
		{name: "malformed MATCH", w: Where{FTS: "AND AND"}, o: MatchOptions{Sources: matchSources, Limit: 10}},
	}
	for _, tt := range refused {
		if rows, err := s.MatchRows(t.Context(), testAccount, tt.w, tt.o); err == nil {
			t.Errorf("%s: MatchRows = %v, nil; want an error", tt.name, keysOf(rows))
		}
	}
}

// SourcesOf pairs folders with their progress UIDVALIDITY, in the order
// asked, once each, leaving out folders without progress.
func TestSourcesOf(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	for _, p := range []Progress{
		{Account: testAccount, Folder: "INBOX", UIDValidity: testUIDValidity},
		{Account: testAccount, Folder: "Sent", UIDValidity: 9},
		{Account: "other", Folder: "Drafts", UIDValidity: 3},
	} {
		if err := s.PutProgress(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SourcesOf(t.Context(), testAccount, []string{"Sent", "Drafts", "INBOX", "Sent"})
	if err != nil {
		t.Fatal(err)
	}
	want := []mailstore.Source{{Folder: "Sent", UIDValidity: 9}, {Folder: "INBOX", UIDValidity: testUIDValidity}}
	if !slices.Equal(got, want) {
		t.Errorf("SourcesOf = %+v, want %+v", got, want)
	}
	none, err := s.SourcesOf(t.Context(), testAccount, nil)
	if err != nil || len(none) != 0 {
		t.Errorf("SourcesOf(nil) = %+v, %v; want none", none, err)
	}
}

// ExcludeLabels skips rows holding any of the labels, compared exactly as
// the server reported them, and binds its arguments in placeholder order
// with every other option.
func TestMatchRowsExcludeLabels(t *testing.T) {
	t.Parallel()
	s, byKey := seedMatch(t)
	tests := []struct {
		name   string
		labels []string
		want   []string
	}{
		{name: "none", labels: nil, want: matchOldest},
		{name: "sent", labels: []string{`\Sent`}, want: []string{"INBOX/1", "Archive/1", "INBOX/3", "Archive/2"}},
		{name: "sent and draft", labels: []string{`\Sent`, `\Draft`}, want: []string{"INBOX/1", "INBOX/3", "Archive/2"}},
		{name: "a user label", labels: []string{"work"}, want: []string{"INBOX/1", "INBOX/2", "INBOX/3", "Archive/2"}},
		{name: "compared exactly", labels: []string{`\sent`}, want: matchOldest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchKeys(t, s, testAccount, Where{}, MatchOptions{Sources: matchSources, ExcludeLabels: tt.labels, Limit: 100})
			if !slices.Equal(got, tt.want) {
				t.Errorf("rows = %v, want %v", got, tt.want)
			}
		})
	}

	w := compileMatch(t, "from:ann zebra", nil)
	uids := map[string][]uint32{"INBOX": {2, 3}, "Archive": {1, 2}}
	everything := []struct {
		name  string
		order MatchOrder
		after string
	}{
		{name: "oldest first", order: OldestFirst, after: "Archive/1"},
		{name: "newest first", order: NewestFirst, after: "Archive/2"},
	}
	for _, tt := range everything {
		t.Run("every argument "+tt.name, func(t *testing.T) {
			got := matchKeys(t, s, testAccount, w, MatchOptions{
				Sources: matchSources, UIDs: uids, After: CursorOf(byKey[tt.after]), Order: tt.order,
				Limit: 100, ExcludeLabels: []string{`\Sent`},
			})
			if want := []string{"INBOX/3"}; !slices.Equal(got, want) {
				t.Errorf("rows = %v, want %v", got, want)
			}
		})
	}
}
