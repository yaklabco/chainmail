package e2e

import (
	"testing"
	"time"
)

// A command whose own child keeps stdout open must not hold runBounded past
// its timeout: killing the command leaves the child writing to the pipe,
// and without a wait delay exec.Cmd.Wait blocks until the child exits. This
// is how `sudo security remove-trusted-cert` hung the macOS job.
func TestRunBoundedReturnsWhenAChildHoldsThePipe(t *testing.T) {
	t.Parallel()
	start := time.Now()
	_, _, err := runBounded(200*time.Millisecond, nil, "sh", "-c", "sleep 30 & wait")
	if err == nil {
		t.Fatal("runBounded returned no error for a command it had to kill")
	}
	if took := time.Since(start); took > waitDelay+5*time.Second {
		t.Fatalf("runBounded took %v, want at most the timeout plus %v", took, waitDelay)
	}
}

func TestRunBoundedOutput(t *testing.T) {
	t.Parallel()
	out, errOut, err := runBounded(5*time.Second, []byte("in"), "sh", "-c", "cat; echo err >&2")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "in" || string(errOut) != "err\n" {
		t.Fatalf("stdout %q, stderr %q", out, errOut)
	}
}
