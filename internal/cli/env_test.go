package cli

import (
	"testing"
	"time"
)

func TestEnvClock(t *testing.T) {
	t.Parallel()
	if DefaultEnv().Now == nil {
		t.Error("DefaultEnv has no clock")
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := (Env{Now: func() time.Time { return at }}).now(); !got.Equal(at) {
		t.Errorf("now() = %v, want %v", got, at)
	}
	if got := (Env{}).now(); time.Since(got) > time.Minute || time.Until(got) > time.Minute {
		t.Errorf("now() with no clock = %v, want about the real time", got)
	}
}
