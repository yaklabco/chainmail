package system

import (
	"bytes"
	"context"
	"fmt"
	"time"
)

// NewService returns the Service for goos. label is the unit name or
// launchd label, path the definition file, logFile the launchd log ("" on
// Linux). The launchd log is read straight from the file, from its end.
func NewService(goos, label, path, logFile string, run Runner) Service {
	switch goos {
	case "linux":
		return &systemdService{unit: label, run: run}
	case "darwin":
		return &launchdService{
			label:   label,
			path:    path,
			logFile: logFile,
			run:     run,
			sleep:   time.Sleep,
		}
	default:
		return unsupportedService{goos: goos}
	}
}

// tail returns the last n lines of data. A final line without a newline
// counts as a line.
func tail(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return nil
	}
	end := len(data)
	if data[end-1] == '\n' {
		end--
	}
	start := end
	for range n {
		i := bytes.LastIndexByte(data[:start], '\n')
		if i < 0 {
			return data
		}
		start = i
	}
	return data[start+1:]
}

type unsupportedService struct{ goos string }

func (u unsupportedService) err() error {
	return fmt.Errorf("service on %s: %w", u.goos, errUnsupported)
}

func (u unsupportedService) State(context.Context) (ServiceState, error) {
	return ServiceState{}, u.err()
}
func (u unsupportedService) Activate(context.Context, bool) error      { return u.err() }
func (u unsupportedService) Restart(context.Context) error             { return u.err() }
func (u unsupportedService) Logs(context.Context, int) ([]byte, error) { return nil, u.err() }
