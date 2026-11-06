// Package items keeps chainmail's work items in state.db: the Lathe roadmap
// hierarchy (objective, key-result, initiative, epic, feature, story, bug
// and support-ticket) plus a task leaf, with acceptance criteria, blockers,
// soft delete, a global revision counter and links from items to mail
// threads. Every write appends item events to the watch log in the same
// transaction.
//
// The package knows nothing about IMAP: the daemon reads mail and passes
// thread keys and Message-IDs in.
//
// Item text (titles, descriptions, link notes, criterion text and evidence)
// is untrusted, since agents often copy it from mail. It never appears in an
// error, a hint, a log line or an audit target, so every error text here is
// fixed.
package items
