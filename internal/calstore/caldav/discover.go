package caldav

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

// wellKnown is the RFC 6764 bootstrap path.
const wellKnown = "/.well-known/caldav"

// discovery is what the principal says about the account.
type discovery struct {
	principal  string
	homes      []string // calendar-home-set collections
	inbox      string   // schedule-inbox-URL, "" when the server has none
	outbox     string   // schedule-outbox-URL, "" when the server has none
	addresses  []string // calendar-user-address-set mailto addresses, without the scheme
	defaultCal string   // schedule-default-calendar-URL, "" when unknown
}

// discover finds the principal, its calendar homes and its scheduling
// collections, once per Store. A failure is not kept, so the next call
// tries again.
func (s *Store) discover(ctx context.Context) (*discovery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disc != nil {
		return s.disc, nil
	}
	principal, err := s.findPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := s.propfind(ctx, principal, "0",
		"c:calendar-home-set", "c:schedule-inbox-URL", "c:schedule-outbox-URL", "c:calendar-user-address-set")
	if err != nil {
		return nil, err
	}
	d := &discovery{principal: principal}
	for _, r := range resp.Responses {
		p := r.props()
		if p.CalendarHomeSet != nil {
			for _, h := range p.CalendarHomeSet.Hrefs {
				if c, ok := collectionPath(strings.TrimSpace(h)); ok {
					d.homes = append(d.homes, c)
				}
			}
		}
		if c, ok := collectionPath(p.ScheduleInboxURL.first()); ok && p.ScheduleInboxURL.first() != "" {
			d.inbox = c
		}
		if c, ok := collectionPath(p.ScheduleOutboxURL.first()); ok && p.ScheduleOutboxURL.first() != "" {
			d.outbox = c
		}
		if p.CalendarUserAddressSet != nil {
			for _, h := range p.CalendarUserAddressSet.Hrefs {
				h = strings.TrimSpace(h)
				if len(h) > 7 && strings.EqualFold(h[:7], "mailto:") {
					d.addresses = append(d.addresses, h[7:])
				}
			}
		}
	}
	if len(d.homes) == 0 {
		return nil, fmt.Errorf("%w: no calendar home", calstore.ErrUnavailable)
	}
	if d.inbox != "" {
		d.defaultCal = s.defaultCalendar(ctx, d.inbox)
	}
	s.disc = d
	return d, nil
}

// findPrincipal asks for current-user-principal at the account URL and at
// /.well-known/caldav, the well-known path first when the account URL is
// the server root.
func (s *Store) findPrincipal(ctx context.Context) (string, error) {
	candidates := []string{s.base.RawPath, wellKnown}
	if s.base.RawPath == "/" || s.base.RawPath == "" {
		candidates = []string{wellKnown, "/"}
	}
	var lastErr error
	for _, c := range candidates {
		resp, err := s.propfind(ctx, c, "0", "d:current-user-principal")
		if err != nil {
			if errors.Is(err, calstore.ErrAuth) || ctx.Err() != nil {
				return "", err
			}
			lastErr = err
			continue
		}
		for _, r := range resp.Responses {
			if p, ok := canonPath(r.props().CurrentUserPrincipal.first()); ok && r.props().CurrentUserPrincipal.first() != "" {
				return p, nil
			}
		}
	}
	if lastErr != nil && errors.Is(lastErr, calstore.ErrUnavailable) {
		return "", lastErr
	}
	return "", fmt.Errorf("%w: no principal found", calstore.ErrUnavailable)
}

// defaultCalendar reads schedule-default-calendar-URL from the inbox. Any
// failure means no default.
func (s *Store) defaultCalendar(ctx context.Context, inbox string) string {
	resp, err := s.propfind(ctx, inbox, "0", "c:schedule-default-calendar-URL")
	if err != nil {
		return ""
	}
	for _, r := range resp.Responses {
		if h := r.props().ScheduleDefaultCal.first(); h != "" {
			if c, ok := collectionPath(h); ok {
				return c
			}
		}
	}
	return ""
}

// propfind sends a PROPFIND and parses the multistatus.
func (s *Store) propfind(ctx context.Context, path, depth string, props ...string) (*multistatus, error) {
	resp, err := s.do(ctx, request{
		method:      "PROPFIND",
		path:        path,
		body:        propfindBody(props...),
		contentType: `application/xml; charset="utf-8"`,
		depth:       depth,
		limit:       maxMultistatus,
	})
	if err != nil {
		return nil, err
	}
	if resp.status != http.StatusMultiStatus {
		if resp.status/100 == 2 {
			return nil, fmt.Errorf("%w: status %d", calstore.ErrUnavailable, resp.status)
		}
		return nil, statusError(opRead, resp)
	}
	return parseMultistatus(resp.body)
}

// ListCalendars returns the calendar collections of every calendar home
// that can hold events.
func (s *Store) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	d, err := s.discover(ctx)
	if err != nil {
		return nil, err
	}
	var out []calendar.Calendar
	seen := map[string]bool{}
	for _, home := range d.homes {
		ms, err := s.propfind(ctx, home, "1",
			"d:resourcetype", "d:displayname", "d:current-user-privilege-set",
			"c:supported-calendar-component-set", "c:calendar-timezone")
		if err != nil {
			return nil, err
		}
		for _, r := range ms.Responses {
			href := r.href()
			if href == "" {
				continue
			}
			id, ok := collectionPath(href)
			if !ok || id == home || seen[id] {
				continue
			}
			p := r.props()
			if !p.ResourceType.has(nsCalDAV, "calendar") {
				continue
			}
			if p.ComponentSet != nil && len(p.ComponentSet.Comps) > 0 && !p.ComponentSet.hasEvents() {
				continue
			}
			seen[id] = true
			c := calendar.Calendar{ID: id, Access: calendar.AccessWrite, Primary: id == d.defaultCal}
			if p.DisplayName != nil {
				c.Name = cleanLine(*p.DisplayName)
			}
			if p.PrivilegeSet != nil && !p.PrivilegeSet.writable() {
				c.Access = calendar.AccessRead
			}
			if p.CalendarTimezone != nil {
				c.TimeZone = timezoneID(*p.CalendarTimezone)
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// timezoneID reads the TZID of the VTIMEZONE in a calendar-timezone
// property and returns it when it is an IANA zone Go knows.
func timezoneID(vtimezone string) string {
	sc := bufio.NewScanner(strings.NewReader(vtimezone))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if len(line) < 5 || !strings.EqualFold(line[:5], "TZID:") {
			continue
		}
		id := strings.TrimSpace(line[5:])
		if loadable(id) {
			return id
		}
		return ""
	}
	return ""
}

// loadable reports whether id names an IANA zone other than UTC or Local.
func loadable(id string) bool {
	if id == "" || id == "Local" || strings.EqualFold(id, "UTC") || strings.ContainsAny(id, "\r\n") {
		return false
	}
	_, err := time.LoadLocation(id)
	return err == nil
}
