package rules

import "time"

// AppliedRow is one rule's act-once record of one message: whether its
// actions ran (applied), were held (a forward saved as a draft), or may
// have run (pending, written before a guarded action), with a note, the
// run that wrote it and when.
type AppliedRow struct {
	Outcome AppliedOutcome
	Note    string
	RunID   int64
	At      time.Time
}
