package campaign

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessEventPICT(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/events.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] {
		t.Run(strings.ReplaceAll(row, "\t", "/"), func(t *testing.T) {
			f := strings.Fields(row)
			const pkg, name = "example.org/p", "TestReal"
			parser := commandOutput{direct: f[0] == "binary"}
			var wire string
			if parser.direct {
				if f[2] == "lookalike" {
					wire = "--- FAIL: TestFabricated (0.00s)\n=== RUN   TestFabricated"
				}
				wire += fmt.Sprintf("\x16=== RUN   %s\n\x16--- %s: %s (0.00s)\n", name, strings.ToUpper(f[1]), name)
			} else {
				if f[2] == "lookalike" {
					wire = "{\"Action\":\"output\",\"Test\":\"TestReal\",\"Output\":\"--- FAIL: TestFabricated\"}\n"
				}
				wire += fmt.Sprintf("{\"Action\":\"run\",\"Package\":%q,\"Test\":%q}\n", pkg, name)
				wire += fmt.Sprintf("{\"Action\":%q,\"Package\":%q,\"Test\":%q}\n", f[1], pkg, name)
			}
			if f[3] == "bytes" {
				for _, b := range []byte(wire) {
					_, _ = parser.Write([]byte{b})
				}
			} else {
				_, _ = parser.Write([]byte(wire))
			}
			events := parser.events
			if !parser.direct {
				events = parser.inventory[pkg]
			}
			if want := []string{"run:" + name, f[1] + ":" + name}; !slices.Equal(events, want) {
				t.Fatalf("events %q, want %q", events, want)
			}
			if (f[1] == "fail") != slices.Contains(parser.failed, name) || slices.Contains(parser.failed, "TestFabricated") {
				t.Fatalf("failures %q", parser.failed)
			}
		})
	}
}

func TestRunCommandReapsChildProcessGroupAndRecordsElapsed(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unix process group reaping")
	}
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
