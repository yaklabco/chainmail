// Package calendar is chainmail's calendar core. It holds the calendar
// model shared by the calendar stores, the calendar cache and the daemon,
// and the pure functions on it: iCalendar (RFC 5545) parsing and encoding,
// recurrence expansion, iTIP (RFC 5546) replies, mail invite parsing, event
// references and free/busy merging.
//
// The package does no IO. It knows nothing about Google, CalDAV, SQLite or
// IMAP.
//
// Event text (summaries, descriptions, locations, organizer and attendee
// names, reply comments and calendar display names) is untrusted: it comes
// from whoever sent the invite. It never appears in an error, so every error
// text here is fixed or carries only field names. Malformed input is
// reported as ErrInvalidICS and is never echoed back.
//
// Every instant in an Event is UTC. Event.TZID names the IANA zone a timed
// event was written in, so recurrence expansion keeps local wall time across
// daylight saving changes.
package calendar
