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
	s.syncBytes = 10 * len(singleICS)
	_, err := s.Sync(context.Background(), "/cal/", "")
	wantTooLarge(t, err)
	if gets.Load() != 1 {
		t.Errorf("Sync read %d multiget batches, want 1", gets.Load())
	}
}
