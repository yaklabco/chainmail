package items

import (
	"errors"
	"time"

	"github.com/jamesainslie/chainmail/internal/watch"
)

// errStoredTime is the fixed error for a stored time column that does not
// parse. It never carries the column value.
var errStoredTime = errors.New("items: malformed stored time")

// formatTime renders t in the watch log's fixed-width UTC layout, so the item
// time columns sort and range-compare correctly as text. The zero time
// renders as "", which the nullable time columns read as unset.
func formatTime(t time.Time) string { return watch.FormatTime(t) }

// parseTime reads a time written by formatTime. "" reads as the zero time.
func parseTime(s string) (time.Time, error) {
	t, err := watch.ParseTime(s)
	if err != nil {
		return time.Time{}, errStoredTime
	}
	return t, nil
}
