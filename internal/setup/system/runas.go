package system

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"syscall"
)

// runAsEnv is the whole environment of a command RunAs starts. Nothing is
// inherited from the caller, who is root under sudo: the chainmail CLI
// reads cli.toml from HOME or XDG_CONFIG_HOME and its settings from
// CHAINMAIL_*, and none of root's may leak into the daemon user's run.
var runAsEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/var/empty", "LANG=C"}

// RunAs runs name with args as uid and gid (with no supplementary groups
// beyond gid), with the given stdio. Real only; the apply step injects it.
// The caller must be root to change to another user. name is an absolute
// path, run as it is, or a bare command name, run from the trusted system
// directories as ExecRunner runs it; PATH is never searched. A nil stdin
// reads from the null device and a nil stdout or stderr discards. The
// command gets a fixed minimal environment, not the caller's.
func RunAs(ctx context.Context, uid, gid uint32, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	path := name
	if !filepath.IsAbs(name) {
		var err error
		if path, err = hostFinder.find(name); err != nil {
			return fmt.Errorf("running %s as %d:%d: %w", name, uid, gid, err)
		}
	}
	cmd := runAsCmd(ctx, uid, gid, stdin, stdout, stderr, path, args...)
	cmd.Args[0] = name
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s as %d:%d: %w", name, uid, gid, err)
	}
	return nil
}

// runAsCmd builds the command RunAs runs from the absolute path.
func runAsCmd(ctx context.Context, uid, gid uint32, stdin io.Reader, stdout, stderr io.Writer, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append([]string(nil), runAsEnv...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{gid}},
	}
	return cmd
}
