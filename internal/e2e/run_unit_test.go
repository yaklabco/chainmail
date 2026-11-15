package e2e

import (
	"errors"
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

// dscl reports a missing record with eDSRecordNotFound (DS Error -14136)
// and a non-zero exit; any other failure is an error, not an absence.
func TestDSCLRecordExists(t *testing.T) {
	t.Parallel()
	failed := errors.New("exit status 56")
	cases := []struct {
		name    string
		stderr  string
		err     error
		want    bool
		wantErr bool
	}{
		{name: "found", want: true},
		{name: "not found by name", stderr: "<dscl_cmd> DS Error: -14136 (eDSRecordNotFound)\n", err: failed},
		{name: "not found by code only", stderr: "DS Error: -14136\n", err: failed},
		{name: "other failure", stderr: "DS Error: -14009 (eDSUnknownNodeName)\n", err: failed, wantErr: true},
		{name: "no output", err: failed, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := dsclRecordExists([]byte(tc.stderr), tc.err)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %v, %v; want %v, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
