package daemon

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
)

// stateOf is a source with matches uids, fetched as a page with limit
// would, of which rows came back.
func stateOf(account string, uids []uint32, limit int, rows ...mailstore.Summary) *sourceState {
	return &sourceState{
		account: account, src: mailstore.Source{Folder: "INBOX", UIDValidity: 1},
		uids: uids, requested: min(len(uids), limit), rows: rows,
	}
}

func uidsOf(rows []mergedRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s%d", r.sum.Ref.Account, r.sum.Ref.UID))
	}
	return out
}

func cursorSources(t *testing.T, s string) []ref.CursorSource {
	t.Helper()
	c, err := ref.DecodeCursor(s)
	if err != nil {
		t.Fatal(err)
	}
	return c.Sources
}

func TestMergeStopsAtASourceWithUnfetchedMatches(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(account string, uid uint32, thread string) mailstore.Summary {
		return summaryAt(account, "INBOX", uid, base.Add(time.Duration(uid)*time.Hour), thread)
	}
	p := &searchPlan{fingerprint: ref.Fingerprint("q")}

	// Page 1: a's fetched rows all collapse into thread x, but 8 and 7,
	// also newer than anything in b, were not fetched.
	a := stateOf("a", []uint32{10, 9, 8, 7}, 2, at("a", 10, "x"), at("a", 9, "x"))
	b := stateOf("b", []uint32{5, 4}, 2, at("b", 5, "y"), at("b", 4, "z"))
	page1 := merge([]*sourceState{a, b}, 2, true)
	if got := uidsOf(page1); !slices.Equal(got, []string{"a10"}) {
		t.Fatalf("page 1 = %v, want a10 alone, as b5 is older than a's unfetched 8", got)
	}
	want := []ref.CursorSource{
		{Account: "a", Folder: "INBOX", UIDValidity: 1, Below: 9},
		{Account: "b", Folder: "INBOX", UIDValidity: 1, Below: 6},
	}
	if got := cursorSources(t, nextCursor(p, []*sourceState{a, b}, nil)); !slices.Equal(got, want) {
		t.Fatalf("page 1 cursor = %+v, want %+v", got, want)
	}

	// Page 2 resumes below 9 in a and 6 in b.
	a = stateOf("a", []uint32{8, 7}, 2, at("a", 8, "x"), at("a", 7, "x"))
	a.prevBelow = 9
	b = stateOf("b", []uint32{5, 4}, 2, at("b", 5, "y"), at("b", 4, "z"))
	b.prevBelow = 6
	page2 := merge([]*sourceState{a, b}, 2, true)
	if got := uidsOf(page2); !slices.Equal(got, []string{"a8", "b5"}) {
		t.Errorf("page 2 = %v", got)
	}
	all := slices.Concat(page1, page2)
	for i := 1; i < len(all); i++ {
		if all[i].sum.InternalDate.After(all[i-1].sum.InternalDate) {
			t.Errorf("row %v comes after the older %v", uidsOf(all[i:i+1]), uidsOf(all[i-1:i]))
		}
	}
}

func TestMergeStopsWhenFetchedRowsWentMissing(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// 9 was expunged between SEARCH and FETCH, so a returned one row of two.
	a := stateOf("a", []uint32{10, 9, 8}, 2, summaryAt("a", "INBOX", 10, base.Add(10*time.Hour), ""))
	b := stateOf("b", []uint32{5}, 2, summaryAt("b", "INBOX", 5, base.Add(5*time.Hour), ""))
	if got := uidsOf(merge([]*sourceState{a, b}, 2, false)); !slices.Equal(got, []string{"a10"}) {
		t.Errorf("merge = %v, want a10 alone, as b5 is older than a's unfetched 8", got)
	}
	got := cursorSources(t, nextCursor(&searchPlan{fingerprint: ref.Fingerprint("q")}, []*sourceState{a, b}, nil))
	want := []ref.CursorSource{
		{Account: "a", Folder: "INBOX", UIDValidity: 1, Below: 9},
		{Account: "b", Folder: "INBOX", UIDValidity: 1, Below: 6},
	}
	if !slices.Equal(got, want) {
		t.Errorf("cursor = %+v, want %+v", got, want)
	}
}

func TestNextCursorBounds(t *testing.T) {
	t.Parallel()
	p := &searchPlan{fingerprint: ref.Fingerprint("q")}
	row := func(uid uint32) mailstore.Summary { return summaryAt("a", "INBOX", uid, time.Now(), "") }
	unreached := stateOf("a", []uint32{7, 3}, 2, row(7), row(3))
	resumed := stateOf("b", []uint32{4}, 2, summaryAt("b", "INBOX", 4, time.Now(), ""))
	resumed.prevBelow = 5
	failed := stateOf("c", []uint32{8, 6}, 2)
	failed.prevBelow = 9
	failed.err = errors.New("down")
	firstPageFailure := stateOf("d", []uint32{8, 6}, 2)
	firstPageFailure.err = errors.New("down")
	carried := []ref.CursorSource{{Account: "e", Folder: "INBOX", UIDValidity: 3, Below: 11}}
	got := cursorSources(t, nextCursor(p, []*sourceState{unreached, resumed, failed, firstPageFailure}, carried))
	want := []ref.CursorSource{
		// No page reached it: mail arriving later stays out of later pages.
		{Account: "a", Folder: "INBOX", UIDValidity: 1, Below: 8},
		{Account: "b", Folder: "INBOX", UIDValidity: 1, Below: 5},
		// A failed fetch continues where its page started, for a later page to retry.
		{Account: "c", Folder: "INBOX", UIDValidity: 1, Below: 9},
		{Account: "d", Folder: "INBOX", UIDValidity: 1, Below: 9},
		carried[0],
	}
	if !slices.Equal(got, want) {
		t.Errorf("cursor sources = %+v, want %+v", got, want)
	}
}
