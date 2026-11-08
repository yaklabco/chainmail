package google_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/calstore/google"
)

// pager serves events.list pages of perPage events each, with ever new page
// tokens unless token is fixed, and counts the requests.
func pager(perPage int, token string, pad int) (*http.Client, *atomic.Int64) {
	var n atomic.Int64
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		page := n.Add(1)
		var items []string
		for i := range perPage {
			items = append(items, fmt.Sprintf(`{"id":"e%d_%d","iCalUID":"u%d_%d","description":%q,`+
				`"start":{"dateTime":"2026-11-10T09:00:00Z"},"end":{"dateTime":"2026-11-10T10:00:00Z"}}`,
				page, i, page, i, strings.Repeat("d", pad)))
		}
		next := token
		if next == "" {
			next = fmt.Sprintf("page%d", page)
		}
		body := `{"items":[` + strings.Join(items, ",") + `],"nextPageToken":"` + next + `"}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	return hc, &n
}

func TestSyncRefusesARepeatedPageToken(t *testing.T) {
	hc, n := pager(1, "again", 0)
	st := google.New(hc, google.WithBaseURL("http://gcal.test/calendar/v3"))
	_, err := st.Sync(ctx(t), me, "")
	if !errors.Is(err, calstore.ErrUnavailable) {
		t.Fatalf("Sync = %v, want ErrUnavailable", err)
	}
	if got := n.Load(); got != 2 {
		t.Errorf("Sync made %d requests, want 2", got)
	}
}

func TestListCalendarsRefusesARepeatedPageToken(t *testing.T) {
	var n atomic.Int64
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"items":[],"nextPageToken":"again"}`)), Request: r,
		}, nil
	})}
	_, err := google.New(hc, google.WithBaseURL("http://gcal.test/calendar/v3")).ListCalendars(ctx(t))
	if !errors.Is(err, calstore.ErrUnavailable) || n.Load() != 2 {
		t.Errorf("ListCalendars = %v after %d requests, want ErrUnavailable after 2", err, n.Load())
	}
}

func TestSyncBounds(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bytes, events int
		perPage, pad  int
	}{
		{"bytes", 8 << 10, 1 << 20, 2, 1000},
		{"events", 1 << 30, 10, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hc, n := pager(tc.perPage, "", tc.pad)
			st := google.New(hc, google.WithBaseURL("http://gcal.test/calendar/v3"))
			google.SetSyncLimits(st, tc.bytes, tc.events)
			_, err := st.Sync(ctx(t), me, "")
			if !errors.Is(err, calstore.ErrSyncTooLarge) || !errors.Is(err, calstore.ErrUnavailable) {
				t.Fatalf("Sync = %v, want ErrSyncTooLarge", err)
			}
			if err.Error() != "calendar server unavailable: calendar too large to sync" {
				t.Errorf("error text %q", err)
			}
			if got := n.Load(); got > 10 {
				t.Errorf("Sync read %d pages past its bound", got)
			}
		})
	}
}
