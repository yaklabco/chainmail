package setuptest

import (
	"slices"
	"sync"
	"time"
)

// Journal records what the fakes did, in order, across all of them, so a
// test can check that steps on different fakes ran in the right order.
// A nil *Journal records nothing.
type Journal struct {
	mu      sync.Mutex
	entries []string
}

// Add appends one entry. It does nothing on a nil Journal.
func (j *Journal) Add(entry string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, entry)
}

// Entries returns every entry so far.
func (j *Journal) Entries() []string {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.entries)
}

// Clock is a fake clock. Sleep advances it instead of waiting.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock reading start.
func NewClock(start time.Time) *Clock { return &Clock{now: start} }

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep advances the clock by d.
func (c *Clock) Sleep(d time.Duration) { c.Advance(d) }

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
