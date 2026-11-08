package rules

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/mailaddr"
	"github.com/yaklabco/chainmail/internal/query"
)

// AccountInfo is what validation needs to know about one account.
type AccountInfo struct {
	// Gmail is true for a Google account, where labels exist.
	Gmail bool
}

// Env is the configuration a definition is checked against.
type Env struct {
	// Accounts holds every configured account by name.
	Accounts map[string]AccountInfo
	// MaxRecipients is the policy's max_recipients. Below 1 reads as 1.
	MaxRecipients int
}

// Hints shared by several refusals.
const (
	accountsHint = "name accounts that chainmail accounts list shows, or leave accounts empty for every account"
	actionsHint  = "give 1 to 16 actions, such as label=Receipts or archive"
	folderHint   = "a folder or label name is 1 to 255 bytes of UTF-8 with no control characters"
	queryHint    = "write the query as chainmail search takes it, such as from:billing@example.com in:inbox"
)

// Refusals of a definition, in the order Validate checks them. The text is
// fixed and never holds rule text.
var (
	errName            = &InvalidError{Msg: "the rule name is not valid", Hint: "a name is 1 to 63 characters of a-z, 0-9 and -, starting with a letter or digit"}
	errDescriptionUTF8 = &InvalidError{Msg: "the description is not valid UTF-8", Hint: "a description is at most 1000 characters"}
	errDescriptionLong = &InvalidError{Msg: "the description is longer than 1000 characters", Hint: "a description is at most 1000 characters"}
	errAccountUnknown  = &InvalidError{Msg: "the rule names an account that is not configured", Hint: accountsHint}
	errQueryAccount    = &InvalidError{Msg: "the query names an account that is not configured", Hint: accountsHint}
	errNoActions       = &InvalidError{Msg: "the rule has no actions", Hint: actionsHint}
	errTooManyActions  = &InvalidError{Msg: "the rule has more than 16 actions", Hint: actionsHint}
	errStrayArgument   = &InvalidError{Msg: "an action has an argument its kind does not take", Hint: "label and unlabel take a label, copy and move a folder, link_item an item, forward recipients and a note; other kinds take none"}
	errLabelName       = &InvalidError{Msg: "a label name is not valid", Hint: folderHint}
	errFolderName      = &InvalidError{Msg: "a folder name is not valid", Hint: folderHint}
	errLinkItem        = &InvalidError{Msg: "a link_item action needs a valid item ID", Hint: "name an item by its ID, such as it_admin_work; chainmail item list shows them"}
	errNoteUTF8        = &InvalidError{Msg: "a forward note is not valid UTF-8", Hint: "a forward note is at most 2000 characters"}
	errNoteLong        = &InvalidError{Msg: "a forward note is longer than 2000 characters", Hint: "a forward note is at most 2000 characters"}
	errTwoRelocations  = &InvalidError{Msg: "the rule has more than one move, archive or trash action", Hint: "keep one of move, archive and trash"}
	errRelocationLast  = &InvalidError{Msg: "a move, archive or trash action must be the last action", Hint: "put move, archive or trash after every other action"}
	errTwoForwards     = &InvalidError{Msg: "the rule has more than one forward action", Hint: "forward to several recipients in one forward action"}
	errTwoCopies       = &InvalidError{Msg: "the rule has more than one copy action", Hint: "keep one copy action, or make a second rule"}
	errLabelNotGmail   = &InvalidError{Msg: "label and unlabel work only on Google accounts", Hint: "name only Google accounts in accounts, or use copy or move"}
	errNoRecipients    = &InvalidError{Msg: "a forward action has no recipients", Hint: "forward to at least one address, such as forward=alex@example.com"}
	errTooRecipients   = &InvalidError{Msg: "a forward action has more recipients than the send policy allows", Hint: "forward to at most max_recipients addresses, set in [policy]"}
	errRecipient       = &InvalidError{Msg: "a forward recipient is not a plain address", Hint: "write each recipient as local@domain, with no display name or quoting"}
	errPriority        = &InvalidError{Msg: "the priority is outside -1000000 to 1000000", Hint: "lower runs first; the default is 100"}
	errMaxPerRun       = &InvalidError{Msg: "max_per_run is outside 1 to 5000", Hint: "the default is 500"}
)

// Validate checks a normalized definition. Every problem is an *InvalidError
// with fixed text. Checks, in order: name, description, accounts known,
// query parses (query.Parse) and its account: terms name known accounts,
// schedule bounds, 1 to MaxActions actions, each action's arguments,
// at most one relocating action and it is last, at most one forward, at
// most one copy, label and unlabel only when every covered account is Gmail
// (covered: Accounts, or every account in Env when empty), forward 1 to
// MaxRecipients addresses each accepted by mailaddr, priority and
// max_per_run bounds.
func (d Definition) Validate(env Env) error {
	if !ValidName(d.Name) {
		return errName
	}
	if !utf8.ValidString(d.Description) {
		return errDescriptionUTF8
	}
	if utf8.RuneCountInString(d.Description) > MaxDescription {
		return errDescriptionLong
	}
	for _, a := range d.Accounts {
		if _, ok := env.Accounts[a]; !ok {
			return errAccountUnknown
		}
	}
	if err := checkQuery(d.Query, env); err != nil {
		return err
	}
	if err := d.Schedule.check(); err != nil {
		return err
	}
	if len(d.Actions) == 0 {
		return errNoActions
	}
	if len(d.Actions) > MaxActions {
		return errTooManyActions
	}
	for _, a := range d.Actions {
		if err := a.checkArguments(); err != nil {
			return err
		}
	}
	if err := checkOrder(d.Actions); err != nil {
		return err
	}
	if err := d.checkLabels(env); err != nil {
		return err
	}
	if err := checkRecipients(d.Actions, env.MaxRecipients); err != nil {
		return err
	}
	if d.Priority < MinPriority || d.Priority > MaxPriority {
		return errPriority
	}
	if d.MaxPerRun < 1 || d.MaxPerRun > MaxMaxPerRun {
		return errMaxPerRun
	}
	return nil
}

// CoveredAccounts is Accounts when set, else every account in env, sorted.
func (d Definition) CoveredAccounts(env Env) []string {
	if len(d.Accounts) > 0 {
		out := slices.Clone(d.Accounts)
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Sorted(maps.Keys(env.Accounts))
}

// checkQuery parses q and checks its account: terms. A syntax error reports
// only the column and the parser's expectation up to its first semicolon:
// the parser's message, and what some expectations add after a semicolon,
// repeat query text.
func checkQuery(q string, env Env) error {
	n, err := query.Parse(q)
	if err != nil {
		var se *query.SyntaxError
		if !errors.As(err, &se) {
			return &InvalidError{Msg: "the query does not parse", Hint: queryHint}
		}
		return querySyntaxError(se)
	}
	scope, _ := query.Split(n)
	for _, a := range scope.Accounts {
		if _, ok := env.Accounts[a]; !ok {
			return errQueryAccount
		}
	}
	return nil
}

// querySyntaxError turns a parser error into fixed text: the column, and
// the expectation cut at its first semicolon.
func querySyntaxError(se *query.SyntaxError) *InvalidError {
	msg := "the query does not parse"
	if se.Pos > 0 {
		msg = fmt.Sprintf("the query does not parse at column %d", se.Pos)
	}
	hint := queryHint
	if want, _, _ := strings.Cut(se.Expected, ";"); want != "" {
		hint = "expected " + want
	}
	return &InvalidError{Msg: msg, Hint: hint}
}

// argSet is a set of an action's argument fields.
type argSet uint8

// The argument fields.
const (
	argLabel argSet = 1 << iota
	argFolder
	argItem
	argAnchor
	argTo
	argNote
)

// args is the set of argument fields k takes.
func (k ActionKind) args() argSet {
	switch k {
	case ActLabel, ActUnlabel:
		return argLabel
	case ActCopy, ActMove:
		return argFolder
	case ActLinkItem:
		return argItem | argAnchor
	case ActForward:
		return argTo | argNote
	default:
		return 0
	}
}

// setArgs is the set of argument fields a sets.
func (a Action) setArgs() argSet {
	var s argSet
	if a.Label != "" {
		s |= argLabel
	}
	if a.Folder != "" {
		s |= argFolder
	}
	if a.ItemID != "" {
		s |= argItem
	}
	if a.Anchor {
		s |= argAnchor
	}
	if len(a.To) > 0 {
		s |= argTo
	}
	if a.Note != "" {
		s |= argNote
	}
	return s
}

// checkArguments checks the kind is known, it sets no field its kind does
// not take, and its arguments are well formed. Forward recipients are
// checked later, by checkRecipients.
func (a Action) checkArguments() error {
	if !a.Kind.Valid() {
		return errActionKind
	}
	if a.setArgs()&^a.Kind.args() != 0 {
		return errStrayArgument
	}
	switch a.Kind {
	case ActLabel, ActUnlabel:
		if !validFolderName(a.Label) {
			return errLabelName
		}
	case ActCopy, ActMove:
		if !validFolderName(a.Folder) {
			return errFolderName
		}
	case ActLinkItem:
		if !items.ValidID(a.ItemID) {
			return errLinkItem
		}
	case ActForward:
		if !utf8.ValidString(a.Note) {
			return errNoteUTF8
		}
		if utf8.RuneCountInString(a.Note) > MaxNote {
			return errNoteLong
		}
	}
	return nil
}

// validFolderName reports whether s is 1 to MaxFolderName bytes of UTF-8
// with no control characters.
func validFolderName(s string) bool {
	return s != "" && len(s) <= MaxFolderName && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// checkOrder checks there is at most one relocating action and it is last,
// then at most one forward and at most one copy.
func checkOrder(as []Action) error {
	var relocations, forwards, copies int
	for _, a := range as {
		switch {
		case a.Kind.Relocates():
			relocations++
		case a.Kind == ActForward:
			forwards++
		case a.Kind == ActCopy:
			copies++
		}
	}
	switch {
	case relocations > 1:
		return errTwoRelocations
	case relocations == 1 && !as[len(as)-1].Kind.Relocates():
		return errRelocationLast
	case forwards > 1:
		return errTwoForwards
	case copies > 1:
		return errTwoCopies
	}
	return nil
}

// checkLabels refuses label and unlabel unless every covered account is a
// Google account.
func (d Definition) checkLabels(env Env) error {
	if !slices.ContainsFunc(d.Actions, func(a Action) bool { return a.Kind == ActLabel || a.Kind == ActUnlabel }) {
		return nil
	}
	for _, name := range d.CoveredAccounts(env) {
		if !env.Accounts[name].Gmail {
			return errLabelNotGmail
		}
	}
	return nil
}

// checkRecipients checks each forward has 1 to maxRecipients addresses
// (below 1 reads as 1), each one mailaddr.Safe.
func checkRecipients(as []Action, maxRecipients int) error {
	maxRecipients = max(maxRecipients, 1)
	for _, a := range as {
		if a.Kind != ActForward {
			continue
		}
		if len(a.To) == 0 {
			return errNoRecipients
		}
		if len(a.To) > maxRecipients {
			return errTooRecipients
		}
		for _, r := range a.To {
			if !mailaddr.Safe(r) {
				return errRecipient
			}
		}
	}
	return nil
}

// Refusals of a schedule built without ParseSchedule. The text is fixed.
var (
	errScheduleKind  = &InvalidError{Msg: "the schedule kind is not known", Hint: scheduleHint}
	errScheduleRange = &InvalidError{Msg: "the at: time is outside 00:00 to 23:59", Hint: scheduleHint}
)

// check reports whether s is within its kind's bounds.
func (s Schedule) check() error {
	switch s.Kind {
	case ScheduleNone:
		return nil
	case ScheduleEvery:
		return checkEvery(s.Every)
	case ScheduleAt:
		if !s.At.valid() {
			return errScheduleRange
		}
		return nil
	default:
		return errScheduleKind
	}
}
