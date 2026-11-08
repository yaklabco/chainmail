package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// MessageKey is g:<gmail_msgid> when gmail and gmailMsgID != 0, else
// i:<uidvalidity>:<uid>:<folder>. It names a message in the act-once table
// the way the cache keys it, so a ref format change cannot break it. UIDs
// are per folder, so the IMAP form needs the folder.
func MessageKey(gmail bool, gmailMsgID uint64, folder string, uidValidity, uid uint32) string {
	if gmail && gmailMsgID != 0 {
		return "g:" + strconv.FormatUint(gmailMsgID, 10)
	}
	return "i:" + strconv.FormatUint(uint64(uidValidity), 10) + ":" + strconv.FormatUint(uint64(uid), 10) + ":" + folder
}

// ForwardKey is the idempotency key of a rule's forward of one message:
// "rule." + ruleID + "." + first 16 hex chars of sha256(account + "\x00" +
// messageKey). For a valid rule ID it is at most 85 bytes of the characters
// idempotency keys allow, and it holds no folder name or other rule text.
func ForwardKey(ruleID, account, messageKey string) string {
	sum := sha256.Sum256([]byte(account + "\x00" + messageKey))
	return "rule." + ruleID + "." + hex.EncodeToString(sum[:8])
}
