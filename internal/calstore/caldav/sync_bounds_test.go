package caldav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yaklabco/chainmail/internal/calstore"
)

// members lists n objects of /cal/ with etags.
func members(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = resp(fmt.Sprintf("/cal/%d.ics", i), fmt.Sprintf(`<d:getetag>"%d"</d:getetag>`, i))
	}
	return out
}

// multigetAnswers answers every href of a calendar-multiget with data,
// and counts the requests.
func multigetAnswers(t *testing.T, list []string, data func(href string) string) (*davServer, *atomic.Int64) {
	t.Helper()
	var gets atomic.Int64
	d := newDAVServer(t)
	d.multistatus("PROPFIND", "/cal/", ms(resp("/cal/", `<cs:getctag>2</cs:getctag>`)))
	d.handle("REPORT", "/cal/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b := string(body)
		w.WriteHeader(http.StatusMultiStatus)
		if strings.Contains(b, "calendar-query") {
			_, _ = fmt.Fprint(w, ms(list...))
			return
		}
		gets.Add(1)
		var out []string
		for _, h := range strings.Split(b, "<d:href>")[1:] {
			href, _, _ := strings.Cut(h, "<")
			out = append(out, resp(href, `<d:getetag>"x"</d:getetag><c:calendar-data>`+xmlText(data(href))+`</c:calendar-data>`))
		}
		_, _ = fmt.Fprint(w, ms(out...))
	})
	return d, &gets
}

// listingCost is what an etag listing of members(n) takes from a Sync's
// byte budget.
func listingCost(n int) int {
	cost := 0
	for i := range n {
		cost += len(fmt.Sprintf("/cal/%d.ics", i)) + len(fmt.Sprintf(`"%d"`, i)) + 2*entryOverhead
	}
	return cost
}

func wantTooLarge(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, calstore.ErrSyncTooLarge) || !errors.Is(err, calstore.ErrUnavailable) {
		t.Fatalf("Sync = %v, want ErrSyncTooLarge", err)
	}
}

func TestSyncBoundsTheObjectsListed(t *testing.T) {
	d, gets := multigetAnswers(t, members(5), func(string) string { return singleICS })
	s := d.store(t, "/")
	s.syncEvents = 4
	_, err := s.Sync(context.Background(), "/cal/", "")
	wantTooLarge(t, err)
	if gets.Load() != 0 {
		t.Errorf("Sync read %d multiget batches of a listing over the bound", gets.Load())
	}
}

func TestSyncCollectionBoundsTheChanges(t *testing.T) {
	d := reportServer(t, members(5), nil, nil)
	s := d.store(t, "/")
	s.syncEvents = 4
	_, err := s.Sync(context.Background(), "/cal/", tokenSync+"t1")
	wantTooLarge(t, err)
}

func TestSyncBoundsTheEventsKept(t *testing.T) {
	d, _ := multigetAnswers(t, members(3), func(string) string { return seriesICS }) // two VEVENTs each
	s := d.store(t, "/")
	s.syncEvents = 5
	_, err := s.Sync(context.Background(), "/cal/", "")
	wantTooLarge(t, err)
}

// TestSyncBoundsTheBytesKept also checks that objects are parsed as each
// multiget batch arrives: the bound stops the sync after the first batch.
func TestSyncBoundsTheBytesKept(t *testing.T) {
	d, gets := multigetAnswers(t, members(3*multigetBatch), func(string) string { return singleICS })
	s := d.store(t, "/")
	// The listing is kept too: the budget holds it and ten objects.
	s.syncBytes = listingCost(3*multigetBatch) + 10*len(singleICS)
	_, err := s.Sync(context.Background(), "/cal/", "")
	wantTooLarge(t, err)
	if gets.Load() != 1 {
		t.Errorf("Sync read %d multiget batches, want 1", gets.Load())
	}
}

// longMembers lists n objects of /cal/ whose hrefs and etags are each
// about pad bytes long.
func longMembers(n, pad int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = resp(fmt.Sprintf("/cal/%d-%s.ics", i, strings.Repeat("h", pad)),
			fmt.Sprintf(`<d:getetag>"%d-%s"</d:getetag>`, i, strings.Repeat("e", pad)))
	}
	return out
}

// The hrefs and etags a listing keeps count against the Sync's byte
// budget, whatever their number: a few long ones over it stop the Sync
// before anything is fetched.
func TestSyncBoundsTheListingBytes(t *testing.T) {
	t.Run("etag listing", func(t *testing.T) {
		d, gets := multigetAnswers(t, longMembers(8, 1000), func(string) string { return singleICS })
		s := d.store(t, "/")
		s.syncBytes = 8 << 10
		_, err := s.Sync(context.Background(), "/cal/", "")
		wantTooLarge(t, err)
		if gets.Load() != 0 {
			t.Errorf("Sync read %d multiget batches of a listing over the budget", gets.Load())
		}
	})
	t.Run("sync-collection", func(t *testing.T) {
		d := reportServer(t, longMembers(8, 1000), nil, nil)
		s := d.store(t, "/")
		s.syncBytes = 8 << 10
		_, err := s.Sync(context.Background(), "/cal/", tokenSync+"t1")
		wantTooLarge(t, err)
	})
	t.Run("listing and objects share the budget", func(t *testing.T) {
		// The listing alone fits, and so do the objects alone; together
		// they do not. About 8 KB of listing and 16 KB of objects against 20 KB.
		big := strings.Replace(singleICS, "END:VEVENT", strings.Repeat("X-PAD:"+strings.Repeat("p", 60)+"\r\n", 120)+"END:VEVENT", 1)
		d, _ := multigetAnswers(t, longMembers(2, 2000), func(string) string { return big })
		s := d.store(t, "/")
		s.syncBytes = 20000
		_, err := s.Sync(context.Background(), "/cal/", "")
		wantTooLarge(t, err)
		short, _ := multigetAnswers(t, members(2), func(string) string { return big })
		ss := short.store(t, "/")
		ss.syncBytes = 20000
		if _, err := ss.Sync(context.Background(), "/cal/", ""); err != nil {
			t.Errorf("the same objects with short hrefs: Sync = %v, want them kept", err)
		}
	})
}

// A sync token longer than maxPageToken is refused before it is kept.
func TestSyncRefusesALongSyncToken(t *testing.T) {
	d := newDAVServer(t)
	d.handle("REPORT", "/cal/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = fmt.Fprint(w, strings.Replace(ms(), "</d:multistatus>",
			"<d:sync-token>"+strings.Repeat("t", maxPageToken+1)+"</d:sync-token></d:multistatus>", 1))
	})
	s := d.store(t, "/")
	_, err := s.Sync(context.Background(), "/cal/", tokenSync+"t1")
	if !errors.Is(err, calstore.ErrUnavailable) {
		t.Fatalf("Sync = %v, want ErrUnavailable", err)
	}
}
