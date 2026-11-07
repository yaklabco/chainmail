package rules

import "time"

// nextRun is when a rule with definition d next runs after after: the
// schedule's next instant in the store's location for an enabled rule with
// a schedule, else the zero time, which is stored as NULL.
func (s *Store) nextRun(d Definition, after time.Time) time.Time {
	if !d.Enabled || d.Schedule.Kind == ScheduleNone {
		return time.Time{}
	}
	return d.Schedule.Next(after, s.loc).UTC()
}
