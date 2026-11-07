package rules

import (
	"time"

	"github.com/yaklabco/chainmail/internal/watch"
)

// The due stages. Each is the watch event kind of its stage, with the
// value watch.KindItemDueSoon, watch.KindItemDue and watch.KindItemOverdue
// have.
const (
	StageDueSoon watch.Kind = "item_due_soon"
	StageDue     watch.Kind = "item_due"
	StageOverdue watch.Kind = "item_overdue"
)

// DueSettings say when due stages begin: at At on the wall clock of
// Location.
type DueSettings struct {
	// Location is the [rules] timezone. Nil means time.Local.
	Location *time.Location
	// At is the [rules] due_time.
	At ClockTime
	// SoonDays is due_soon_days. 0 (or less) turns item_due_soon off.
	SoonDays int
}

// location is s.Location, or time.Local when nil.
func (s DueSettings) location() *time.Location {
	if s.Location == nil {
		return time.Local
	}
	return s.Location
}

// DueStage returns the most advanced stage reached at now for a due date:
// KindItemOverdue from (due+1 day at At), KindItemDue from (due at At),
// KindItemDueSoon from (due-SoonDays at At) when SoonDays > 0. ok is false
// before the first stage or for a bad date.
func DueStage(dueDate string, now time.Time, s DueSettings) (watch.Kind, bool) {
	due, err := time.Parse(time.DateOnly, dueDate)
	if err != nil || due.Format(time.DateOnly) != dueDate || !s.At.valid() {
		return "", false
	}
	loc := s.location()
	y, m, d := due.Date()
	switch {
	case !now.Before(wallInstant(y, m, d+1, s.At, loc)):
		return StageOverdue, true
	case !now.Before(wallInstant(y, m, d, s.At, loc)):
		return StageDue, true
	case s.SoonDays > 0 && !now.Before(wallInstant(y, m, d-s.SoonDays, s.At, loc)):
		return StageDueSoon, true
	}
	return "", false
}

// NextDueWake is the next instant strictly after now at At in Location.
func NextDueWake(now time.Time, s DueSettings) time.Time {
	return Schedule{Kind: ScheduleAt, At: s.At}.Next(now, s.location())
}
