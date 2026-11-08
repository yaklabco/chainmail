package watch

import "strings"

// rulesGroupPrefix starts every group name a daemon rule runner reads the
// log through. Clients can never use such a name: ValidGroupName refuses it.
const rulesGroupPrefix = "rules."

// RulesGroup returns the event log group the rule runner of account reads
// through: "rules." followed by the account name.
func RulesGroup(account string) string {
	return rulesGroupPrefix + account
}

// IsRulesGroup reports whether name is a rules group: "rules." followed by
// a valid account name, ^[a-z0-9][a-z0-9-]{0,62}$. Like a cache group, a
// rules group name can be longer than ValidGroupName allows.
func IsRulesGroup(name string) bool {
	return isAccountGroup(name, rulesGroupPrefix)
}

// isAccountGroup reports whether name is prefix followed by a valid account
// name, ^[a-z0-9][a-z0-9-]{0,62}$, as internal/config accepts it.
func isAccountGroup(name, prefix string) bool {
	account, ok := strings.CutPrefix(name, prefix)
	if !ok || account == "" || len(account) > maxAccountName {
		return false
	}
	for i := range len(account) {
		c := account[i]
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		case i > 0 && c == '-':
		default:
			return false
		}
	}
	return true
}

// logGroupName reports whether the Log takes name as a group: a client
// group, one of the cache's, or one of the rule runners'.
func logGroupName(name string) bool {
	return ValidGroupName(name) || IsCacheGroup(name) || IsRulesGroup(name)
}
