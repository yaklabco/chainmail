package setuptest

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/yaklabco/chainmail/internal/setup/system"
)

// startDelay is how long after a start or restart the fake daemon reports
// as its start time, so it is always later than a time read before.
const startDelay = time.Millisecond

// Service is a fake system.Service. A start sets StartedAt from Clock, so
// a Daemon built on it answers admin status the way a real one would.
type Service struct {
	// Clock gives start times. Nil means time.Now.
	Clock *Clock
	// Journal, when set, gets the same entries as Calls.
	Journal *Journal
	// Stuck leaves StartedAt where it was on a start or restart, as a
	// daemon that never comes up would.
	Stuck bool
	// Launchd models launchd instead of systemd: Activate(true) on a
	// loaded job unloads and loads it again, which restarts it.
	Launchd bool
	// Log is what Logs returns, cut to the last n lines.
	Log []string
	// StateErr, ActivateErr and RestartErr make those methods fail.
	StateErr, ActivateErr, RestartErr error

	mu        sync.Mutex
	loaded    bool
	running   bool
	startedAt time.Time
	starts    int
	calls     []string
}

var _ system.Service = (*Service)(nil)

// SetState sets whether the service is loaded and running. A running
// service gets a start time from the clock.
func (s *Service) SetState(st system.ServiceState) *Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded, s.running = st.Loaded || st.Running, st.Running
	if st.Running {
		s.startedAt = s.now()
	}
	return s
}

// Running reports whether the service runs now, and since when.
func (s *Service) Running() (bool, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running, s.startedAt
}

// Calls lists the calls so far: "state", "activate <changed>", "restart"
// and "logs <n>".
func (s *Service) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Starts counts the starts and restarts so far, however they came about:
// Restart, Activate on a stopped service, or Activate(true) on a loaded
// launchd job.
func (s *Service) Starts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

// State implements system.Service.
func (s *Service) State(context.Context) (system.ServiceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("state")
	if s.StateErr != nil {
		return system.ServiceState{}, s.StateErr
	}
	return system.ServiceState{Loaded: s.loaded, Running: s.running}, nil
}

// Activate implements system.Service: it loads the service and starts it
// when it is not running. Like systemd's enable --now, it never restarts
// a running service, unless Launchd is set: then a changed definition on
// a loaded job is booted out and bootstrapped again, which restarts it.
func (s *Service) Activate(_ context.Context, changed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("activate " + strconv.FormatBool(changed))
	if s.ActivateErr != nil {
		return s.ActivateErr
	}
	reload := s.Launchd && changed && s.loaded
	s.loaded = true
	if !s.running || reload {
		s.start()
	}
	return nil
}

// Restart implements system.Service.
func (s *Service) Restart(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("restart")
	if s.RestartErr != nil {
		return s.RestartErr
	}
	s.start()
	return nil
}

// Logs implements system.Service.
func (s *Service) Logs(_ context.Context, n int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record(fmt.Sprintf("logs %d", n))
	lines := s.Log
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var out []byte
	for _, l := range lines {
		out = append(out, l...)
		out = append(out, '\n')
	}
	return out, nil
}

// start marks the service running. The caller holds mu.
func (s *Service) start() {
	s.running = true
	s.starts++
	if s.Stuck {
		return
	}
	if s.Clock != nil {
		s.Clock.Advance(startDelay)
	}
	s.startedAt = s.now()
}

// now reads the clock. The caller holds mu.
func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// record notes a call. The caller holds mu.
func (s *Service) record(call string) {
	s.calls = append(s.calls, call)
	s.Journal.Add("service " + call)
}
