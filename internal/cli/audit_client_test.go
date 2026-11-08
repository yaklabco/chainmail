package cli

import "testing"

// --client asks the daemon for one client kind's rows; without it the
// request names none.
func TestAuditClientFilter(t *testing.T) {
	t.Parallel()
	f := &fakeAdmin{}
	socket := startFakeAdmin(t, f)
	if got := runCLI(t, Env{}, "--socket", socket, "audit", "--client", "rule"); got.code != 0 {
		t.Fatalf("audit --client rule: %+v", got)
	}
	if c := f.last(t).GetClient(); c != "rule" {
		t.Errorf("client = %q, want rule", c)
	}
	if got := runCLI(t, Env{}, "--socket", socket, "audit"); got.code != 0 {
		t.Fatalf("audit: %+v", got)
	}
	if c := f.last(t).GetClient(); c != "" {
		t.Errorf("default client = %q, want every client", c)
	}
}
