package cache

import (
	"testing"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

func TestSearchUIDs(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	var rows []Row
	for uid := uint32(1); uid <= 6; uid++ {
		rows = append(rows, testRow("INBOX", uid))
	}
	stale := testRow("INBOX", 7)
	stale.UIDValidity = 6
	other := testRow("INBOX", 8)
	other.Account = "other"
	putRows(t, s, append(rows, stale, other, testRow("Sent", 9))...)
	putBody(t, s, testRef("INBOX", 2), rawMessage("Report 2", "zebra"), false)
	putBody(t, s, testRef("INBOX", 5), rawMessage("Report 5", "zebra"), false)
	src := mailstore.Source{Folder: "INBOX", UIDValidity: testUIDValidity}
	big := Where{SQL: "m.size > ?", Args: []any{1002}}
	tests := []struct {
		name      string
		w         Where
		below     uint32
		want      []uint32
		wantCount uint32
	}{
		{"everything", Where{}, 0, []uint32{6, 5, 4, 3, 2, 1}, 6},
		{"below", Where{}, 4, []uint32{3, 2, 1}, 3},
		{"sql", big, 0, []uint32{6, 5, 4, 3}, 4},
		{"sql and below", big, 5, []uint32{4, 3}, 2},
		{"fts", Where{FTS: `"zebra"`}, 0, []uint32{5, 2}, 2},
		{"sql and fts", Where{SQL: "m.size > ?", Args: []any{1003}, FTS: `"zebra"`}, 0, []uint32{5}, 1},
		{"sql and fts and below", Where{SQL: "m.size > ?", Args: []any{1003}, FTS: `"zebra"`}, 5, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uids, n, err := s.SearchUIDs(t.Context(), testAccount, src, tt.w, tt.below, false)
			if err != nil {
				t.Fatal(err)
			}
			wantUIDs(t, "UIDs", uids, tt.want...)
			if n != tt.wantCount {
				t.Errorf("count = %d, want %d", n, tt.wantCount)
			}
			uids, n, err = s.SearchUIDs(t.Context(), testAccount, src, tt.w, tt.below, true)
			if err != nil || uids != nil || n != tt.wantCount {
				t.Errorf("countOnly = %v, %d, %v; want nil, %d", uids, n, err, tt.wantCount)
			}
		})
	}
	if _, _, err := s.SearchUIDs(t.Context(), testAccount, src, Where{FTS: "AND AND"}, 0, false); err == nil {
		t.Error("a malformed MATCH expression succeeded")
	}
}
