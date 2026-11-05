package fake

import (
	"fmt"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// Malformation is how the fake answers a fetch of one message, for tests of
// messages whose FETCH response imapstore cannot parse.
type Malformation int

const (
	// WellFormed is a message read whole, the default.
	WellFormed Malformation = iota
	// BadStructure is a message whose body structure cannot be parsed.
	// imapstore reads it again without one, so FetchHeaders, FetchSummaries
	// and a sync pass return it with AttachmentsUnknown set, HasAttachment
	// false and no attachments.
	BadStructure
	// BadEnvelope is a message that cannot be read at all. FetchHeaders and
	// FetchSummaries leave it out and return what they read with a
	// *mailstore.UnreadableError naming it. A sync pass lists it in
	// Delta.Unreadable instead of Delta.New.
	BadEnvelope
)

// Malform sets how fetches answer for the message at uid in folder, in
// every folder it is in. It panics when there is no such message.
func (s *Store) Malform(folder string, uid uint32, how Malformation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.findLocked(folder, uid)
	if m == nil {
		panic(fmt.Sprintf("fake: no message %d in %q", uid, folder))
	}
	if s.malformed == nil {
		s.malformed = map[*Message]Malformation{}
	}
	if how == WellFormed {
		delete(s.malformed, m)
		return
	}
	s.malformed[m] = how
}

// AddMalformed adds m, as Add does, and sets how fetches answer for it in
// the same step, so no pass can read it whole first.
func (s *Store) AddMalformed(m Message, how Malformation) ref.Ref {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, r := s.addLocked(m)
	if how != WellFormed {
		if s.malformed == nil {
			s.malformed = map[*Message]Malformation{}
		}
		s.malformed[stored] = how
	}
	return r
}

// SetMalformedBatch makes a FetchHeaders or FetchSummaries call that asks
// for n UIDs or more, and holds a BadEnvelope message, fail with a plain
// mailstore.ErrMalformed beside what it read, as imapstore's does once its
// hunt for the malformed messages runs out of logins. imapstore always
// isolates a fetch of mailstore.IsolatedBatch UIDs or fewer, so n must be
// larger: SetMalformedBatch panics otherwise. 0, the default, turns it
// off. A fault in the folder, which fails every fetch with a plain
// ErrMalformed, is Fail("FetchHeaders", err).
func (s *Store) SetMalformedBatch(n int) {
	if n > 0 && n <= mailstore.IsolatedBatch {
		panic(fmt.Sprintf("fake: imapstore isolates every fetch of %d UIDs or fewer; SetMalformedBatch(%d) cannot happen", mailstore.IsolatedBatch, n))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.malformedBatch = n
}

// degradeLocked reports how m reads: false when its envelope is bad, so it
// is unreadable. Otherwise it clears what a bad body structure loses from
// sum.
func (s *Store) degradeLocked(m *Message, sum *mailstore.Summary) bool {
	switch s.malformed[m] {
	case BadEnvelope:
		return false
	case BadStructure:
		sum.AttachmentsUnknown, sum.HasAttachment = true, false
	}
	return true
}

// unreadableErrLocked is the error of a fetch of asked UIDs in folder that
// could not read unreadable: nil when it read everything, a plain
// mailstore.ErrMalformed under SetMalformedBatch, and otherwise a
// *mailstore.UnreadableError. Either way the fetch returns what it read.
func (s *Store) unreadableErrLocked(folder string, asked int, unreadable []uint32) error {
	switch {
	case len(unreadable) == 0:
		return nil
	case s.malformedBatch > 0 && asked >= s.malformedBatch:
		return fmt.Errorf("%w: %d messages in %q", mailstore.ErrMalformed, asked, folder)
	default:
		return &mailstore.UnreadableError{Folder: folder, UIDs: unreadable}
	}
}

// malformedNewLocked reports whether the pass d on folder met new mail with
// a malformation, read without its body structure or not at all.
func (s *Store) malformedNewLocked(folder string, d mailstore.Delta) bool {
	if len(d.Unreadable) > 0 {
		return true
	}
	for _, sum := range d.New {
		if m := s.findLocked(folder, sum.Ref.UID); m != nil && s.malformed[m] != WellFormed {
			return true
		}
	}
	return false
}
