package setuptest

import (
	"context"
	"slices"
	"sync"

	"github.com/yaklabco/chainmail/internal/setup/system"
)

// Units is a fake system.Units. Show returns Info, or Err when it is set,
// whatever unit it is asked about.
type Units struct {
	Info system.UnitInfo
	Err  error
	// Journal, when set, gets an entry for each call.
	Journal *Journal

	mu    sync.Mutex
	calls []string
}

var _ system.Units = (*Units)(nil)

// Show implements system.Units.
func (u *Units) Show(_ context.Context, unit string) (system.UnitInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, "show "+unit)
	u.Journal.Add("units show " + unit)
	if u.Err != nil {
		return system.UnitInfo{}, u.Err
	}
	return u.Info, nil
}

// Calls lists the calls so far: "show <unit>".
func (u *Units) Calls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.calls)
}
