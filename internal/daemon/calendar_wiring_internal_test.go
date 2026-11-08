package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/calstore/caldav"
	calfake "github.com/yaklabco/chainmail/internal/calstore/fake"
	"github.com/yaklabco/chainmail/internal/calstore/google"
	"github.com/yaklabco/chainmail/internal/calsync"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/watch"
)

// calendarConfig has a CalDAV account, a Google account and a password
// account with no calendar store.
const calendarConfig = `
[calendar]
poll = "2m"
window_past = "10d"

[oauth_clients.g]
client_id = "client-id"

[accounts.gm]
provider = "gmail"
address = "me@gmail.com"
auth = "oauth2"
oauth_client = "g"

[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.mw]
provider = "hostinger"
address = "james@mw.example"

[accounts.mw.calendar]
store = "caldav"
url = "https://mw.example/"
include = ["Work"]
`

// calendarStores returns a NewCalendarStore that hands out one fake per
// account and records the accounts asked for.
func calendarStores() (func(config.ResolvedAccount) calstore.Store, map[string]*calfake.Store, func() []string) {
	var mu sync.Mutex
	var asked []string
	stores := map[string]*calfake.Store{"gm": calfake.New(calfake.Options{}), "mw": calfake.New(calfake.Options{ObjectReplace: true})}
	return func(acct config.ResolvedAccount) calstore.Store {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, acct.Name)
			return stores[acct.Name]
		}, stores, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Sorted(slices.Values(asked))
		}
}

func TestCalendarAccounts(t *testing.T) {
	t.Parallel()
	newStore, stores, asked := calendarStores()
	s, _ := newTestServerOptions(t, calendarConfig, Options{NewCalendarStore: newStore})
	if got := asked(); !slices.Equal(got, []string{"gm", "mw"}) {
		t.Errorf("stores built for %v, want gm and mw", got)
	}
	if len(s.calendars) != 3 {
		t.Errorf("calendars has %d accounts, want 3", len(s.calendars))
	}
	for name, want := range map[string]calstore.Store{"gm": stores["gm"], "mw": stores["mw"], "host": nil} {
		ca, err := s.calendarAccount(name)
		if err != nil {
			t.Fatalf("calendarAccount(%s): %v", name, err)
		}
		if ca.name != name || ca.store != want || ca.syncer != nil {
			t.Errorf("calendarAccount(%s) = %+v, want store %v and no syncer before Serve", name, ca, want)
		}
	}
	if s.calSettings.Poll != 2*time.Minute || s.calSettings.WindowPast != 10*24*time.Hour {
		t.Errorf("calSettings = %+v", s.calSettings)
	}
	if _, err := s.calendarAccount("nope"); !errors.Is(err, config.ErrUnknownAccount) {
		t.Errorf("calendarAccount(nope) = %v, want ErrUnknownAccount", err)
	}
	if _, err := s.calendarAccount("Bad Name"); !errors.Is(err, errInvalidAccountName) {
		t.Errorf("calendarAccount(Bad Name) = %v, want errInvalidAccountName", err)
	}
}

// TestCalendarRealStores checks that without NewCalendarStore the daemon
// builds the Google and CalDAV backends.
func TestCalendarRealStores(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerConfig(t, calendarConfig)
	if _, ok := s.calendars["gm"].store.(*google.Store); !ok {
		t.Errorf("gm store is %T, want *google.Store", s.calendars["gm"].store)
	}
	if _, ok := s.calendars["mw"].store.(*caldav.Store); !ok {
		t.Errorf("mw store is %T, want *caldav.Store", s.calendars["mw"].store)
	}
	if s.calendars["host"].store != nil {
		t.Errorf("host store is %T, want none", s.calendars["host"].store)
	}
}

func TestOpenCalendar(t *testing.T) {
	t.Parallel()
	newStore, _, _ := calendarStores()
	dir := filepath.Join(t.TempDir(), "cache")
	s, _ := newTestServerOptions(t, calendarConfig, Options{NewCalendarStore: newStore, CacheDir: dir, DisableCache: true})
	s.openCalendar(t.Context())
	defer s.closeCalendar()
	if s.calcache == nil {
		t.Fatal("calendar.db did not open")
	}
	if _, err := os.Stat(filepath.Join(dir, calcache.FileName)); err != nil {
		t.Errorf("calendar.db: %v", err)
	}
	for _, name := range []string{"gm", "mw"} {
		if s.calendars[name].syncer == nil {
			t.Errorf("%s has no syncer", name)
		}
	}
	if s.calendars["host"].syncer != nil {
		t.Error("host, with no store, has a syncer")
	}
}

func TestOpenCalendarDisabled(t *testing.T) {
	t.Parallel()
	newStore, _, asked := calendarStores()
	dir := t.TempDir()
	s, _ := newTestServerOptions(t, calendarConfig, Options{NewCalendarStore: newStore, CacheDir: dir, DisableCalendar: true})
	s.openCalendar(t.Context())
	s.closeCalendar()
	if s.calcache != nil || len(asked()) != 0 {
		t.Errorf("calcache %v, stores built for %v; want neither", s.calcache, asked())
	}
	if _, err := os.Stat(filepath.Join(dir, calcache.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("calendar.db exists with DisableCalendar: %v", err)
	}
	ca, err := s.calendarAccount("gm")
	if err != nil || ca.store != nil || ca.syncer != nil {
		t.Errorf("calendarAccount(gm) = %+v, %v; want no store", ca, err)
	}
}

// TestOpenCalendarNoDir checks that without a cache directory there is no
// calendar.db and no syncer, but the stores stay.
func TestOpenCalendarNoDir(t *testing.T) {
	t.Parallel()
	newStore, _, _ := calendarStores()
	s, _ := newTestServerOptions(t, calendarConfig, Options{NewCalendarStore: newStore})
	s.openCalendar(t.Context())
	s.closeCalendar()
	if s.calcache != nil || s.calendars["gm"].syncer != nil || s.calendars["gm"].store == nil {
		t.Errorf("calcache %v, gm %+v; want no cache, a store and no syncer", s.calcache, s.calendars["gm"])
	}
}

func TestCalendarReady(t *testing.T) {
	t.Parallel()
	newStore, _, _ := calendarStores()
	s, _ := newTestServerOptions(t, calendarConfig, Options{NewCalendarStore: newStore, CacheDir: t.TempDir(), DisableCache: true})
	if err := s.calendarReady(t.Context(), "gm"); !errors.Is(err, errNotReady) {
		t.Errorf("calendarReady before open = %v, want not ready", err)
	}
	s.openCalendar(t.Context())
	defer s.closeCalendar()
	err := s.calendarReady(t.Context(), "gm")
	cerr := toConnectError(err)
	if d := api.Detail(cerr); d.GetReason() != api.ReasonCacheNotReady || !strings.Contains(d.GetHint(), "calendar") {
		t.Errorf("calendarReady = %v (%v), want CACHE_NOT_READY with a calendar hint", err, d)
	}
	if err := s.calcache.MarkReady(t.Context(), "gm", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.calendarReady(t.Context(), "gm"); err != nil {
		t.Errorf("calendarReady after MarkReady = %v", err)
	}
}

func TestCalendarStatuses(t *testing.T) {
	t.Parallel()
	newStore, stores, _ := calendarStores()
	s, _ := newTestServerOptions(t, calendarConfig, Options{NewCalendarStore: newStore, CacheDir: t.TempDir(), DisableCache: true})
	stores["gm"].AddCalendar(calendar.Calendar{ID: "primary@gm", Name: "Me", Access: calendar.AccessWrite, Primary: true})
	stores["gm"].Add("primary@gm", calendar.Event{UID: "a@x", Start: time.Now().Add(time.Hour), End: time.Now().Add(2 * time.Hour)})
	stores["mw"].Fail(calfake.MethodListCalendars, fmt.Errorf("%w: status 401", calstore.ErrAuth))
	s.openCalendar(t.Context())
	defer s.closeCalendar()
	accts, err := s.resolveAccounts(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Before any cycle: syncing, with nothing counted.
	got := s.calendarStatuses(t.Context(), accts)
	if len(got) != 2 || got[0].GetAccount() != "gm" || got[0].GetState() != calsync.StateSyncing || got[1].GetAccount() != "mw" {
		t.Fatalf("statuses = %v, want gm and mw syncing", got)
	}
	if err := s.calendars["gm"].syncer.SyncNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = s.calendars["mw"].syncer.SyncNow(t.Context())
	got = s.calendarStatuses(t.Context(), accts)
	gm, mw := got[0], got[1]
	if gm.GetState() != calsync.StateReady || gm.GetCalendars() != 1 || gm.GetEvents() != 1 || gm.GetLastSyncAt() == nil || gm.GetLastError() != "" {
		t.Errorf("gm = %v, want ready with 1 calendar and 1 event", gm)
	}
	if mw.GetState() != calsync.StateAuthRequired || mw.GetLastError() != api.ReasonAuthRequired {
		t.Errorf("mw = %v, want auth_required with reason AUTH_REQUIRED", mw)
	}
}

// TestCalendarStatusesNoCache checks the line of an account whose syncer
// could not start: error, with an INTERNAL reason.
func TestCalendarStatusesNoCache(t *testing.T) {
	t.Parallel()
	newStore, _, _ := calendarStores()
	s, _ := newTestServerOptions(t, calendarConfig, Options{NewCalendarStore: newStore})
	accts, err := s.resolveAccounts([]string{"gm", "host"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.calendarStatuses(t.Context(), accts)
	if len(got) != 1 || got[0].GetAccount() != "gm" || got[0].GetState() != calsync.StateError || got[0].GetLastError() != api.ReasonInternal {
		t.Errorf("statuses = %v, want gm in error", got)
	}
}

func TestCalendarErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		code   connect.Code
		reason string
		hint   string
	}{
		{"auth with account", calendarError("gm", fmt.Errorf("%w: status 401", calstore.ErrAuth)), connect.CodeUnauthenticated, api.ReasonAuthRequired, "run: chainmail auth login gm"},
		{"auth bare", fmt.Errorf("%w: status 401", calstore.ErrAuth), connect.CodeUnauthenticated, api.ReasonAuthRequired, "run: chainmail auth login <account>"},
		{"conflict", fmt.Errorf("%w: status 412", calstore.ErrConflict), connect.CodeAborted, api.ReasonEventConflict, ""},
		{"read only", fmt.Errorf("%w: status 403", calstore.ErrReadOnly), connect.CodePermissionDenied, api.ReasonCalendarReadOnly, ""},
		{"store not found", fmt.Errorf("%w: status 404", calstore.ErrNotFound), connect.CodeNotFound, api.ReasonNotFound, ""},
		{"cache not found", calcache.ErrNotFound, connect.CodeNotFound, api.ReasonNotFound, ""},
		{"unavailable", fmt.Errorf("%w: status 503", calstore.ErrUnavailable), connect.CodeUnavailable, api.ReasonServerUnavailable, ""},
		{
			"invalid ref", fmt.Errorf("%w: bad base", calendar.ErrInvalidRef), connect.CodeInvalidArgument, api.ReasonInvalidArgument,
			"event refs come from agenda, invites or event results; pass them unchanged",
		},
		{"google request", fmt.Errorf("%w: status 400", google.ErrRequest), connect.CodeInvalidArgument, api.ReasonInvalidArgument, ""},
		{"caldav rejected", fmt.Errorf("%w: status 403", caldav.ErrRejected), connect.CodeFailedPrecondition, api.ReasonInvalidArgument, ""},
		{"not attendee", caldav.ErrNotAttendee, connect.CodeFailedPrecondition, api.ReasonInvalidArgument, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cerr := toConnectError(tt.err)
			d := api.Detail(cerr)
			if cerr.Code() != tt.code || d.GetReason() != tt.reason {
				t.Errorf("toConnectError = %v %s, want %v %s", cerr.Code(), d.GetReason(), tt.code, tt.reason)
			}
			if tt.hint != "" && d.GetHint() != tt.hint {
				t.Errorf("hint = %q, want %q", d.GetHint(), tt.hint)
			}
		})
	}
}

// TestCalendarErrorTextFixed checks that store errors reach the client as
// fixed text, whatever the wrapped error says.
func TestCalendarErrorTextFixed(t *testing.T) {
	t.Parallel()
	for _, sentinel := range []error{google.ErrRequest, caldav.ErrRejected, caldav.ErrNotAttendee} {
		cerr := toConnectError(fmt.Errorf("%w: %s", sentinel, "SECRET-EVENT-TEXT"))
		if strings.Contains(cerr.Message(), "SECRET") {
			t.Errorf("%v reaches the client as %q", sentinel, cerr.Message())
		}
	}
}

func TestEventToProtoCalendarKinds(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	start := at.Add(24 * time.Hour)
	in := watch.Event{ID: 9, At: at, Kind: watch.KindInviteReply, Account: "gm", Ref: "ce_x", Calendar: &watch.CalendarChange{
		Ref: "ce_x", MessageRef: "m_1", CalendarID: "work", Start: start.Format(time.RFC3339), End: "not a time",
		Organizer: "boss@example.com", Fields: []string{"start"}, Attendee: "a@example.com", PartStat: "ACCEPTED",
	}}
	want := &chainmailv1.WatchEvent{Id: 9, At: timestamppb.New(at), Kind: "invite_reply", Account: "gm", Ref: "ce_x", Calendar: &chainmailv1.CalendarChange{
		Ref: "ce_x", MessageRef: "m_1", CalendarId: "work", Start: timestamppb.New(start),
		Organizer: &chainmailv1.Address{Email: &chainmailv1.Untrusted{Untrusted: "boss@example.com"}},
		Fields:    []string{"start"},
		Attendee:  &chainmailv1.Address{Email: &chainmailv1.Untrusted{Untrusted: "a@example.com"}},
		Partstat:  "ACCEPTED",
	}}
	if got := eventToProto(in); !proto.Equal(got, want) {
		t.Errorf("eventToProto = %v, want %v", got, want)
	}
	plain := eventToProto(watch.Event{Kind: watch.KindEventCancelled, Account: "gm", Ref: "ce_y", Calendar: &watch.CalendarChange{Ref: "ce_y"}})
	if c := plain.GetCalendar(); c.GetOrganizer() != nil || c.GetAttendee() != nil || c.GetStart() != nil {
		t.Errorf("unset payload fields are set: %v", c)
	}
}

// TestCacheWorkersFindInvitesOnceCalendarOpens checks that the cache
// workers get the invite hook once calendar.db is open, and none before.
func TestCacheWorkersFindInvitesOnceCalendarOpens(t *testing.T) {
	t.Parallel()
	newStore, _, _ := calendarStores()
	dir := filepath.Join(t.TempDir(), "cache")
	s, _ := newTestServerOptions(t, calendarConfig, Options{NewCalendarStore: newStore, CacheDir: dir, DisableCache: true})
	acct, err := s.opts.Config.Account("gm")
	if err != nil {
		t.Fatal(err)
	}
	if s.cacheAccountOptions(acct).OnInvites != nil {
		t.Error("OnInvites is set before calendar.db opens")
	}
	s.openCalendar(t.Context())
	defer s.closeCalendar()
	if s.cacheAccountOptions(acct).OnInvites == nil {
		t.Error("OnInvites is nil with calendar.db open")
	}
	s.pokeCalendar("gm")      // a syncer: queued, never blocks
	s.pokeCalendar("host")    // no store, no syncer: nothing to do
	s.pokeCalendar("missing") // not configured: nothing to do
}
