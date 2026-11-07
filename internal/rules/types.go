package rules

import (
	"regexp"
	"slices"
	"time"
)

// Limits and defaults of a rule definition.
const (
	// IDPrefix starts every rule ID.
	IDPrefix = "ru_"
	// DefaultPriority is the priority of a rule that names none. Lower runs
	// first.
	DefaultPriority = 100
	// MinPriority and MaxPriority bound a rule's priority.
	MinPriority = -1000000
	MaxPriority = 1000000
	// DefaultMaxPerRun is the most messages one run acts on unless the
	// rule says otherwise.
	DefaultMaxPerRun = 500
	// MaxMaxPerRun is the highest max_per_run a rule may set.
	MaxMaxPerRun = 5000
	// MaxActions is the most actions one rule may hold.
	MaxActions = 16
	// MaxDescription is the longest description, in characters.
	MaxDescription = 1000
	// MaxNote is the longest forward note, in characters.
	MaxNote = 2000
	// MaxFolderName is the longest folder or label name, in bytes.
	MaxFolderName = 255
)

var (
	ruleIDRE   = regexp.MustCompile(`^ru_[0-9a-z_]{1,60}$`)
	ruleNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// ValidID matches ^ru_[0-9a-z_]{1,60}$, every rule ID items.NewID makes
// with the ru_ prefix.
func ValidID(id string) bool {
	return ruleIDRE.MatchString(id)
}

// ValidName matches ^[a-z0-9][a-z0-9-]{0,62}$. A name never starts with
// ru_, so a value that names a rule is an ID or a name, never both.
func ValidName(n string) bool {
	return ruleNameRE.MatchString(n)
}

// Trigger is what started a run. Triggers are stored, so their values never
// change.
type Trigger string

// The triggers.
const (
	TriggerNew      Trigger = "new"
	TriggerSchedule Trigger = "schedule"
	TriggerManual   Trigger = "manual"
)

// Outcome is how a run ended. Outcomes are stored.
type Outcome string

// The run outcomes.
const (
	OutcomeOK            Outcome = "ok"
	OutcomeCapped        Outcome = "capped"
	OutcomeFailed        Outcome = "failed"
	OutcomeCacheNotReady Outcome = "cache_not_ready"
	OutcomeInterrupted   Outcome = "interrupted"
)

// AppliedOutcome is what a rule did to one message, in the act-once table.
// Applied outcomes are stored.
type AppliedOutcome string

// The applied outcomes. AppliedPending is written before a guarded action
// runs, so a replay knows the action may already have happened.
const (
	AppliedPending AppliedOutcome = "pending"
	AppliedDone    AppliedOutcome = "applied"
	AppliedHeld    AppliedOutcome = "held"
)

// Notes on an applied row, saying why a replay recorded it without acting.
const (
	NoteReplayed       = "replayed"
	NoteOutcomeUnknown = "outcome_unknown"
)

// Changes say what a rule revision and a rule event record. They are
// stored.
const (
	ChangeCreated  = "created"
	ChangeUpdated  = "updated"
	ChangeDeleted  = "deleted"
	ChangeRestored = "restored"
	ChangeDisabled = "disabled"
	ChangeEnabled  = "enabled"
)

// Reasons the runner disables a rule on its own. They are stored.
const (
	ReasonAllFailed        = "all_failed"
	ReasonRepeatedFailures = "repeated_failures"
)

// Definition is what a client writes. Zero MaxPerRun is filled by
// Normalize. A zero Priority is a real priority, so callers that mean
// "default" start from Defaults().
type Definition struct {
	Name        string
	Description string
	// Accounts is sorted and de-duplicated by Normalize. Empty means every
	// account.
	Accounts  []string
	Query     string
	OnNew     bool
	Schedule  Schedule
	Priority  int
	Stop      bool
	Actions   []Action
	MaxPerRun int
	Enabled   bool
}

// Defaults returns an empty definition with the default priority and
// max_per_run, enabled.
func Defaults() Definition {
	return Definition{Priority: DefaultPriority, MaxPerRun: DefaultMaxPerRun, Enabled: true}
}

// Normalize returns d with Accounts sorted and de-duplicated (nil when
// empty) and a zero MaxPerRun set to DefaultMaxPerRun. Every other field is
// kept as it is. The result shares no slices with d.
func (d Definition) Normalize() Definition {
	n := d
	n.Accounts = nil
	if len(d.Accounts) > 0 {
		n.Accounts = slices.Clone(d.Accounts)
		slices.Sort(n.Accounts)
		n.Accounts = slices.Compact(n.Accounts)
	}
	if d.Actions != nil {
		n.Actions = make([]Action, len(d.Actions))
		for i, a := range d.Actions {
			a.To = slices.Clone(a.To)
			n.Actions[i] = a
		}
	}
	if n.MaxPerRun == 0 {
		n.MaxPerRun = DefaultMaxPerRun
	}
	return n
}

// Rule is a stored rule: its definition and the bookkeeping the store
// keeps.
type Rule struct {
	ID string
	Definition
	// DisabledReason is set when the runner disabled the rule:
	// ReasonAllFailed or ReasonRepeatedFailures.
	DisabledReason string
	// Revision is the rule's own revision, 1 on create.
	Revision  int64
	CreatedAt time.Time
	UpdatedAt time.Time
	// DeletedAt is zero while the rule is live.
	DeletedAt time.Time
	CreatedBy string
	UpdatedBy string
	// NextRunAt and LastRunAt are zero when there is none.
	NextRunAt time.Time
	LastRunAt time.Time
	// FailedRuns counts consecutive runs that had a permanent failure.
	FailedRuns int
}
