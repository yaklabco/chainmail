package caldav

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/yaklabco/chainmail/internal/calstore"
)

// XML namespaces.
const (
	nsDAV    = "DAV:"
	nsCalDAV = "urn:ietf:params:xml:ns:caldav"
	nsCS     = "http://calendarserver.org/ns/"
)

// multistatus is a DAV multistatus response (RFC 4918 section 14.16, RFC
// 6578 section 6.4 for sync-token).
type multistatus struct {
	XMLName   xml.Name     `xml:"DAV: multistatus"`
	Responses []msResponse `xml:"DAV: response"`
	SyncToken string       `xml:"DAV: sync-token"`
}

type msResponse struct {
	Hrefs     []string   `xml:"DAV: href"`
	Status    string     `xml:"DAV: status"`
	Propstats []propstat `xml:"DAV: propstat"`
}

type propstat struct {
	Status string `xml:"DAV: status"`
	Prop   prop   `xml:"DAV: prop"`
}

// prop holds every property this package asks for. A nil field was not
// returned with a success status.
type prop struct {
	CurrentUserPrincipal   *hrefs        `xml:"DAV: current-user-principal"`
	CalendarHomeSet        *hrefs        `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
	ScheduleInboxURL       *hrefs        `xml:"urn:ietf:params:xml:ns:caldav schedule-inbox-URL"`
	ScheduleOutboxURL      *hrefs        `xml:"urn:ietf:params:xml:ns:caldav schedule-outbox-URL"`
	ScheduleDefaultCal     *hrefs        `xml:"urn:ietf:params:xml:ns:caldav schedule-default-calendar-URL"`
	CalendarUserAddressSet *hrefs        `xml:"urn:ietf:params:xml:ns:caldav calendar-user-address-set"`
	ResourceType           *elements     `xml:"DAV: resourcetype"`
	DisplayName            *string       `xml:"DAV: displayname"`
	PrivilegeSet           *privilegeSet `xml:"DAV: current-user-privilege-set"`
	ComponentSet           *componentSet `xml:"urn:ietf:params:xml:ns:caldav supported-calendar-component-set"`
	ReportSet              *reportSet    `xml:"DAV: supported-report-set"`
	CalendarTimezone       *string       `xml:"urn:ietf:params:xml:ns:caldav calendar-timezone"`
	SyncToken              *string       `xml:"DAV: sync-token"`
	CTag                   *string       `xml:"http://calendarserver.org/ns/ getctag"`
	ETag                   *string       `xml:"DAV: getetag"`
	CalendarData           *string       `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
}

type hrefs struct {
	Hrefs []string `xml:"DAV: href"`
}

func (h *hrefs) first() string {
	if h == nil || len(h.Hrefs) == 0 {
		return ""
	}
	return strings.TrimSpace(h.Hrefs[0])
}

// elements collects the names of an element's children.
type elements struct {
	Children []struct {
		XMLName xml.Name
	} `xml:",any"`
}

func (e *elements) has(space, local string) bool {
	if e == nil {
		return false
	}
	for _, c := range e.Children {
		if c.XMLName.Space == space && c.XMLName.Local == local {
			return true
		}
	}
	return false
}

type privilegeSet struct {
	Privileges []elements `xml:"DAV: privilege"`
}

// writable reports whether the set grants writing objects: DAV:all,
// DAV:write or DAV:write-content.
func (p *privilegeSet) writable() bool {
	for _, priv := range p.Privileges {
		if priv.has(nsDAV, "all") || priv.has(nsDAV, "write") || priv.has(nsDAV, "write-content") {
			return true
		}
	}
	return false
}

type componentSet struct {
	Comps []struct {
		Name string `xml:"name,attr"`
	} `xml:"urn:ietf:params:xml:ns:caldav comp"`
}

func (c *componentSet) hasEvents() bool {
	for _, comp := range c.Comps {
		if strings.EqualFold(comp.Name, "VEVENT") {
			return true
		}
	}
	return false
}

type reportSet struct {
	Reports []struct {
		Report elements `xml:"DAV: report"`
	} `xml:"DAV: supported-report"`
}

func (r *reportSet) has(space, local string) bool {
	if r == nil {
		return false
	}
	for _, rep := range r.Reports {
		if rep.Report.has(space, local) {
			return true
		}
	}
	return false
}

// parseMultistatus decodes a 207 body. Errors carry no body text.
func parseMultistatus(body []byte) (*multistatus, error) {
	var ms multistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return nil, fmt.Errorf("%w: malformed multistatus", calstore.ErrUnavailable)
	}
	return &ms, nil
}

// href returns a response's canonical path, or "" when it has none.
func (r *msResponse) href() string {
	if len(r.Hrefs) == 0 {
		return ""
	}
	p, ok := canonPath(strings.TrimSpace(r.Hrefs[0]))
	if !ok {
		return ""
	}
	return p
}

// gone reports whether a response says its resource no longer exists: a
// response-level 404 or 410, as RFC 6578 section 3.5.2 reports a removed
// member and RFC 4791 section 7.9 a missing multiget href.
func (r *msResponse) gone() bool {
	code := statusCode(r.Status)
	return code == http.StatusNotFound || code == http.StatusGone
}

// failure returns the error for a response that lacks a property it was
// asked for: its failed response-level status, else the status of its
// first failed propstat, else ErrUnavailable naming the missing property.
func (r *msResponse) failure(missing string) error {
	if code := statusCode(r.Status); code/100 != 2 {
		return resourceError(code)
	}
	for _, ps := range r.Propstats {
		if code := statusCode(ps.Status); code/100 != 2 {
			return resourceError(code)
		}
	}
	return fmt.Errorf("%w: no %s", calstore.ErrUnavailable, missing)
}

// resourceError maps the failure of one resource inside a multistatus:
// 401 and 403 to ErrAuth, anything else to ErrUnavailable. A failed
// resource is never taken for a deleted one.
func resourceError(code int) error {
	sentinel := calstore.ErrUnavailable
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		sentinel = calstore.ErrAuth
	}
	return fmt.Errorf("%w: resource status %d", sentinel, code)
}

// props merges the properties of every 2xx propstat.
func (r *msResponse) props() prop {
	var out prop
	for _, ps := range r.Propstats {
		if code := statusCode(ps.Status); code/100 != 2 {
			continue
		}
		p := ps.Prop
		mergeProp(&out, &p)
	}
	return out
}

func mergeProp(dst, src *prop) {
	set := func(d **hrefs, s *hrefs) {
		if s != nil {
			*d = s
		}
	}
	set(&dst.CurrentUserPrincipal, src.CurrentUserPrincipal)
	set(&dst.CalendarHomeSet, src.CalendarHomeSet)
	set(&dst.ScheduleInboxURL, src.ScheduleInboxURL)
	set(&dst.ScheduleOutboxURL, src.ScheduleOutboxURL)
	set(&dst.ScheduleDefaultCal, src.ScheduleDefaultCal)
	set(&dst.CalendarUserAddressSet, src.CalendarUserAddressSet)
	str := func(d **string, s *string) {
		if s != nil {
			*d = s
		}
	}
	str(&dst.DisplayName, src.DisplayName)
	str(&dst.CalendarTimezone, src.CalendarTimezone)
	str(&dst.SyncToken, src.SyncToken)
	str(&dst.CTag, src.CTag)
	str(&dst.ETag, src.ETag)
	str(&dst.CalendarData, src.CalendarData)
	if src.ResourceType != nil {
		dst.ResourceType = src.ResourceType
	}
	if src.PrivilegeSet != nil {
		dst.PrivilegeSet = src.PrivilegeSet
	}
	if src.ComponentSet != nil {
		dst.ComponentSet = src.ComponentSet
	}
	if src.ReportSet != nil {
		dst.ReportSet = src.ReportSet
	}
}

// statusCode reads the code of an HTTP status line such as
// "HTTP/1.1 404 Not Found". It returns 200 for an empty line, which RFC
// 4918 allows only where success is implied, and 0 for a malformed one.
func statusCode(line string) int {
	line = strings.TrimSpace(line)
	if line == "" {
		return 200
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0
	}
	n, err := strconv.Atoi(f[1])
	if err != nil {
		return 0
	}
	return n
}

// elementNames returns the local names of every element in an XML body,
// for reading DAV precondition codes. A malformed body gives what was read
// before the error.
func elementNames(body []byte) map[string]bool {
	names := map[string]bool{}
	dec := xml.NewDecoder(bytes.NewReader(body))
	for range 10000 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok {
			names[se.Name.Local] = true
		}
	}
	return names
}

func hasElement(body []byte, local string) bool {
	return elementNames(body)[local]
}

// xmlText escapes s for XML character data.
func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s)) // a strings.Builder never fails
	return b.String()
}

const xmlHeader = `<?xml version="1.0" encoding="utf-8"?>` + "\n"

// propfindBody asks for the given properties, each a prefixed element name
// such as "d:displayname".
func propfindBody(props ...string) []byte {
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/"><d:prop>`)
	for _, p := range props {
		b.WriteString("<" + p + "/>")
	}
	b.WriteString(`</d:prop></d:propfind>`)
	return []byte(b.String())
}

// syncCollectionBody is an RFC 6578 sync-collection REPORT for etags.
func syncCollectionBody(token string) []byte {
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<d:sync-collection xmlns:d="DAV:"><d:sync-token>`)
	b.WriteString(xmlText(token))
	b.WriteString(`</d:sync-token><d:sync-level>1</d:sync-level><d:prop><d:getetag/></d:prop></d:sync-collection>`)
	return []byte(b.String())
}

// etagQueryBody is a calendar-query REPORT listing the etags of every
// object with a VEVENT.
func etagQueryBody() []byte {
	return []byte(xmlHeader + `<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<d:prop><d:getetag/></d:prop>` +
		`<c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"/></c:comp-filter></c:filter>` +
		`</c:calendar-query>`)
}

// multigetBody is a calendar-multiget REPORT for the etag and data of each
// href.
func multigetBody(paths []string) []byte {
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/><c:calendar-data/></d:prop>`)
	for _, p := range paths {
		b.WriteString("<d:href>" + xmlText(p) + "</d:href>")
	}
	b.WriteString(`</c:calendar-multiget>`)
	return []byte(b.String())
}

// scheduleResponse is an RFC 6638 schedule-response to a free/busy POST.
type scheduleResponse struct {
	XMLName   xml.Name `xml:"urn:ietf:params:xml:ns:caldav schedule-response"`
	Responses []struct {
		Recipient struct {
			Href string `xml:"DAV: href"`
			Text string `xml:",chardata"`
		} `xml:"urn:ietf:params:xml:ns:caldav recipient"`
		RequestStatus string `xml:"urn:ietf:params:xml:ns:caldav request-status"`
		CalendarData  string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
	} `xml:"urn:ietf:params:xml:ns:caldav response"`
}
