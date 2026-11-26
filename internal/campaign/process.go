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
	"slices"
	"strconv"
	"strings"
	"time"
)

type goEnvironment struct {
	GOROOT, GOOS, GOARCH, GOFLAGS string
	GOVERSION, GOWORK, GOMOD      string
}

type commandResult struct {
	kind            string
	detail          string
	failed          []string
	tests           []string
	events          []string
	inventory       map[string][]string
	packages        map[string]string
	diagnostics     []compilerDiagnostic
	moreDiagnostics bool
	failedBuilds     map[string]bool
	elapsed          time.Duration
	maxTestElapsed   time.Duration
	hasTestElapsed   bool
	deadlineExceeded bool
	testTimedOut     bool
}

func environment(ctx context.Context, dir string) (goEnvironment, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOROOT", "GOOS", "GOARCH", "GOFLAGS", "GOVERSION", "GOWORK", "GOMOD")
	cmd.Dir = dir
	configureProcess(cmd)
	out, err := cmd.Output()
	cleanupProcess(cmd)
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
	if env.GOOS != "linux" && env.GOOS != "darwin" && env.GOOS != "windows" {
		return env, fmt.Errorf("native probe execution requires Linux, macOS, or Windows")
	}
	if err := checkGoFlags(env.GOFLAGS); err != nil {
		return env, err
	}
	return env, nil
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func checkGoFlags(raw string) error {
	for raw = strings.TrimSpace(raw); raw != ""; raw = strings.TrimSpace(raw) {
		var flag string
		if quote := raw[0]; quote == '\'' || quote == '"' {
			var found bool
			flag, raw, found = strings.Cut(raw[1:], string(quote))
			if !found {
				return fmt.Errorf("GOFLAGS contains an unterminated quote")
			}
		} else {
			end := strings.IndexAny(raw, " \t\r\n")
			if end < 0 {
				end = len(raw)
			}
			flag, raw = raw[:end], raw[end:]
		}
		name, value, _ := strings.Cut(flag, "=")
		switch strings.TrimLeft(name, "-") {
		case "a", "asan", "buildvcs", "msan", "p", "race", "tags", "trimpath":
			continue
		case "mod":
			if value == "readonly" || value == "vendor" {
				continue
			}
		}
		return fmt.Errorf("GOFLAGS option %q is not supported in a mutation campaign", name)
	}
	return nil
}

func cleanEnv(overrides ...string) []string {
	env := os.Environ()
	remove := map[string]bool{"TMT_GO_TRACE": true, "TMT_GO_MUTATION": true, sourceWitnessEnvironment: true}
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

func (s *session) workerEnv(overrides ...string) []string {
	if s.workerProcs > 0 {
		overrides = append([]string{"GOMAXPROCS=" + strconv.Itoa(s.workerProcs)}, overrides...)
	}
	return cleanEnv(overrides...)
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
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	cleanupProcess(cmd)
	if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil
	}
	output.finish()
	deadlineExceeded := ctx.Err() == context.DeadlineExceeded
	testTimedOut := output.timedOut && !output.buildFailed
	result := commandResult{detail: strings.TrimSpace(output.tail.String()), failed: output.failed, tests: output.tests, events: output.events, inventory: output.inventory, packages: output.packages,
		diagnostics: output.diagnostics, moreDiagnostics: output.moreDiagnostics, failedBuilds: output.failedBuilds, elapsed: elapsed, maxTestElapsed: output.maxTestElapsed, hasTestElapsed: output.hasTestElapsed, deadlineExceeded: deadlineExceeded, testTimedOut: testTimedOut}
	switch {
	case deadlineExceeded:
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
	tail            bytes.Buffer
	line            []byte
	discardLine     bool
	failed          []string
	tests           []string
	events          []string
	direct          bool
	inventory       map[string][]string
	packages        map[string]string
	buildFailed     bool
	timedOut        bool
	diagnostics     []compilerDiagnostic
	moreDiagnostics bool
	failedBuilds    map[string]bool
	maxTestElapsed  time.Duration
	hasTestElapsed  bool
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
		o.moreDiagnostics = true
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
		Action, Test, Package, ImportPath, FailedBuild, Output string
		Elapsed                                                *float64
	}
	if json.Unmarshal([]byte(line), &event) != nil {
		return
	}
	if event.Action == "build-output" {
		for _, diagnostic := range parseCompilerOutput(event.ImportPath, event.Output) {
			if slices.Contains(o.diagnostics, diagnostic) {
				continue
			}
			if len(o.diagnostics) == 128 {
				o.moreDiagnostics = true
				break
			}
			o.diagnostics = append(o.diagnostics, diagnostic)
		}
	}
	if event.FailedBuild != "" || event.Action == "build-fail" {
		o.buildFailed = true
		key := event.FailedBuild
		if key == "" {
			key = event.ImportPath
		}
		if o.failedBuilds == nil {
			o.failedBuilds = make(map[string]bool)
		}
		o.failedBuilds[key] = true
	}
	if event.Test == "" && (event.Action == "pass" || event.Action == "skip" || event.Action == "fail") {
		if o.packages == nil {
			o.packages = make(map[string]string)
		}
		o.packages[event.Package] = event.Action
		if event.Action == "pass" && event.Elapsed != nil && *event.Elapsed >= 0 {
			o.hasTestElapsed = true
			if d := time.Duration(*event.Elapsed * float64(time.Second)); d > o.maxTestElapsed {
				o.maxTestElapsed = d
			}
		}
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
	if strings.HasPrefix(event.Output, "panic: test timed out after ") {
		o.timedOut = true
	}
}

func (o *commandOutput) recordBinary(line string) {
	if strings.HasPrefix(line, "panic: test timed out after ") {
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
