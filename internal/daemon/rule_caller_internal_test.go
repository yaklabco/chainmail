package daemon

import (
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/yaklabco/chainmail/internal/api"
)

// TestCallerOfReservesTheDaemonsKinds checks that a socket peer cannot
// claim the client kinds the daemon records its own actions under, so
// audit --client rule shows only what rules did. Kinds that only start
// like a reserved one stay as given.
func TestCallerOfReservesTheDaemonsKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind, want string
	}{
		{kind: "rule", want: invalidTarget},
		{kind: "daemon", want: invalidTarget},
		{kind: "cli", want: "cli"},
		{kind: "mcp", want: "mcp"},
		{kind: "rule-x", want: "rule-x"},
		{kind: "rules", want: "rules"},
		{kind: "daemon.test", want: "daemon.test"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			h.Set(api.HeaderClient, tt.kind)
			c, err := callerOf(peerContext(t.Context()), h)
			if err != nil {
				t.Fatal(err)
			}
			if c.client != tt.want || c.uid != uint32(os.Getuid()) {
				t.Errorf("caller = %+v, want client %q and the peer's uid", c, tt.want)
			}
		})
	}
}

// TestRuleCaller pins who a rule's actions run as: the daemon's own uid
// with client kind rule, and the item actor that goes with it.
func TestRuleCaller(t *testing.T) {
	t.Parallel()
	c := ruleCaller()
	if c.uid != uint32(os.Getuid()) || c.client != "rule" || ruleClient != "rule" {
		t.Errorf("ruleCaller() = %+v, ruleClient %q; want uid %d, client rule", c, ruleClient, os.Getuid())
	}
	if got, want := actorOf(c).String(), fmt.Sprintf("%d/rule", os.Getuid()); got != want {
		t.Errorf("actor = %q, want %q", got, want)
	}
}
