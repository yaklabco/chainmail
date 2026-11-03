package query

import "time"

// AgeStart is the day an age such as 7d reaches back to from now, at
// midnight UTC. Searches compare dates without times, so newer_than:1d means
// since the start of yesterday. The day is taken in now's own zone. A month
// or year back from a day the target month lacks is that month's last day:
// newer_than:1m on 31 March is since 28 February. The longest age the parser
// allows, 9999y, still fits a time.Time. v must be an age ParseAge accepts;
// any other value is read as zero days, giving the start of today.
func AgeStart(now time.Time, v string) time.Time {
	n, unit, _ := ParseAge(v) // callers validated v
	y, m, d := now.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	switch unit {
	case 'w':
		return day.AddDate(0, 0, -7*n)
	case 'm':
		return AddMonths(day, -n)
	case 'y':
		return AddMonths(day, -12*n)
	default:
		return day.AddDate(0, 0, -n)
	}
}

// AddMonths moves t by months, keeping its time of day and location. Where
// time.AddDate would spill past the end of a shorter target month, as 31
// March less a month gives 3 March, AddMonths stops at that month's last day.
func AddMonths(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(months), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	if last := first.AddDate(0, 1, -1).Day(); d > last {
		d = last
	}
	return first.AddDate(0, 0, d-1)
}
