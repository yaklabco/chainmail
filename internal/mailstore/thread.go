package mailstore

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// RootMessageID returns the Message-ID a thread is named after: the first
// entry of References, else the first In-Reply-To, else the message's own
// Message-ID. Angle brackets are dropped.
func RootMessageID(references, inReplyTo []string, messageID string) string {
	for _, ids := range [][]string{references, inReplyTo} {
		for _, id := range ids {
			if id = trimID(id); id != "" {
				return id
			}
		}
	}
	return trimID(messageID)
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
