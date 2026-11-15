package system

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"syscall"
)

// runAsEnv is the whole environment of a command RunAs starts. Nothing is
// inherited from the caller, who is root under sudo: the chainmail CLI
// reads cli.toml from HOME or XDG_CONFIG_HOME and its settings from
// CHAINMAIL_*, and none of root's may leak into the daemon user's run.
var runAsEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/var/empty", "LANG=C"}

// RunAs runs name with args as uid and gid (with no supplementary groups
// beyond gid), with the given stdio. Real only; the apply step injects it.
// The caller must be root to change to another user. A nil stdin reads
// from the null device and a nil stdout or stderr discards. The command
// gets a fixed minimal environment, not the caller's.
func RunAs(ctx context.Context, uid, gid uint32, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	if err := runAsCmd(ctx, uid, gid, stdin, stdout, stderr, name, args...).Run(); err != nil {
		return fmt.Errorf("running %s as %d:%d: %w", name, uid, gid, err)
	}
	return nil
}

// runAsCmd builds the command RunAs runs.
func runAsCmd(ctx context.Context, uid, gid uint32, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append([]string(nil), runAsEnv...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{gid}},
	}
	return cmd
}
