package watch

// cacheGroupPrefix starts every group name the daemon's cache reads the log
// through. Clients can never use such a name: ValidGroupName refuses it.
const cacheGroupPrefix = "cache."

// maxAccountName is the longest account name internal/config accepts.
const maxAccountName = 63

// CacheGroup returns the event log group the cache of account reads
// through: "cache." followed by the account name.
func CacheGroup(account string) string {
	return cacheGroupPrefix + account
}

// IsCacheGroup reports whether name is a cache group: "cache." followed by
// a valid account name, ^[a-z0-9][a-z0-9-]{0,62}$. A cache group name can
// be longer than ValidGroupName allows, since account names reach 63
// characters.
func IsCacheGroup(name string) bool {
	return isAccountGroup(name, cacheGroupPrefix)
}
