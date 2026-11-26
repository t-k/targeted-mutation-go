//go:build darwin || linux

package campaign

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

var killProcessGroup = func(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 1 {
			return os.ErrProcessDone
		}
		err := killProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

func cleanupProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 1 {
		return
	}
	pid := cmd.Process.Pid
	_ = killProcessGroup(pid, syscall.SIGKILL)
	for range 25 {
		if errors.Is(killProcessGroup(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}
