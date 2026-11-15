package system

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// systemdService drives one systemd unit through systemctl and journalctl.
type systemdService struct {
	unit string
	run  Runner
}

func (s *systemdService) State(ctx context.Context) (ServiceState, error) {
	// systemctl prints the properties in its own order, which puts
	// LoadState before ActiveState whatever order they are asked in.
	out, err := s.run(ctx, "systemctl", "show", "--property=LoadState,ActiveState", "--value", s.unit)
	if err != nil {
		return ServiceState{}, fmt.Errorf("reading state of %s: %w", s.unit, err)
	}
	lines := strings.Fields(string(out))
	if len(lines) != 2 {
		return ServiceState{}, fmt.Errorf("reading state of %s: unexpected output %q", s.unit, out)
	}
	return ServiceState{
		Loaded:  lines[0] == "loaded",
		Running: lines[1] == "active",
	}, nil
}

func (s *systemdService) Activate(ctx context.Context, changed bool) error {
	if changed {
		if _, err := s.run(ctx, "systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("reloading systemd: %w", err)
		}
	}
	if _, err := s.run(ctx, "systemctl", "enable", "--now", s.unit); err != nil {
		return fmt.Errorf("starting %s: %w", s.unit, err)
	}
	return nil
}

func (s *systemdService) Restart(ctx context.Context) error {
	if _, err := s.run(ctx, "systemctl", "restart", s.unit); err != nil {
		return fmt.Errorf("restarting %s: %w", s.unit, err)
	}
	return nil
}

func (s *systemdService) Logs(ctx context.Context, n int) ([]byte, error) {
	out, err := s.run(ctx, "journalctl", "--unit", s.unit, "--lines", strconv.Itoa(n), "--no-pager", "--output", "cat")
	if err != nil {
		return nil, fmt.Errorf("reading the log of %s: %w", s.unit, err)
	}
	return out, nil
}
