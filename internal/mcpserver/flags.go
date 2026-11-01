package mcpserver

import "strings"

// systemFlags maps the short names agents use to IMAP system flags.
var systemFlags = map[string]string{
	"seen": `\Seen`, "flagged": `\Flagged`, "answered": `\Answered`, "draft": `\Draft`,
}

// shownFlags are the system flags a result names by short name: \Deleted
// as "deleted", and the four of systemFlags.
var shownFlags = map[string]string{
	`\seen`: "seen", `\flagged`: "flagged", `\answered`: "answered", `\draft`: "draft", `\deleted`: "deleted",
}

// splitFlags sorts a message's IMAP flags for agents. A system flag of
// shownFlags, matched in any case, becomes its short name in flags.
// Everything else, a keyword such as $Forwarded or another flag a server
// or a sender's client set, stays as the server has it, in keywords: these
// are not restricted to a fixed vocabulary, so they are untrusted. flags
// is never nil, so an output always holds the array.
func splitFlags(fs []string) (flags []string, keywords []untrusted) {
	flags = []string{}
	for _, f := range fs {
		if name, ok := shownFlags[strings.ToLower(f)]; ok {
			flags = append(flags, name)
			continue
		}
		keywords = append(keywords, serverName(f))
	}
	return flags, keywords
}

// imapFlag turns a short system flag name, in any case, into the IMAP flag.
// Anything else, such as a keyword or \Seen itself, is passed as given and
// the daemon validates it.
func imapFlag(s string) string {
	if f, ok := systemFlags[strings.ToLower(s)]; ok {
		return f
	}
	return s
}
