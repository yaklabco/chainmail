package cache

import (
	"maps"
	"testing"

	"github.com/yaklabco/chainmail/internal/mailstore"
)

// BodyStates reports each cached message's body as held, pending or
// finally failed, and leaves out UIDs with no row in the source.
func TestBodyStates(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	var rows []Row
	for uid := uint32(1); uid <= 6; uid++ {
		rows = append(rows, testRow("INBOX", uid))
	}
	stale := testRow("INBOX", 7)
	stale.UIDValidity = 6
	putRows(t, s, append(rows, stale)...)
	fail := func(uid uint32, n int) {
		t.Helper()
		for range n {
			if _, err := s.RecordBodyFailure(t.Context(), testRef("INBOX", uid), failFetch); err != nil {
				t.Fatalf("RecordBodyFailure(%d): %v", uid, err)
			}
		}
	}
	putBody(t, s, testRef("INBOX", 1), rawMessage("Report 1", "held"), false)
	fail(2, 1)
	fail(3, finalAttempts-1)
	fail(4, finalAttempts)
	fail(5, finalAttempts)
	putBody(t, s, testRef("INBOX", 5), rawMessage("Report 5", "fetched by a read at last"), false)
	// UID 6 has a row and no body or failure: pending.

	ctx := t.Context()
	src := mailstore.Source{Folder: "INBOX", UIDValidity: testUIDValidity}
	got, err := s.BodyStates(ctx, testAccount, src, []uint32{1, 2, 3, 4, 5, 6, 7, 99})
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint32]BodyState{1: BodyHeld, 2: BodyPending, 3: BodyPending, 4: BodyFailed, 5: BodyHeld, 6: BodyPending}
	if !maps.Equal(got, want) {
		t.Errorf("BodyStates = %v, want %v (UID 7 is cached only under UIDVALIDITY 6, 99 not at all)", got, want)
	}

	older := mailstore.Source{Folder: "INBOX", UIDValidity: 6}
	if got, err := s.BodyStates(ctx, testAccount, older, []uint32{7}); err != nil || !maps.Equal(got, map[uint32]BodyState{7: BodyPending}) {
		t.Errorf("BodyStates under UIDVALIDITY 6 = %v, %v; want UID 7 pending", got, err)
	}
	if got, err := s.BodyStates(ctx, "other", src, []uint32{1, 2}); err != nil || len(got) != 0 {
		t.Errorf("BodyStates of another account = %v, %v; want none", got, err)
	}
	if got, err := s.BodyStates(ctx, testAccount, src, nil); err != nil || len(got) != 0 {
		t.Errorf("BodyStates of no UIDs = %v, %v; want none", got, err)
	}
}
