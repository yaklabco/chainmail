package cache

import "time"

// budgetDayLayout formats a budget day, a UTC date (spec section 4.4).
const budgetDayLayout = "2006-01-02"

// sysClock is watch.Clock over package time, for options that leave Clock nil.
type sysClock struct{}

func (sysClock) Now() time.Time { return time.Now() }

func (sysClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// utcDay is the budget day now falls in.
func utcDay(now time.Time) string { return now.UTC().Format(budgetDayLayout) }

// untilUTCMidnight is how long from now until the next budget day starts,
// when spent budgets reset.
func untilUTCMidnight(now time.Time) time.Duration {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC).Sub(now)
}
