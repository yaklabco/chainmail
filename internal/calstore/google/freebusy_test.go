package google_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore/google"
	"github.com/yaklabco/chainmail/internal/testutil/fakegcal"
)

func TestFreeBusy(t *testing.T) {
	st, srv := newStore(t)
	day := utc(2026, 11, 10, 0, 0)
	srv.SetFreeBusy("ann@example.org",
		fakegcal.Busy{Start: day.Add(9 * time.Hour), End: day.Add(10 * time.Hour)},
		fakegcal.Busy{Start: day.Add(10 * time.Hour), End: day.Add(11 * time.Hour)},
		fakegcal.Busy{Start: day.Add(14 * time.Hour), End: day.Add(15 * time.Hour)},
	)
	srv.SetFreeBusy("free@example.org")

	got, err := st.FreeBusy(ctx(t), []string{"ann@example.org", "free@example.org", "nobody@example.org"}, day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("FreeBusy: %v", err)
	}
	ann := got["ann@example.org"]
	want := []calendar.BusyBlock{
		{Start: day.Add(9 * time.Hour), End: day.Add(11 * time.Hour)},
		{Start: day.Add(14 * time.Hour), End: day.Add(15 * time.Hour)},
	}
	if !ann.Known || !slices.EqualFunc(ann.Busy, want, func(a, b calendar.BusyBlock) bool {
		return a.Start.Equal(b.Start) && a.End.Equal(b.End)
	}) {
		t.Errorf("ann = %+v, want %+v", ann, want)
	}
	if free := got["free@example.org"]; !free.Known || len(free.Busy) != 0 {
		t.Errorf("free person = %+v, want known with no busy time", free)
	}
	if nobody, ok := got["nobody@example.org"]; !ok || nobody.Known {
		t.Errorf("unknown person = %+v (present %v), want Known false", nobody, ok)
	}

	req, _ := srv.LastRequest("POST")
	b := body(t, req)
	if b["timeMin"] != "2026-11-10T00:00:00Z" || b["timeMax"] != "2026-11-11T00:00:00Z" {
		t.Errorf("freeBusy range = %v to %v", b["timeMin"], b["timeMax"])
	}
}

func TestFreeBusyBatchesAndDuplicates(t *testing.T) {
	st, srv := newStore(t)
	day := utc(2026, 11, 10, 0, 0)
	var people []string
	for i := range fakegcal.MaxFreeBusyItems + 1 {
		people = append(people, "p"+itoa(i)+"@example.org")
	}
	people = append(people, "P0@Example.org")
	srv.SetFreeBusy("p50@example.org", fakegcal.Busy{Start: day, End: day.Add(time.Hour)})

	got, err := st.FreeBusy(ctx(t), people, day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("FreeBusy: %v", err)
	}
	if len(got) != len(people) {
		t.Errorf("got %d people, want %d", len(got), len(people))
	}
	if p := got["p50@example.org"]; !p.Known || len(p.Busy) != 1 {
		t.Errorf("person in the second batch = %+v", p)
	}
	var sizes []int
	for _, r := range srv.Requests() {
		if r.Path == "/freeBusy" {
			sizes = append(sizes, len(body(t, r)["items"].([]any)))
		}
	}
	if !slices.Equal(sizes, []int{fakegcal.MaxFreeBusyItems, 1}) {
		t.Errorf("batch sizes = %v, want [50 1] with the duplicate merged", sizes)
	}
}

func TestFreeBusyEdges(t *testing.T) {
	st, srv := newStore(t)
	day := utc(2026, 11, 10, 0, 0)
	got, err := st.FreeBusy(ctx(t), nil, day, day.Add(time.Hour))
	if err != nil || len(got) != 0 {
		t.Errorf("nobody: %v, %v", got, err)
	}
	if _, err := st.FreeBusy(ctx(t), []string{"ann@example.org"}, day, day); !errors.Is(err, google.ErrRequest) {
		t.Errorf("empty range: err = %v, want ErrRequest", err)
	}
	if n := len(srv.Requests()); n != 0 {
		t.Errorf("sent %d requests for nothing to ask", n)
	}
}
