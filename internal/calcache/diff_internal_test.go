package calcache

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// refusingQuerier fails the test on any query.
type refusingQuerier struct{ t *testing.T }

func (q refusingQuerier) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	q.t.Helper()
	q.t.Error("planChanges read the stored rows, want no read")
	return nil, errors.New("no read expected")
}

// TestPlanChangesReadsNothingForAnEmptyIncrementalSync covers an
// incremental sync that touches no ProviderID, such as a poll that only
// moves the token. Its plan is empty, and loading the stored rows of no
// ProviderIDs must not turn into loading the whole calendar.
func TestPlanChangesReadsNothingForAnEmptyIncrementalSync(t *testing.T) {
	t.Parallel()
	for _, ch := range []calendar.Changes{
		{Token: "t2"},
		{Token: "t2", ObjectReplace: true},
		{Token: "t2", Upserts: []calendar.Event{}, Deletes: []string{}},
	} {
		p, err := planChanges(t.Context(), refusingQuerier{t}, "acme", "work", ch, nil)
		if err != nil || len(p.changes) != 0 {
			t.Errorf("planChanges(%+v) = %v, %v; want an empty plan", ch, p.changes, err)
		}
	}
}

func TestLoadRecordsOfNoProvidersReadsNothing(t *testing.T) {
	t.Parallel()
	for _, providers := range [][]string{nil, {}} {
		old, err := loadRecords(t.Context(), refusingQuerier{t}, "acme", "work", false, providers)
		if err != nil || len(old) != 0 {
			t.Errorf("loadRecords(%#v) = %v, %v; want no rows", providers, old, err)
		}
	}
}
