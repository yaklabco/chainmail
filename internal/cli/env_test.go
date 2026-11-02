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

func TestEnvAfter(t *testing.T) {
	t.Parallel()
	if DefaultEnv().After == nil {
		t.Error("DefaultEnv has no After")
	}
	fired := make(chan time.Time, 1)
	fired <- time.Time{}
	var asked time.Duration
	env := Env{After: func(d time.Duration) <-chan time.Time {
		asked = d
		return fired
	}}
	<-env.after(42 * time.Second)
	if asked != 42*time.Second {
		t.Errorf("After got %v, want 42s", asked)
	}
	select {
	case <-(Env{}).after(0):
	case <-time.After(10 * time.Second):
		t.Fatal("after with no clock did not fire")
	}
}
