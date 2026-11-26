//go:build darwin || linux

package cli

import (
	"syscall"
	"testing"
)

func assertOrphanReaped(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("orphan child process %d survived after campaign finished", pid)
	}
}
