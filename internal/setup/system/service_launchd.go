package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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

// The launchd log is read from its end in chunks of logChunk bytes, and
// never more than maxLogTail bytes in all, however large the file.
const (
	logChunk   = 8 << 10
	maxLogTail = 64 << 10
)

// Logs returns the last n lines of the daemon's log file, read from the
// end in bounded chunks. A log that does not exist yet is empty. A final
// symlink and anything but a regular file are refused, and a FIFO is
// refused without blocking.
func (s *launchdService) Logs(ctx context.Context, n int) ([]byte, error) {
	f, err := os.OpenFile(s.logFile, os.O_RDONLY|noFollow|nonBlock, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", s.logFile, err)
	}
	defer func() { _ = f.Close() }() // read-only; nothing to flush
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", s.logFile, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", s.logFile)
	}
	out, err := tailLog(ctx, f, fi.Size(), n)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.logFile, err)
	}
	return out, nil
}

// tailLog returns the last n lines of the size bytes in r. It reads
// backwards in chunks of logChunk, checking ctx before each, until it has
// n whole lines, reaches the start or has read maxLogTail bytes. Lines
// that do not fit in maxLogTail are cut to the whole lines that do, or
// to the last maxLogTail bytes when not even one fits.
func tailLog(ctx context.Context, r io.ReaderAt, size int64, n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	var buf []byte
	off := size
	for off > 0 && len(buf) < maxLogTail && !hasLines(buf, n) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := min(int64(logChunk), off, int64(maxLogTail-len(buf)))
		off -= chunk
		p := make([]byte, chunk)
		if read, err := r.ReadAt(p, off); err != nil && (read < len(p) || !errors.Is(err, io.EOF)) {
			return nil, err
		}
		buf = append(p, buf...)
	}
	if off > 0 && !hasLines(buf, n) {
		// buf starts part way through a line; keep only whole lines.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 && i < len(buf)-1 {
			buf = buf[i+1:]
		}
	}
	return tail(buf, n), nil
}

// hasLines reports whether buf holds the newline before the last n lines,
// so they are whole. A final newline ends the last line.
func hasLines(buf []byte, n int) bool {
	count := bytes.Count(buf, []byte{'\n'})
	if len(buf) > 0 && buf[len(buf)-1] == '\n' {
		count--
	}
	return count >= n
}
