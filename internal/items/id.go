package items

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"regexp"
	"time"
)

// The Admin items' fixed IDs. The daemon makes sure both exist at startup.
const (
	AdminWorkID     = "it_admin_work"
	AdminPersonalID = "it_admin_personal"
)

// idCrockford is Crockford's base32 alphabet in lowercase, as IDs use it:
// no i, l, o or u.
const idCrockford = "0123456789abcdefghjkmnpqrstvwxyz"

// itemIDRE is the item ID rule. internal/query keeps its own copy of the
// pattern, since it cannot import this package.
var itemIDRE = regexp.MustCompile(`^it_[0-9a-z_]{1,60}$`)

// ValidID matches ^it_[0-9a-z_]{1,60}$: every generated item ID and the
// Admin IDs. internal/query uses the same pattern.
func ValidID(id string) bool {
	return itemIDRE.MatchString(id)
}

// NewID returns prefix ("it_" or "cr_") plus a 26-character lowercase
// Crockford ULID: the low 48 bits of now in Unix milliseconds as ten
// characters, so IDs sort by creation time as text, then 80 random bits
// from rnd as sixteen characters. It reads exactly ten bytes from rnd. A
// nil rnd reads crypto/rand, and so does a rnd that fails or runs short.
func NewID(prefix string, now time.Time, rnd io.Reader) string {
	var id [26]byte
	ms := uint64(now.UnixMilli()) & (1<<48 - 1)
	for i := 9; i >= 0; i-- {
		id[i] = idCrockford[ms&31]
		ms >>= 5
	}
	if rnd == nil {
		rnd = rand.Reader
	}
	var r [10]byte
	if _, err := io.ReadFull(rnd, r[:]); err != nil {
		// crypto/rand.Read never fails and always fills r.
		_, _ = rand.Read(r[:])
	}
	// The 80 random bits are hi (the top 16) and lo (the low 64). Each turn
	// takes the lowest five bits and shifts the 80-bit value right by five.
	hi := uint64(binary.BigEndian.Uint16(r[:2]))
	lo := binary.BigEndian.Uint64(r[2:])
	for i := len(id) - 1; i >= 10; i-- {
		id[i] = idCrockford[lo&31]
		lo = lo>>5 | (hi&31)<<59
		hi >>= 5
	}
	return prefix + string(id[:])
}
