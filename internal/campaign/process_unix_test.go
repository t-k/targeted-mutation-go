//go:build darwin || linux

package campaign

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCommandReapsChildProcessGroupAndRecordsElapsed(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	result := runCommand(
		context.Background(),
		5*time.Second,
		dir,
		cleanEnv(),
		"sh",
		"-c",
		`sleep 30 </dev/null >/dev/null 2>&1 & printf "%d" "$!" > "$1"`,
		"sh",
		pidFile,
	)
	if result.kind != "passed" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.elapsed <= 0 {
		t.Fatalf("expected positive elapsed duration, got %v", result.elapsed)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", raw, err)
	}
	err = syscall.Kill(childPID, 0)
	deadline := time.Now().Add(2 * time.Second)
	for err == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		err = syscall.Kill(childPID, 0)
	}
	if !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("expected child process %d to be reaped (ESRCH), got err=%v", childPID, err)
	}

	// Also verify a background child inheriting stdout/stderr pipes does not fail a passing leader.
	inheritPIDFile := filepath.Join(dir, "inherit.pid")
	inheritRes := runCommand(
		context.Background(),
		5*time.Second,
		dir,
		cleanEnv(),
		"sh",
		"-c",
		`sleep 30 </dev/null & printf "%d" "$!" > "$1"`,
		"sh",
		inheritPIDFile,
	)
	if inheritRes.kind != "passed" {
		t.Fatalf("expected leader exit 0 with inherited pipe child to pass, got %+v", inheritRes)
	}
	rawInherit, err := os.ReadFile(inheritPIDFile)
	if err != nil {
		t.Fatal(err)
	}
	inheritPID, err := strconv.Atoi(strings.TrimSpace(string(rawInherit)))
	if err != nil {
		t.Fatal(err)
	}
	if killErr := syscall.Kill(inheritPID, 0); !errors.Is(killErr, syscall.ESRCH) {
		_ = syscall.Kill(inheritPID, syscall.SIGKILL)
		t.Fatalf("expected pipe-holding child %d to be terminated, got %v", inheritPID, killErr)
	}
}

func TestCleanupProcessRejectsUnsafePIDsUnix(t *testing.T) {
	var killed []int
	origKill := killProcessGroup
	killProcessGroup = func(pid int, sig syscall.Signal) error {
		killed = append(killed, pid)
		return syscall.ESRCH
	}
	t.Cleanup(func() { killProcessGroup = origKill })

	cleanupProcess(nil)
	cleanupProcess(&exec.Cmd{})
	cleanupProcess(&exec.Cmd{Process: &os.Process{Pid: 0}})
	cleanupProcess(&exec.Cmd{Process: &os.Process{Pid: 1}})
	if len(killed) != 0 {
		t.Fatalf("cleanupProcess must not signal pid <= 1, got %v", killed)
	}
}
