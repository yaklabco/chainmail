package system

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

// bootout returns before launchd has finished unloading the job, and a
// bootstrap in that window fails. Activate polls the state this many
// times, this far apart, before it gives up.
const (
	unloadPolls    = 50
	unloadInterval = 200 * time.Millisecond
)

// launchdService drives one launchd job in the system domain.
type launchdService struct {
	label   string
	path    string
	logFile string
	run     Runner
	files   Files
	sleep   func(time.Duration)
}

func (s *launchdService) target() string { return "system/" + s.label }

// launchctl print's exit status and message for a job launchd does not
// know.
const (
	launchctlNotFound    = 113
	launchctlNotFoundMsg = "Could not find service"
)

// State reads launchctl print. Only launchd saying it does not know the
// job means not loaded; any other failure, including a cancelled context
// or a missing launchctl, is an error.
func (s *launchdService) State(ctx context.Context) (ServiceState, error) {
	out, err := s.run(ctx, "launchctl", "print", s.target())
	if err != nil {
		if exitCode(err) == launchctlNotFound || strings.Contains(err.Error(), launchctlNotFoundMsg) {
			return ServiceState{}, nil
		}
		return ServiceState{}, fmt.Errorf("reading the state of %s: %w", s.label, err)
	}
	return ServiceState{
		Loaded:  true,
		Running: strings.Contains(string(out), "state = running"),
	}, nil
}

func (s *launchdService) Activate(ctx context.Context, changed bool) error {
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	switch {
	case !st.Loaded:
		return s.bootstrap(ctx)
	case changed:
		// kickstart -k does not reread the plist, so a changed one is
		// unloaded and loaded again.
		if _, err := s.run(ctx, "launchctl", "bootout", s.target()); err != nil {
			return fmt.Errorf("unloading %s: %w", s.label, err)
		}
		if err := s.waitUnloaded(ctx); err != nil {
			return err
		}
		return s.bootstrap(ctx)
	case !st.Running:
		if _, err := s.run(ctx, "launchctl", "kickstart", s.target()); err != nil {
			return fmt.Errorf("starting %s: %w", s.label, err)
		}
	}
	return nil
}

func (s *launchdService) bootstrap(ctx context.Context) error {
	if _, err := s.run(ctx, "launchctl", "bootstrap", "system", s.path); err != nil {
		return fmt.Errorf("loading %s: %w", s.path, err)
	}
	return nil
}

// waitUnloaded polls until launchd no longer knows the job.
func (s *launchdService) waitUnloaded(ctx context.Context) error {
	for range unloadPolls {
		st, err := s.State(ctx)
		if err != nil {
			return err
		}
		if !st.Loaded {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for %s to unload: %w", s.label, err)
		}
		s.sleep(unloadInterval)
	}
	return fmt.Errorf("%s was still loaded %s after bootout", s.label, unloadPolls*unloadInterval)
}

func (s *launchdService) Restart(ctx context.Context) error {
	if _, err := s.run(ctx, "launchctl", "kickstart", "-k", s.target()); err != nil {
		return fmt.Errorf("restarting %s: %w", s.label, err)
	}
	return nil
}

// Logs returns the tail of the daemon's log file. A log that does not
// exist yet is empty.
func (s *launchdService) Logs(_ context.Context, n int) ([]byte, error) {
	data, err := s.files.ReadFile(s.logFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.logFile, err)
	}
	return tail(data, n), nil
}
