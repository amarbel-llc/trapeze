//go:build windows

package fish

import "os/exec"

// setupProcessGroup is a no-op on Windows; cancellation falls back to
// exec.CommandContext's default Process.Kill, which terminates only the
// direct child. (Shell mode targets fish, so Windows is best-effort.)
func setupProcessGroup(cmd *exec.Cmd) {}
