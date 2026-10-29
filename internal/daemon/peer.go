package daemon

import (
	"context"
	"net"
	"net/http"
	"os/user"
	"slices"
	"strconv"
)

// Peer identifies the process on the other end of a socket connection.
type Peer struct {
	UID uint32
	GID uint32
	PID int32
}

type peerKey struct{}

// connContext attaches the peer's credentials to every request on c.
// http.Server calls it once per connection. If the credentials can't be read,
// the context has no peer and the authorizer refuses the request.
func connContext(ctx context.Context, c net.Conn) context.Context {
	p, err := peerCredentials(c)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, peerKey{}, p)
}

// PeerFromContext returns the peer of the connection a request arrived on.
func PeerFromContext(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(Peer)
	return p, ok
}

// Authorizer decides which local users may use the daemon.
type Authorizer struct {
	selfUID  uint32
	gid      int
	groupsOf func(uid uint32) ([]string, error)
}

// NewAuthorizer allows selfUID, plus members of group gid when gid is not negative.
func NewAuthorizer(selfUID uint32, gid int) *Authorizer {
	return &Authorizer{selfUID: selfUID, gid: gid, groupsOf: lookupGroups}
}

func lookupGroups(uid uint32) ([]string, error) {
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return nil, err
	}
	return u.GroupIds()
}

// Allowed reports whether p may use the daemon. Group membership is looked up
// on every call, so adding a user to the group takes effect without a restart.
func (a *Authorizer) Allowed(p Peer) bool {
	if p.UID == a.selfUID {
		return true
	}
	if a.gid < 0 {
		return false
	}
	if int64(p.GID) == int64(a.gid) {
		return true
	}
	groups, err := a.groupsOf(p.UID)
	if err != nil {
		return false
	}
	return slices.Contains(groups, strconv.Itoa(a.gid))
}

// Middleware refuses requests from peers that aren't allowed.
func (a *Authorizer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PeerFromContext(r.Context())
		if !ok || !a.Allowed(p) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
