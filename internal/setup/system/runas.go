package system

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"syscall"
)

// RunAs runs name with args as uid and gid (with no supplementary groups
// beyond gid), with the given stdio. Real only; the apply step injects it.
// The caller must be root to change to another user. A nil stdin reads
// from the null device and a nil stdout or stderr discards.
func RunAs(ctx context.Context, uid, gid uint32, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{gid}},
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s as %d:%d: %w", name, uid, gid, err)
	}
	return nil
}
