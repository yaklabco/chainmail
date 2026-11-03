package cache

import "testing"

func TestBudget(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	ctx := t.Context()
	for _, tt := range []struct {
		account, day string
		add, want    int64
	}{
		{testAccount, "2026-11-03", 2048, 2048},
		{testAccount, "2026-11-03", 1000, 3048},
		{testAccount, "2026-11-04", 10, 10},
		{"other", "2026-11-03", 5, 5},
		{testAccount, "2026-11-03", 0, 3048},
	} {
		got, err := s.AddBudget(ctx, tt.account, tt.day, tt.add)
		if err != nil || got != tt.want {
			t.Errorf("AddBudget(%s, %s, %d) = %d, %v; want %d", tt.account, tt.day, tt.add, got, err, tt.want)
		}
	}
	if got, err := s.Budget(ctx, testAccount, "2026-11-03"); err != nil || got != 3048 {
		t.Errorf("Budget = %d, %v; want 3048", got, err)
	}
	if got, err := s.Budget(ctx, testAccount, "2026-11-05"); err != nil || got != 0 {
		t.Errorf("Budget of an unspent day = %d, %v; want 0", got, err)
	}
	for _, day := range []string{"", "2026-11-3", "03/11/2026", "2026-11-03T00:00:00Z"} {
		if _, err := s.AddBudget(ctx, testAccount, day, 1); err == nil {
			t.Errorf("AddBudget day %q accepted", day)
		}
		if _, err := s.Budget(ctx, testAccount, day); err == nil {
			t.Errorf("Budget day %q accepted", day)
		}
	}
	if _, err := s.AddBudget(ctx, testAccount, "2026-11-03", -1); err == nil {
		t.Error("negative bytes accepted")
	}
}
