//go:build unix

package larkcli

import (
	"os/exec"
	"syscall"
)

// The consumer gets its own process group so a stop also reaches the native
// binary that the npm wrapper (`node run.js`) spawns.
func setProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func signalGroup(cmd *exec.Cmd, kill bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}
