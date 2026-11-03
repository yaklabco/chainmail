package cache

import "time"

// utcDay is the budget day now falls in, the UTC date (spec section 4.4).
// Account methods use a.day(), which is utcDay of the account's clock.
func utcDay(now time.Time) string { return now.UTC().Format(time.DateOnly) }
