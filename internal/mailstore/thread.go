package mailstore

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// RootMessageID returns the Message-ID a thread is named after: the first
// entry of References, else the first In-Reply-To, else the message's own
// Message-ID. Angle brackets are dropped. Entries are read as MessageIDs
// reads them, so a whole raw References header passed as one string works too.
func RootMessageID(references, inReplyTo []string, messageID string) string {
	for _, entries := range [][]string{references, inReplyTo} {
		if ids := MessageIDs(entries); len(ids) > 0 {
			return ids[0]
		}
	}
	return trimID(messageID)
}

// MessageIDs returns the bare Message-IDs in References or In-Reply-To
// entries, in order. An entry may be one ID or a whole raw header. When it
// has angle brackets, each <...> span is an ID and the text around them,
// such as a comment or an old-style phrase, is dropped. Otherwise each word
// is an ID.
func MessageIDs(entries []string) []string {
	var out []string
	add := func(id string) {
		if id = trimID(id); id != "" {
			out = append(out, id)
		}
	}
	for _, e := range entries {
		if !strings.Contains(e, "<") {
			for _, f := range strings.Fields(e) {
				add(f)
			}
			continue
		}
		for {
			_, rest, ok := strings.Cut(e, "<")
			if !ok {
				break
			}
			id, after, _ := strings.Cut(rest, ">")
			add(id)
			e = after
		}
	}
	return out
}

func trimID(id string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(id), "<"), ">")
}

// ThreadKey returns a short stable thread ID for a root Message-ID, or ""
// when there is no root.
func ThreadKey(root string) string {
	if root == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:8])
}
