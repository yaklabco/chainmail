package calcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// Each event's Raw is stored once per object and content: cal_raws holds
// it under its SHA-256, and each cal_events row names it by raw_sum. The
// VEVENTs of one CalDAV object share one Raw (calendar.ParseICS gives them
// the same slice), so an object with many overrides is hashed, stored and
// read back once, never copied per row. Rows read back share one slice per
// object and content.

// rawID names one Raw backing array, so the events that share it are
// summed once.
type rawID struct {
	p *byte
	n int
}

// rawSet holds the raw objects of one plan by sum.
type rawSet struct {
	byID  map[rawID]string
	bySum map[string][]byte
}

func newRawSet() *rawSet {
	return &rawSet{byID: map[rawID]string{}, bySum: map[string][]byte{}}
}

// add returns raw's sum, "" for none, computing it once per backing array.
func (s *rawSet) add(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	id := rawID{p: &raw[0], n: len(raw)}
	if sum, ok := s.byID[id]; ok {
		return sum
	}
	h := sha256.Sum256(raw)
	sum := hex.EncodeToString(h[:])
	s.byID[id] = sum
	if _, ok := s.bySum[sum]; !ok {
		s.bySum[sum] = raw
	}
	return sum
}

// rawKey names one cal_raws row.
type rawKey struct {
	account, calID, provider, sum string
}

// rawChunk bounds the keys of one cal_raws read: four variables each, well
// under SQLite's variable limit.
const rawChunk = 200

// loadRaws reads the cal_raws rows of keys. A key with no row is left out.
// It opens one query at a time and closes it before returning.
func loadRaws(ctx context.Context, q querier, keys []rawKey) (map[rawKey][]byte, error) {
	out := make(map[rawKey][]byte, len(keys))
	for chunk := range slices.Chunk(keys, rawChunk) {
		args := make([]any, 0, 4*len(chunk))
		for _, k := range chunk {
			args = append(args, k.account, k.calID, k.provider, k.sum)
		}
		values := strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?), ", len(chunk)), ", ")
		err := func() error {
			rows, err := q.QueryContext(ctx,
				"SELECT account, calendar_id, provider_id, sum, raw FROM cal_raws "+
					"WHERE (account, calendar_id, provider_id, sum) IN (VALUES "+values+")", args...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }() // rows.Err below reports read errors
			for rows.Next() {
				var (
					k   rawKey
					raw []byte
				)
				if err := rows.Scan(&k.account, &k.calID, &k.provider, &k.sum, &raw); err != nil {
					return err
				}
				out[k] = raw
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MaxObjectBytes bounds what the rows of one object may hold in one sync:
// the raw objects they carry, each backing array counted once however many
// rows share it, and the text of every row.
const MaxObjectBytes = 8 << 20

// rowOverhead is what Admit counts for each row besides its text.
const rowOverhead = 256

// Admit returns ch with the upserts of every object (ProviderID) that has
// more than calendar.MaxObjectEvents rows, or whose rows hold more than
// MaxObjectBytes, removed, and its ProviderID added to Skipped, sorted and
// without repeats. Diff and Apply admit the changes they are given the
// same way, so a caller uses Admit to learn what they will skip. ch's
// slices are not modified.
func Admit(ch calendar.Changes) calendar.Changes {
	type tally struct {
		rows  int
		bytes int64
		seen  map[rawID]bool
	}
	per := map[string]*tally{}
	for i := range ch.Upserts {
		ev := &ch.Upserts[i]
		t := per[ev.ProviderID]
		if t == nil {
			t = &tally{seen: map[rawID]bool{}}
			per[ev.ProviderID] = t
		}
		t.rows++
		t.bytes += rowBytes(ev)
		if n := len(ev.Raw); n > 0 {
			if id := (rawID{p: &ev.Raw[0], n: n}); !t.seen[id] {
				t.seen[id] = true
				t.bytes += int64(n)
			}
		}
	}
	skip := map[string]bool{}
	for p, t := range per {
		if t.rows > calendar.MaxObjectEvents || t.bytes > MaxObjectBytes {
			skip[p] = true
		}
	}
	if len(skip) == 0 {
		return ch
	}
	out := ch
	out.Upserts = make([]calendar.Event, 0, len(ch.Upserts))
	for _, ev := range ch.Upserts {
		if !skip[ev.ProviderID] {
			out.Upserts = append(out.Upserts, ev)
		}
	}
	skipped := slices.Clone(ch.Skipped)
	for p := range skip {
		skipped = append(skipped, p)
	}
	slices.Sort(skipped)
	out.Skipped = slices.Compact(skipped)
	return out
}

// rowBytes estimates the stored size of ev's row other than its raw.
func rowBytes(ev *calendar.Event) int64 {
	n := rowOverhead + len(ev.UID) + len(ev.ETag) + len(ev.Summary) + len(ev.Location) + len(ev.Description) +
		len(ev.RRule) + len(ev.TZID) + 24*(len(ev.RDates)+len(ev.ExDates))
	if o := ev.Organizer; o != nil {
		n += len(o.Email) + len(o.Name) + 32
	}
	for _, a := range ev.Attendees {
		n += len(a.Email) + len(a.Name) + len(a.PartStat) + len(a.Role) + 64
	}
	return int64(n)
}

// unreferencedRaw is the condition of a cal_raws row no event row names.
const unreferencedRaw = `NOT EXISTS (SELECT 1 FROM cal_events e
	WHERE e.account = cal_raws.account AND e.calendar_id = cal_raws.calendar_id
	AND e.provider_id = cal_raws.provider_id AND e.raw_sum = cal_raws.sum)`
