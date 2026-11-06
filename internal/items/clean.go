package items

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jamesainslie/chainmail/internal/idempotency"
)

// Limits on item text, and how long item_requests rows are kept. Runes are
// counted for every text but the description, which is bounded in bytes.
const (
	MaxTitle         = 200   // runes
	MaxDescription   = 65536 // bytes
	MaxCriterion     = 500   // runes
	MaxEvidence      = 1000  // runes
	MaxNote          = 500   // runes
	RequestRetention = 7 * 24 * time.Hour
)

// CleanTitle trims white space from both ends of s and checks it is 1 to
// MaxTitle characters of valid UTF-8.
func CleanTitle(s string) (string, error) {
	return checkText(strings.TrimSpace(s), "title", true, MaxTitle, "a title is 1 to 200 characters")
}

// CleanDescription trims trailing white space only, since leading
// indentation means something in Markdown, and checks s is at most
// MaxDescription bytes of valid UTF-8. An empty description is allowed.
func CleanDescription(s string) (string, error) {
	const hint = "a description is at most 65536 bytes"
	s = strings.TrimRightFunc(s, unicode.IsSpace)
	if !utf8.ValidString(s) {
		return "", &InvalidError{Msg: "the description is not valid UTF-8", Hint: hint}
	}
	if len(s) > MaxDescription {
		return "", &InvalidError{Msg: fmt.Sprintf("the description is longer than %d bytes", MaxDescription), Hint: hint}
	}
	return s, nil
}

// CleanCriterion trims white space from both ends of s and checks it is 1
// to MaxCriterion characters of valid UTF-8.
func CleanCriterion(s string) (string, error) {
	return checkText(strings.TrimSpace(s), "criterion", true, MaxCriterion, "a criterion is 1 to 500 characters")
}

// CleanEvidence trims white space from both ends of s and checks it is 1 to
// MaxEvidence characters of valid UTF-8. Ticking a criterion requires it.
func CleanEvidence(s string) (string, error) {
	return checkText(strings.TrimSpace(s), "evidence", true, MaxEvidence,
		"ticking a criterion needs evidence of 1 to 1000 characters")
}

// CleanNote trims white space from both ends of s and checks it is at most
// MaxNote characters of valid UTF-8. An empty note is allowed.
func CleanNote(s string) (string, error) {
	return checkText(strings.TrimSpace(s), "note", false, MaxNote, "a note is at most 500 characters")
}

// checkText checks that s, already trimmed, is valid UTF-8 of at most
// maxRunes characters, and not empty when required. Its errors name the
// field and never repeat s.
func checkText(s, field string, required bool, maxRunes int, hint string) (string, error) {
	if !utf8.ValidString(s) {
		return "", &InvalidError{Msg: "the " + field + " is not valid UTF-8", Hint: hint}
	}
	n := utf8.RuneCountInString(s)
	switch {
	case required && n == 0:
		return "", &InvalidError{Msg: "the " + field + " is empty", Hint: hint}
	case n > maxRunes:
		return "", &InvalidError{Msg: fmt.Sprintf("the %s is longer than %d characters", field, maxRunes), Hint: hint}
	}
	return s, nil
}

// ValidKey reports whether k is a valid idempotency key: 1 to 128 bytes of
// [A-Za-z0-9._:-], the rule send requests use.
func ValidKey(k string) bool {
	return idempotency.ValidKey(k)
}
