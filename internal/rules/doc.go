// Package rules holds chainmail's mail rules: the rule model, its
// validation, schedules, the stable keys that make a rule act on a message
// once, and the stages of a work item's due date.
//
// A rule matches mail with the search language and runs a short list of
// actions on each match. The package knows nothing about IMAP or the cache:
// the daemon matches and acts, and passes account facts in through Env.
//
// Rule descriptions, queries, folder and label names, forward addresses and
// notes are untrusted, since they are often copied from mail. None of them
// ever appears in an error, a hint or a log line, so every error text here
// is fixed.
package rules
