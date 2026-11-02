package daemon

import (
	"testing"
	"time"
)

// TestWatchServerDefaults checks the watch fields New fills in: the 30
// second heartbeat unless Options sets one, the event log and folder state,
// and a ReadEvents wait unit of one second.
func TestWatchServerDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		set      time.Duration
		wantBeat time.Duration
	}{
		{name: "zero means 30s", set: 0, wantBeat: 30 * time.Second},
		{name: "set", set: 20 * time.Millisecond, wantBeat: 20 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestServerOptions(t, passwordConfig, Options{WatchHeartbeat: tt.set})
			if s.opts.WatchHeartbeat != tt.wantBeat {
				t.Errorf("heartbeat = %v, want %v", s.opts.WatchHeartbeat, tt.wantBeat)
			}
			if s.watchLog == nil || s.watchState == nil {
				t.Error("watch log or state not built")
			}
			if s.watchWaitUnit != time.Second {
				t.Errorf("wait unit = %v, want 1s", s.watchWaitUnit)
			}
		})
	}
}
