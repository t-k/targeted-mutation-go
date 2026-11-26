package campaign

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type goEnvironment struct {
	GOROOT, GOOS, GOARCH, GOFLAGS string
}

type commandResult struct {
	kind      string
	detail    string
	failed    []string
	tests     []string
	events    []string
	inventory map[string][]string
	packages  map[string]string
}

func environment(ctx context.Context, dir string) (goEnvironment, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOROOT", "GOOS", "GOARCH", "GOFLAGS")
	cmd.Dir = dir
	configureProcess(cmd)
	out, err := cmd.Output()
	var env goEnvironment
	if err != nil {
		return env, fmt.Errorf("go env: %w", err)
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return env, fmt.Errorf("decode go env: %w", err)
	}
	if env.GOOS != runtime.GOOS || env.GOARCH != runtime.GOARCH {
		return env, fmt.Errorf("target GOOS/GOARCH is %s/%s, test binaries must run locally on %s/%s",
			env.GOOS, env.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	if env.GOOS != "linux" && env.GOOS != "darwin" {
		return env, fmt.Errorf("native probe execution requires Linux or macOS")
	}
	for _, flag := range strings.Fields(env.GOFLAGS) {
		key, value, _ := strings.Cut(strings.Trim(flag, "'\""), "=")
		normalized := strings.TrimPrefix(strings.TrimLeft(key, "-"), "test.")
		switch normalized {
		case "overlay", "exec", "run", "skip", "bench", "count", "short", "parallel", "shuffle",
			"failfast", "v", "fuzz", "fuzzcachedir", "list", "vet", "o", "c", "json", "n",
			"outputdir", "cover", "coverprofile", "cpuprofile", "memprofile", "blockprofile",
			"mutexprofile", "trace", "testlogfile", "modfile":
			return env, fmt.Errorf("GOFLAGS option %s conflicts with campaign execution", key)
		case "mod":
			if value != "readonly" && value != "vendor" {
				return env, fmt.Errorf("GOFLAGS option %s can modify module files", flag)
			}
		}
	}
	return env, nil
}

func cleanEnv(overrides ...string) []string {
	env := os.Environ()
	remove := map[string]bool{"TMT_GO_TRACE": true, "TMT_GO_MUTATION": true}
	for _, value := range overrides {
		key, _, _ := strings.Cut(value, "=")
		remove[key] = true
	}
	values := make([]string, 0, len(env)+len(overrides))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !remove[key] {
			values = append(values, entry)
		}
	}
	return append(values, overrides...)
}

func binaryEnv(env goEnvironment, trace, mutation string) []string {
	return cleanEnv("PATH="+filepath.Join(env.GOROOT, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMT_GO_TRACE="+trace, "TMT_GO_MUTATION="+mutation)
}

func runCommand(ctx context.Context, limit time.Duration, dir string, env []string, path string, args ...string) commandResult {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = append(env, "PWD="+dir)
	output := commandOutput{direct: filepath.IsAbs(path)}
	cmd.Stdout, cmd.Stderr = &output, &output
	configureProcess(cmd)
	err := cmd.Run()
	output.finish()
	result := commandResult{detail: strings.TrimSpace(output.tail.String()), failed: output.failed, tests: output.tests, events: output.events, inventory: output.inventory, packages: output.packages}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		result.kind = "timeout"
	case ctx.Err() != nil:
		result.kind = "cancelled"
	case err == nil && !output.buildFailed:
		result.kind = "passed"
	case output.buildFailed:
		result.kind = "build-error"
	case output.timedOut:
		result.kind = "timeout"
	default:
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(output.failed) != 0 {
			result.kind = "failed"
		} else {
			result.kind = "error"
		}
		if err != nil && result.detail == "" {
			result.detail = err.Error()
		}
	}
	return result
}

type commandOutput struct {
	tail        bytes.Buffer
	line        []byte
	discardLine bool
	failed      []string
	tests       []string
	events      []string
	direct      bool
	inventory   map[string][]string
	packages    map[string]string
	buildFailed bool
	timedOut    bool
}

func (o *commandOutput) Write(data []byte) (int, error) {
	const limit = 64 * 1024
	if len(data) >= limit {
		o.tail.Reset()
		o.tail.Write(data[len(data)-limit:])
	} else {
		if excess := o.tail.Len() + len(data) - limit; excess > 0 {
			o.tail.Next(excess)
		}
		o.tail.Write(data)
	}
	for pending := data; len(pending) != 0; {
		idx := bytes.IndexByte(pending, '\n')
		if idx < 0 {
			o.add(pending)
			break
		}
		o.add(pending[:idx])
		o.finish()
		pending = pending[idx+1:]
	}
	return len(data), nil
}

func (o *commandOutput) add(data []byte) {
	const lineLimit = 64 * 1024
	if !o.discardLine && len(o.line)+len(data) <= lineLimit {
		o.line = append(o.line, data...)
	} else {
		o.discardLine = true
		o.line = o.line[:0]
	}
}

func (o *commandOutput) finish() {
	line := string(o.line)
	o.line = o.line[:0]
	o.discardLine = false
	if line == "" {
		return
	}
	if o.direct {
		o.recordBinary(line)
		return
	}
	if line[0] != '{' {
		return
	}
	var event struct {
		Action, Test, Package, FailedBuild, Output string
	}
	if json.Unmarshal([]byte(line), &event) != nil {
		return
	}
	if event.FailedBuild != "" || event.Action == "build-fail" {
		o.buildFailed = true
	}
	if event.Test == "" && (event.Action == "pass" || event.Action == "skip" || event.Action == "fail") {
		if o.packages == nil {
			o.packages = make(map[string]string)
		}
		o.packages[event.Package] = event.Action
	}
	if event.Test != "" {
		switch event.Action {
		case "run", "pass", "skip", "fail":
			if o.inventory == nil {
				o.inventory = make(map[string][]string)
			}
			o.inventory[event.Package] = append(o.inventory[event.Package], event.Action+":"+event.Test)
			if event.Action == "fail" {
				o.failed = append(o.failed, event.Test)
			}
		}
	}
	if strings.HasPrefix(event.Output, "panic: test timed out") {
		o.timedOut = true
	}
}

func (o *commandOutput) recordBinary(line string) {
	if strings.HasPrefix(line, "panic: test timed out") {
		o.timedOut = true
		return
	}
	_, framed, ok := strings.Cut(line, "\x16")
	if !ok {
		return
	}
	framed = strings.TrimLeft(framed, " \t")
	if test, ok := strings.CutPrefix(framed, "=== RUN   "); ok {
		test = strings.TrimSpace(test)
		o.tests = append(o.tests, test)
		o.events = append(o.events, "run:"+test)
		return
	}
	for _, status := range []string{"PASS", "SKIP", "FAIL"} {
		test, ok := strings.CutPrefix(framed, "--- "+status+": ")
		if !ok {
			continue
		}
		test, _, _ = strings.Cut(test, " ")
		o.events = append(o.events, strings.ToLower(status)+":"+test)
		if status == "FAIL" {
			o.failed = append(o.failed, test)
		}
		return
	}
}

var _ io.Writer = (*commandOutput)(nil)
