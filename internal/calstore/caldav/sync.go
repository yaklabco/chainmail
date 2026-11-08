package caldav

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

// Token forms. A sync-collection token carries the server's token; an etag
// token carries the collection's ctag and the etag of every object, since
// without sync-collection the next sync has to compare etags itself.
const (
	tokenSync = "s1:"
	tokenETag = "e1:"
	// maxSyncRounds bounds the sync-collection requests of one Sync when
	// the server truncates its answers (RFC 6578 section 3.6).
	maxSyncRounds = 100
	// maxTokenBytes bounds a decoded etag token.
	maxTokenBytes = 16 << 20
)

// etagState is the content of an etag token.
type etagState struct {
	CTag  string            `json:"c"`
	ETags map[string]string `json:"e"`
}

// storedObject is one object read with GET.
type storedObject struct {
	href string
	etag string
	raw  []byte
}

// getObject GETs one calendar object.
func (s *Store) getObject(ctx context.Context, href string) (*storedObject, error) {
	resp, err := s.do(ctx, request{
		method:   http.MethodGet,
		path:     href,
		header:   map[string]string{"Accept": "text/calendar"},
		limit:    calendar.MaxICSBytes,
		tooLarge: errCalendarTooLarge,
	})
	if err != nil {
		return nil, err
	}
	if resp.status != http.StatusOK {
		if resp.status/100 == 2 {
			return nil, fmt.Errorf("%w: status %d", calstore.ErrUnavailable, resp.status)
		}
		return nil, statusError(opRead, resp)
	}
	etag := normalizeETag(resp.header.Get("ETag"))
	if etag == "" {
		etag, err = s.etagOf(ctx, href)
		if err != nil {
			return nil, err
		}
	}
	return &storedObject{href: href, etag: etag, raw: resp.body}, nil
}

// etagOf reads an object's getetag.
func (s *Store) etagOf(ctx context.Context, href string) (string, error) {
	ms, err := s.propfind(ctx, href, "0", "d:getetag")
	if err != nil {
		return "", err
	}
	for _, r := range ms.Responses {
		if e := r.props().ETag; e != nil {
			return normalizeETag(*e), nil
		}
	}
	return "", fmt.Errorf("%w: no etag", calstore.ErrUnavailable)
}

// events parses a stored object into events carrying its href and etag.
func (o *storedObject) events() ([]calendar.Event, error) {
	_, evs, err := calendar.ParseICS(o.raw)
	if err != nil {
		return nil, err
	}
	for i := range evs {
		evs[i].ProviderID = o.href
		evs[i].ETag = o.etag
	}
	return evs, nil
}

// GetEvent returns every VEVENT of one object: its master and overrides,
// or one event.
func (s *Store) GetEvent(ctx context.Context, calID, providerID string) ([]calendar.Event, error) {
	_, href, err := objectPath(calID, providerID)
	if err != nil {
		return nil, err
	}
	obj, err := s.getObject(ctx, href)
	if err != nil {
		return nil, err
	}
	evs, err := obj.events()
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return nil, fmt.Errorf("%w: no event in object", calstore.ErrNotFound)
	}
	return evs, nil
}

// Sync returns the changes to one calendar since token, with sync-collection
// when the collection supports it and etag comparison otherwise. Changed
// objects are read with calendar-multiget. Each object's events replace
// the stored rows of that object (ObjectReplace). An object that no longer
// parses, or holds no VEVENT, is reported deleted. Only a 404 or 410 for a
// member is a deletion: a member the server answers with any other failure
// fails the whole Sync, so the caller keeps its token.
func (s *Store) Sync(ctx context.Context, calID, token string) (calendar.Changes, error) {
	coll, ok := collectionPath(calID)
	if !ok {
		return calendar.Changes{}, errBadID
	}
	switch {
	case token == "":
		return s.fullSync(ctx, coll)
	case strings.HasPrefix(token, tokenSync):
		return s.collectionSync(ctx, coll, strings.TrimPrefix(token, tokenSync), false)
	case strings.HasPrefix(token, tokenETag):
		state, err := decodeETagState(strings.TrimPrefix(token, tokenETag))
		if err != nil {
			return calendar.Changes{}, err
		}
		return s.etagSync(ctx, coll, state)
	default:
		return calendar.Changes{}, fmt.Errorf("%w: unknown token form", calstore.ErrSyncTokenInvalid)
	}
}

// fullSync lists the whole collection, by sync-collection when the
// collection reports a sync token or the report, else by calendar-query.
func (s *Store) fullSync(ctx context.Context, coll string) (calendar.Changes, error) {
	ms, err := s.propfind(ctx, coll, "0", "d:supported-report-set", "d:sync-token", "cs:getctag")
	if err != nil {
		return calendar.Changes{}, err
	}
	var p prop
	for _, r := range ms.Responses {
		if r.href() == coll || len(ms.Responses) == 1 {
			p = r.props()
			break
		}
	}
	if p.SyncToken != nil || p.ReportSet.has(nsDAV, "sync-collection") {
		return s.collectionSync(ctx, coll, "", true)
	}
	state := etagState{ETags: map[string]string{}}
	if p.CTag != nil {
		state.CTag = *p.CTag
	}
	return s.etagSyncFrom(ctx, coll, state, nil)
}

// collectionSync runs sync-collection from token ("" for the initial sync)
// until the server stops truncating.
func (s *Store) collectionSync(ctx context.Context, coll, token string, full bool) (calendar.Changes, error) {
	changed := map[string]string{} // href to etag
	deleted := map[string]bool{}
	for range maxSyncRounds {
		resp, err := s.do(ctx, request{
			method:      "REPORT",
			path:        coll,
			body:        syncCollectionBody(token),
			contentType: `application/xml; charset="utf-8"`,
			depth:       "0", // RFC 6578 section 3.2 fixes Depth at 0
			limit:       maxMultistatus,
		})
		if err != nil {
			return calendar.Changes{}, err
		}
		if resp.status != http.StatusMultiStatus {
			if resp.status/100 == 2 {
				return calendar.Changes{}, fmt.Errorf("%w: status %d", calstore.ErrUnavailable, resp.status)
			}
			return calendar.Changes{}, statusError(opSync, resp)
		}
		ms, err := parseMultistatus(resp.body)
		if err != nil {
			return calendar.Changes{}, err
		}
		if ms.SyncToken == "" {
			return calendar.Changes{}, fmt.Errorf("%w: no sync token", calstore.ErrUnavailable)
		}
		truncated := false
		for _, r := range ms.Responses {
			href := r.href()
			code := statusCode(r.Status)
			if href == coll || (href+"/") == coll {
				if code == http.StatusInsufficientStorage {
					truncated = true
				}
				continue
			}
			if !childOf(coll, href) {
				continue
			}
			if r.gone() {
				deleted[href] = true
				delete(changed, href)
				continue
			}
			// A member the server could not report is not a deleted one:
			// the sync fails and the caller keeps its token.
			e := r.props().ETag
			if code/100 != 2 || e == nil {
				return calendar.Changes{}, r.failure("etag")
			}
			changed[href] = normalizeETag(*e)
			delete(deleted, href)
		}
		token = strings.TrimSpace(ms.SyncToken)
		if !truncated {
			ch, _, err := s.fetchChanges(ctx, coll, changed, deleted, full)
			if err != nil {
				return calendar.Changes{}, err
			}
			ch.Token = tokenSync + token
			return ch, nil
		}
	}
	return calendar.Changes{}, fmt.Errorf("%w: sync did not finish", calstore.ErrUnavailable)
}

// etagSync compares the collection's ctag with the stored one and, when it
// changed or is unknown, its etags with the stored ones.
func (s *Store) etagSync(ctx context.Context, coll string, old etagState) (calendar.Changes, error) {
	ms, err := s.propfind(ctx, coll, "0", "cs:getctag")
	if err != nil {
		return calendar.Changes{}, err
	}
	var ctag string
	for _, r := range ms.Responses {
		if c := r.props().CTag; c != nil {
			ctag = *c
		}
	}
	if ctag != "" && ctag == old.CTag {
		tok, err := encodeETagState(old)
		if err != nil {
			return calendar.Changes{}, err
		}
		return calendar.Changes{Token: tok, ObjectReplace: true}, nil
	}
	return s.etagSyncFrom(ctx, coll, etagState{CTag: ctag, ETags: map[string]string{}}, old.ETags)
}

// etagSyncFrom lists etags with calendar-query and fetches what differs
// from prev. A nil prev is a full sync.
func (s *Store) etagSyncFrom(ctx context.Context, coll string, state etagState, prev map[string]string) (calendar.Changes, error) {
	resp, err := s.do(ctx, request{
		method:      "REPORT",
		path:        coll,
		body:        etagQueryBody(),
		contentType: `application/xml; charset="utf-8"`,
		depth:       "1",
		limit:       maxMultistatus,
	})
	if err != nil {
		return calendar.Changes{}, err
	}
	if resp.status != http.StatusMultiStatus {
		if resp.status/100 == 2 {
			return calendar.Changes{}, fmt.Errorf("%w: status %d", calstore.ErrUnavailable, resp.status)
		}
		return calendar.Changes{}, statusError(opRead, resp)
	}
	ms, err := parseMultistatus(resp.body)
	if err != nil {
		return calendar.Changes{}, err
	}
	changed := map[string]string{}
	for _, r := range ms.Responses {
		href := r.href()
		if !childOf(coll, href) || r.gone() {
			continue
		}
		e := r.props().ETag
		if statusCode(r.Status)/100 != 2 || e == nil {
			// Leaving out a member it could not read would report it
			// deleted.
			return calendar.Changes{}, r.failure("etag")
		}
		etag := normalizeETag(*e)
		state.ETags[href] = etag
		if prev == nil || prev[href] != etag {
			changed[href] = etag
		}
	}
	deleted := map[string]bool{}
	for href := range prev {
		if _, ok := state.ETags[href]; !ok {
			deleted[href] = true
		}
	}
	ch, vanished, err := s.fetchChanges(ctx, coll, changed, deleted, prev == nil)
	if err != nil {
		return calendar.Changes{}, err
	}
	// An object that no longer parses keeps its etag, so it is read again
	// only once it changes.
	for _, d := range vanished {
		delete(state.ETags, d)
	}
	ch.Token, err = encodeETagState(state)
	if err != nil {
		return calendar.Changes{}, err
	}
	return ch, nil
}

// fetchChanges reads the changed objects with calendar-multiget and builds
// the Changes. Objects the multiget answers with 404 or 410, and objects
// that no longer parse or hold no VEVENT, are deletes.
func (s *Store) fetchChanges(ctx context.Context, coll string, changed map[string]string, deleted map[string]bool, full bool) (calendar.Changes, []string, error) {
	ch := calendar.Changes{Full: full, ObjectReplace: true}
	hrefs := make([]string, 0, len(changed))
	for h := range changed {
		hrefs = append(hrefs, h)
	}
	sort.Strings(hrefs)
	objs, missing, err := s.multiget(ctx, coll, hrefs)
	if err != nil {
		return calendar.Changes{}, nil, err
	}
	for _, h := range missing {
		deleted[h] = true
	}
	for _, o := range objs {
		evs, err := o.events()
		if err != nil || len(evs) == 0 {
			deleted[o.href] = true
			continue
		}
		ch.Upserts = append(ch.Upserts, evs...)
	}
	if !full {
		for h := range deleted {
			ch.Deletes = append(ch.Deletes, h)
		}
		sort.Strings(ch.Deletes)
	}
	return ch, missing, nil
}

// multiget reads objects in batches, halving a batch whose answer is too
// large. It returns the objects read and the hrefs the server reported
// missing.
func (s *Store) multiget(ctx context.Context, coll string, hrefs []string) ([]*storedObject, []string, error) {
	var objs []*storedObject
	var missing []string
	for start := 0; start < len(hrefs); start += multigetBatch {
		o, m, err := s.multigetBatch(ctx, coll, hrefs[start:min(start+multigetBatch, len(hrefs))])
		if err != nil {
			return nil, nil, err
		}
		objs = append(objs, o...)
		missing = append(missing, m...)
	}
	return objs, missing, nil
}

var errMultigetTooLarge = errors.New("multiget response too large")

func (s *Store) multigetBatch(ctx context.Context, coll string, batch []string) ([]*storedObject, []string, error) {
	resp, err := s.do(ctx, request{
		method:      "REPORT",
		path:        coll,
		body:        multigetBody(batch),
		contentType: `application/xml; charset="utf-8"`,
		depth:       "1",
		limit:       maxMultistatus,
		tooLarge:    errMultigetTooLarge,
	})
	if errors.Is(err, errMultigetTooLarge) {
		if len(batch) == 1 {
			// One object larger than the response cap is larger than any
			// object chainmail reads: report it missing.
			return nil, batch, nil
		}
		half := len(batch) / 2
		a, am, err := s.multigetBatch(ctx, coll, batch[:half])
		if err != nil {
			return nil, nil, err
		}
		b, bm, err := s.multigetBatch(ctx, coll, batch[half:])
		if err != nil {
			return nil, nil, err
		}
		return append(a, b...), append(am, bm...), nil
	}
	if err != nil {
		return nil, nil, err
	}
	if resp.status != http.StatusMultiStatus {
		if resp.status/100 == 2 {
			return nil, nil, fmt.Errorf("%w: status %d", calstore.ErrUnavailable, resp.status)
		}
		return nil, nil, statusError(opRead, resp)
	}
	ms, err := parseMultistatus(resp.body)
	if err != nil {
		return nil, nil, err
	}
	want := map[string]bool{}
	for _, h := range batch {
		want[h] = true
	}
	var objs []*storedObject
	seen := map[string]bool{}
	for _, r := range ms.Responses {
		href := r.href()
		if !want[href] || seen[href] {
			continue
		}
		seen[href] = true
		if r.gone() {
			continue // reported missing below
		}
		p := r.props()
		if statusCode(r.Status)/100 != 2 || p.CalendarData == nil {
			return nil, nil, r.failure("calendar data")
		}
		// Without its etag an object cannot be replaced conditionally later,
		// so a missing, failed or empty etag fails the sync like missing data.
		if p.ETag == nil || normalizeETag(*p.ETag) == "" {
			return nil, nil, r.failure("etag")
		}
		etag := normalizeETag(*p.ETag)
		objs = append(objs, &storedObject{href: href, etag: etag, raw: []byte(*p.CalendarData)})
	}
	// Only an explicit 404 or 410 is a missing object. RFC 4791 section
	// 7.9 has the server answer for every href, so one left out is a
	// failure, not a deletion.
	var missing []string
	for _, h := range batch {
		if !seen[h] {
			return nil, nil, fmt.Errorf("%w: multiget answer incomplete", calstore.ErrUnavailable)
		}
		if !containsObject(objs, h) {
			missing = append(missing, h)
		}
	}
	return objs, missing, nil
}

func containsObject(objs []*storedObject, href string) bool {
	for _, o := range objs {
		if o.href == href {
			return true
		}
	}
	return false
}

func encodeETagState(st etagState) (string, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(st); err != nil {
		return "", fmt.Errorf("%w: cannot encode token", calstore.ErrUnavailable)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("%w: cannot encode token", calstore.ErrUnavailable)
	}
	return tokenETag + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

func decodeETagState(s string) (etagState, error) {
	invalid := fmt.Errorf("%w: malformed token", calstore.ErrSyncTokenInvalid)
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return etagState{}, invalid
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return etagState{}, invalid
	}
	data, err := io.ReadAll(io.LimitReader(zr, maxTokenBytes+1))
	if err != nil || len(data) > maxTokenBytes {
		return etagState{}, invalid
	}
	var st etagState
	if err := json.Unmarshal(data, &st); err != nil {
		return etagState{}, invalid
	}
	if st.ETags == nil {
		st.ETags = map[string]string{}
	}
	return st, nil
}
