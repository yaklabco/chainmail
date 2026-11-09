package google

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// maxFreeBusyItems is the most calendars one freeBusy query may name.
const maxFreeBusyItems = 50

type freeBusyAnswer struct {
	Calendars map[string]struct {
		Busy []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"busy"`
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"calendars"`
}

// FreeBusy asks Google for the busy time of each address in people over
// [from, to), at most 50 people per query. A person Google reports an error
// for, or leaves out, is Known false: unknown, never free. The result has an
// entry for every element of people, duplicates included; addresses that
// differ only in case are asked once.
func (s *Store) FreeBusy(ctx context.Context, people []string, from, to time.Time) (map[string]calendar.FreeBusy, error) {
	out := make(map[string]calendar.FreeBusy, len(people))
	if len(people) == 0 {
		return out, nil
	}
	if !to.After(from) {
		return nil, fmt.Errorf("%w: empty time range", ErrRequest)
	}
	var ask []string
	seen := map[string]bool{}
	for _, p := range people {
		if k := strings.ToLower(p); !seen[k] {
			seen[k] = true
			ask = append(ask, p)
		}
	}
	answers := map[string]calendar.FreeBusy{}
	for len(ask) > 0 {
		n := min(len(ask), maxFreeBusyItems)
		got, err := s.freeBusy(ctx, ask[:n], from, to)
		if err != nil {
			return nil, err
		}
		for k, v := range got {
			answers[k] = v
		}
		ask = ask[n:]
	}
	for _, p := range people {
		out[p] = answers[strings.ToLower(p)]
	}
	return out, nil
}

// freeBusy runs one query and returns the answers by lowercased address.
func (s *Store) freeBusy(ctx context.Context, people []string, from, to time.Time) (map[string]calendar.FreeBusy, error) {
	items := make([]map[string]string, len(people))
	for i, p := range people {
		items[i] = map[string]string{"id": p}
	}
	data, err := s.do(ctx, call{method: "POST", path: "/freeBusy", limit: maxPageBytes, body: map[string]any{
		"timeMin": from.UTC().Format(time.RFC3339),
		"timeMax": to.UTC().Format(time.RFC3339),
		"items":   items,
	}})
	if err != nil {
		return nil, err
	}
	var ans freeBusyAnswer
	if err := json.Unmarshal(data, &ans); err != nil {
		return nil, errBadResponse
	}
	byKey := make(map[string]string, len(ans.Calendars))
	for id := range ans.Calendars {
		byKey[strings.ToLower(id)] = id
	}
	out := make(map[string]calendar.FreeBusy, len(people))
	for _, p := range people {
		key := strings.ToLower(p)
		id, ok := byKey[key]
		if !ok {
			out[key] = calendar.FreeBusy{}
			continue
		}
		c := ans.Calendars[id]
		if len(c.Errors) > 0 {
			out[key] = calendar.FreeBusy{}
			continue
		}
		fb := calendar.FreeBusy{Known: true}
		for _, b := range c.Busy {
			start, err1 := time.Parse(time.RFC3339, b.Start)
			end, err2 := time.Parse(time.RFC3339, b.End)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("%w: bad busy block", errBadResponse)
			}
			fb.Busy = append(fb.Busy, calendar.BusyBlock{Start: start.UTC(), End: end.UTC()})
		}
		fb.Busy = calendar.MergeBusy(fb.Busy)
		out[key] = fb
	}
	return out, nil
}
