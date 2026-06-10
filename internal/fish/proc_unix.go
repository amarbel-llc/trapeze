//go:build !windows

package fish

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setupProcessGroup puts the child in its own process group and makes
// cancellation kill the whole group, so interrupting a command also
// stops its descendants (e.g. processes spawned by a script).
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
