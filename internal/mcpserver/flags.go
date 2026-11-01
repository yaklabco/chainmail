package mcpserver

import "strings"

// systemFlags maps the short names agents use to IMAP system flags.
var systemFlags = map[string]string{
	"seen": `\Seen`, "flagged": `\Flagged`, "answered": `\Answered`, "draft": `\Draft`,
}

// shortFlag names an IMAP flag for agents: \Seen is "seen". Keywords stay as they are.
func shortFlag(f string) string {
	if name, ok := strings.CutPrefix(f, `\`); ok {
		return strings.ToLower(name)
	}
	return f
}

func shortFlags(fs []string) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = shortFlag(f)
	}
	return out
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
