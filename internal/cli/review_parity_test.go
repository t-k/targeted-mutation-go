package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/campaign"
)

func assertAccountingIdentities(t *testing.T, report campaign.Report) {
	t.Helper()
	if report.MutantsRun != report.Killed+report.Survived {
		t.Errorf("mutants_run (%d) != killed (%d) + survived (%d)", report.MutantsRun, report.Killed, report.Survived)
	}
	if report.TimedOut > report.Killed {
		t.Errorf("timed_out (%d) > killed (%d)", report.TimedOut, report.Killed)
	}
	if report.SurvivorBreakdown == nil {
		t.Errorf("survivor_breakdown must be present on evaluated report: %+v", report)
		return
	}
	want := report.SurvivorBreakdown.Reported + report.SurvivorBreakdown.Suppressed
	if report.Survived != want {
		t.Errorf("survived (%d) != reported (%d) + suppressed (%d)",
			report.Survived, report.SurvivorBreakdown.Reported, report.SurvivorBreakdown.Suppressed)
	}
}

func TestCLIPackageInitOrderAndGo122Directive(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod": "module example.org/initorder\n\ngo 1.22.0\n",
		"a.go": `package initorder

var order []string
var first = recordFirst()
var second = recordSecond()

func Ready(x int) bool { return x >= 2 }

func recordFirst() bool {
	order = append(order, "first")
	return Ready(3)
}

func recordSecond() int {
	order = append(order, "second")
	return 42
}
`,
		"a_test.go": `package initorder

import "testing"

func TestInitOrder(t *testing.T) {
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("package init order changed: %v", order)
	}
	if Ready(0) || !Ready(2) {
		t.Fatal("unexpected Ready result")
	}
}
`,
	})
	report, code := analyzeJSON(t, dir, "--verify-survivors", ".")
	if code != 2 || report.Incomplete != 0 {
		t.Fatalf("expected clean exit 2 on go 1.22 module, got exit %d, report %+v", code, report)
	}
	assertAccountingIdentities(t, report)
	if len(report.Findings) != 1 || report.Findings[0].Witness == nil || report.Findings[0].Witness.Input != "1" {
		t.Fatalf("expected input-gap at 1, got %+v", report.Findings)
	}
}

func TestCLIUnreachedFullDoesNotClaimObserved(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod": "module example.org/unreached\n\ngo 1.26.0\n",
		"lib.go": `package unreached

func Used(x int) bool { return x >= 1 }
func Unused(x int) bool { return x >= 5 }
`,
		"lib_test.go": `package unreached

import "testing"

func TestUsed(t *testing.T) {
	if Used(0) || !Used(1) {
		t.Fatal("used")
	}
}
`,
	})
	fullReport, fullCode := analyzeJSON(t, dir, "--full", ".")
	sourceReport, sourceCode := analyzeJSON(t, dir, "--source-only", ".")
	if fullCode != 2 || sourceCode != 2 {
		t.Fatalf("full exit %d, source-only exit %d", fullCode, sourceCode)
	}
	assertAccountingIdentities(t, fullReport)
	assertAccountingIdentities(t, sourceReport)
	for _, f := range fullReport.Findings {
		if f.Line == 4 && f.Evidence == "observed" {
			t.Fatalf("unreached site claimed observed evidence: %+v", f)
		}
	}
}

func TestCLIEmptyTestSelectionAndUnimportedTestPackage(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod":     "module example.org/unimported\n\ngo 1.26.0\n",
		"lib/lib.go": "package lib\nfunc Value(x int) int { return x + 1 }\n",
		"other/other_test.go": `package other

import "testing"

func TestOther(t *testing.T) {}
`,
	})

	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"analyze", "--dir", dir, "--test=^TestDoesNotExist$", "./other"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "no tests matched --test") {
		t.Fatalf("expected exit 1 for empty --test, got %d, stderr=%q", code, &stderr)
	}

	report, code := analyzeJSON(t, dir, "--source-only", "--test-package=./other", "./lib")
	if code != 2 || report.Rejected == 0 {
		t.Fatalf("expected empty-block on Value to be rejected as non-viable, exit=%d, report=%+v", code, report)
	}
	assertAccountingIdentities(t, report)
	for _, f := range report.Findings {
		if f.Rule == "empty-block" && f.Status != "non-viable" {
			t.Fatalf("uncompiled invalid empty-block became %s: %+v", f.Status, f)
		}
	}
}

func TestCLISinceMultilineAndChangedTestsScoping(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod": "module example.org/multiline\n\ngo 1.26.0\n",
		"price.go": `package multiline

func Multi(x int) bool {
	return x >=
		5000
}

func Unchanged(x int) bool {
	return x >= 10
}
`,
		"price_test.go": `package multiline

import "testing"

func TestMulti(t *testing.T) {
	if Multi(4800) || !Multi(5000) {
		t.Fatal("multi")
	}
	if Unchanged(9) || !Unchanged(10) {
		t.Fatal("unchanged")
	}
}
`,
	})
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "base")

	// Modify ONLY line 5 (operand line, leaving operator >= on unchanged line 4) AND touch price_test.go.
	writeFiles(t, dir, map[string]string{
		"price.go": `package multiline

func Multi(x int) bool {
	return x >=
		6000
}

func Unchanged(x int) bool {
	return x >= 10
}
`,
		"price_test.go": `package multiline

import "testing"

func TestMulti(t *testing.T) {
	if Multi(4800) || !Multi(6000) {
		t.Fatal("multi updated")
	}
	if Unchanged(9) || !Unchanged(10) {
		t.Fatal("unchanged")
	}
}
`,
	})

	report, code := analyzeJSON(t, dir, "--since=HEAD", ".")
	if code != 2 {
		t.Fatalf("expected exit 2 for multiline gap, got %d: %+v", code, report)
	}
	assertAccountingIdentities(t, report)
	if report.Sites != 1 {
		t.Fatalf("expected Sites=1 (only changed Multi), got %d: %+v", report.Sites, report)
	}
	found5999 := false
	for _, f := range report.Findings {
		if f.Line == 8 || f.Line == 9 {
			t.Fatalf("unchanged site leaked into --since findings: %+v", f)
		}
		if f.Status == "input-gap" && f.Witness != nil && f.Witness.Input == "5999" {
			found5999 = true
		}
	}
	if !found5999 {
		t.Fatalf("multiline changed comparison lost its input-gap finding: %+v", report.Findings)
	}
}

func TestCLICheckpointResumesUnderParallelSubtestsAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod":  "module example.org/parcheck\n\ngo 1.26.0\n",
		"sub/x":   "fixture",
		"par.go":  "package parcheck\nfunc Check(x int) bool { return x >= 10 }\n",
		"par_test.go": `package parcheck

import (
	"fmt"
	"testing"
	"time"
)

func TestParallel(t *testing.T) {
	for i := range 6 {
		t.Run(fmt.Sprintf("sub-%d", i), func(t *testing.T) {
			t.Parallel()
			time.Sleep(time.Duration((i*7)%5) * time.Millisecond)
			if Check(9) || !Check(10) {
				t.Fatal("boundary")
			}
		})
	}
}
`,
	})
	if err := os.Symlink("/nonexistent/target", filepath.Join(dir, "broken_symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "sub"), filepath.Join(dir, "dir_symlink")); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "par.jsonl")
	args := []string{"--checkpoint=" + journal, "--conventional", "--show-killed", "."}
	first, code1 := analyzeJSON(t, dir, args...)
	second, code2 := analyzeJSON(t, dir, args...)
	if code1 != 0 || code2 != 0 || first.Killed != 2 || second.Killed != 2 || second.Resumed != 2 || second.Replays != 0 {
		t.Fatalf("parallel checkpoint failed to resume: first=%+v (exit %d), second=%+v (exit %d)", first, code1, second, code2)
	}
}

func TestCLIBitmaskHoleJumpingNegativeMaskLenAndMarginAndFloat(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod": "module example.org/advanced\n\ngo 1.26.0\n",
		"adv.go": `package advanced

func HighTier(flags int) bool {
	return (flags & 0xF0) >= 0x20
}

func EvenOnly(x int8) bool {
	return (x & -2) >= 1
}

func HasTwo(xs []int) bool {
	return len(xs) >= 2
}

func NonEmpty(xs []int) bool {
	return len(xs) >= 0
}

func CanAfford(balance, price int) bool {
	return balance >= price
}

func Warm(temp float64) bool {
	return temp >= 36.5
}
`,
		"adv_test.go": `package advanced

import (
	"math"
	"testing"
)

func TestAdvanced(t *testing.T) {
	if HighTier(0x00) || !HighTier(0x20) {
		t.Fatal("HighTier")
	}
	if EvenOnly(0) || !EvenOnly(2) {
		t.Fatal("EvenOnly")
	}
	if HasTwo(nil) || !HasTwo([]int{1, 2}) {
		t.Fatal("HasTwo")
	}
	if !NonEmpty([]int{1}) {
		t.Fatal("NonEmpty")
	}
	if CanAfford(80, 100) || !CanAfford(100, 100) {
		t.Fatal("CanAfford")
	}
	if Warm(35.0) || !Warm(38.0) || Warm(math.NaN()) {
		t.Fatal("Warm")
	}
}
`,
	})
	report, code := analyzeJSON(t, dir, "--verify-witnesses", ".")
	if code != 2 || report.Incomplete != 0 {
		t.Fatalf("expected exit 2 with 0 incomplete, got exit=%d, report=%+v", code, report)
	}
	assertAccountingIdentities(t, report)

	var sawMask16, sawLen1, sawMargin, sawFloatBoundary bool
	for _, f := range report.Findings {
		switch f.Line {
		case 4:
			if f.Status == "input-gap" && f.Witness != nil && f.Witness.Input == "16" && f.Witness.Verified {
				sawMask16 = true
			}
		case 8:
			if f.Status == "input-gap" && f.Witness != nil && f.Witness.Input == "1" {
				t.Fatalf("negative mask emitted unreachable odd witness 1: %+v", f)
			}
		case 12:
			if f.Status == "input-gap" && f.Witness != nil && f.Witness.Input == "1" {
				sawLen1 = true
			}
		case 16:
			if f.Witness != nil && f.Witness.Input == "-1" {
				t.Fatalf("len(xs) >= 0 emitted impossible negative witness -1: %+v", f)
			}
		case 20:
			if f.Status == "input-gap" && f.Rule == "margin" && strings.Contains(f.Mutation, "price - 1 (without overflow)") {
				sawMargin = true
			}
		case 24:
			if f.Status == "input-gap" && f.Rule == "boundary" && f.Mutation == "temp > 36.5" {
				sawFloatBoundary = true
			}
		}
	}
	if !sawMask16 || !sawLen1 || !sawMargin || !sawFloatBoundary {
		t.Fatalf("missing expected findings (mask16=%t, len1=%t, margin=%t, float=%t): %+v",
			sawMask16, sawLen1, sawMargin, sawFloatBoundary, report.Findings)
	}
}

func TestCLIInlineIgnoreSanitizeShadowingTimeoutAndOrphanCleanup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	writeFiles(t, dir, map[string]string{
		"go.mod": "module example.org/safety\n\ngo 1.26.0\n",
		"safety.go": `package safety

const testing = 1

func t(x int) bool { return x >= 10 }

func IgnoredTrailing(x int) bool { return x >= 10 } // tmt:ignore: intentional gap

func IgnoredAbove(x int) bool {
	// tmt:ignore standalone reason
	return(x >= 10)
}

func EvilLiteral(s string) bool {
	return s == "` + `\x1b[31m\u202eforged` + `"
}
`,
		"safety_test.go": `package safety

import stdtesting "testing"

func TestSafety(tt *stdtesting.T) {
	if t(5) || !t(10) {
		tt.Fatal("t")
	}
	if IgnoredTrailing(5) || !IgnoredTrailing(10) {
		tt.Fatal("trailing")
	}
	if IgnoredAbove(5) || !IgnoredAbove(10) {
		tt.Fatal("above")
	}
	_ = EvilLiteral("other")
}
`,
	})

	var textOut, textErr bytes.Buffer
	code := Execute(context.Background(), []string{"analyze", "--dir", dir, "--full", "--verify-witnesses", "."}, &textOut, &textErr)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d\nstdout:\n%s\nstderr:\n%s", code, &textOut, &textErr)
	}
	rendered := textOut.String()
	if strings.Contains(rendered, "\x1b") || strings.Contains(rendered, "\u202e") {
		t.Fatalf("unsanitized control/BiDi sequence leaked into text output: %q", rendered)
	}
	if strings.Contains(rendered, "NON VIABLE") {
		t.Fatalf("NON VIABLE should be hidden from default text output: %s", rendered)
	}

	report, jsonCode := analyzeJSON(t, dir, "--full", "--verify-witnesses", ".")
	if jsonCode != 2 || report.Incomplete != 0 || report.Suppressed < 2 {
		t.Fatalf("unexpected JSON report (code=%d): %+v", jsonCode, report)
	}
	assertAccountingIdentities(t, report)

	verifiedShadowedT := false
	for _, f := range report.Findings {
		if f.Line == 7 || f.Line == 11 {
			if f.Status != "non-viable" && f.Status != "killed" {
				t.Fatalf("tmt:ignore line %d was not suppressed: %+v", f.Line, f)
			}
		}
		if f.Line == 5 && f.Status == "input-gap" && f.Witness != nil && f.Witness.Verified {
			verifiedShadowedT = true
		}
	}
	if !verifiedShadowedT {
		t.Fatalf("witness for function named t with shadowed testing did not verify: %+v", report.Findings)
	}

	// Verify infinite-loop probe timeout is confirmed serially and counted as killed (TimedOut == 1, exit 0)
	loopDir := t.TempDir()
	writeFiles(t, loopDir, map[string]string{
		"go.mod": "module example.org/loop\n\ngo 1.26.0\n",
		"loop.go": `package loop

func CountUp(limit int) int {
	i := 0
	for i < limit {
		i++
	}
	return i
}
`,
		"loop_test.go": `package loop

import "testing"

func TestCountUp(t *testing.T) {
	if got := CountUp(0); got != 0 {
		t.Fatalf("got %d", got)
	}
	if got := CountUp(1); got != 1 {
		t.Fatalf("got %d", got)
	}
}
`,
	})
	loopReport, loopCode := analyzeJSON(t, loopDir, "--timeout=400ms", "--jobs=2", ".")
	if loopCode != 0 || loopReport.Incomplete != 0 || loopReport.TimedOut == 0 {
		t.Fatalf("expected infinite-loop probe mutation to be killed via timeout (exit 0), got exit=%d, report=%+v", loopCode, loopReport)
	}
	assertAccountingIdentities(t, loopReport)

	// Verify orphan child process reaping when statement-remove deletes defer cmd.Process.Kill()
	if runtime.GOOS == "windows" {
		return
	}
	procDir := t.TempDir()
	writeFiles(t, procDir, map[string]string{
		"go.mod": "module example.org/orphan\n\ngo 1.26.0\n",
		"orphan.go": `package orphan

import (
	"os"
	"os/exec"
	"strconv"
)

func SpawnAndCleanup(pidPath string) error {
	cmd := exec.Command("sh", "-c", "sleep 30 >/dev/null 2>&1 </dev/null")
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Process.Kill()
	return os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0600)
}
`,
		"orphan_test.go": `package orphan

import "testing"

func TestSpawn(t *testing.T) {
	if err := SpawnAndCleanup(` + strconv.Quote(pidFile) + `); err != nil {
		t.Fatal(err)
	}
}
`,
	})
	_, _ = analyzeJSON(t, procDir, "--source-only", "--lines=orphan.go:14", ".")
	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("missing child PID file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid child PID %q: %v", rawPID, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	assertOrphanReaped(t, pid)
}

func TestCLIReviewRound2Regressions(t *testing.T) {
	// 1. Left-constant float32/float64 (NaN, -0.0, +Inf): --full must match --source-only verdicts.
	floatDir := t.TempDir()
	writeFiles(t, floatDir, map[string]string{
		"go.mod": "module example.org/floatparity\n\ngo 1.26.0\n",
		"0.go": `package floatparity

type F32 float32

func LeftBound(x F32) bool {
	return 0.5 < x
}

func RightBound(x float64) bool {
	return x <= -0.0
}
`,
		"0_test.go": `package floatparity

import (
	"math"
	"testing"
)

func TestFloats(t *testing.T) {
	if LeftBound(F32(math.NaN())) {
		t.Fatal("NaN")
	}
	if LeftBound(0.25) || !LeftBound(F32(math.Inf(1))) {
		t.Fatal("bounds")
	}
	if !RightBound(math.Copysign(0, -1)) || RightBound(1.0) || RightBound(math.NaN()) {
		t.Fatal("right")
	}
}
`,
	})
	fullReport, fullCode := analyzeJSON(t, floatDir, "--full", "--show-killed", ".")
	srcReport, srcCode := analyzeJSON(t, floatDir, "--source-only", "--show-killed", ".")
	if fullCode != srcCode || fullReport.Incomplete != 0 || srcReport.Incomplete != 0 {
		t.Fatalf("float --full (exit %d) vs --source-only (exit %d) mismatch:\nfull=%+v\nsrc=%+v",
			fullCode, srcCode, fullReport, srcReport)
	}
	assertAccountingIdentities(t, fullReport)
	fullStatus := make(map[string]string)
	for _, f := range fullReport.Findings {
		if f.Rule == "source-operator" {
			fullStatus[strconv.Itoa(f.Line)+":"+f.Mutation] = f.Status
		}
	}
	foundFloatSwap := false
	for _, f := range srcReport.Findings {
		if f.Rule == "source-operator" && f.Expression == "<" && f.Mutation == ">" {
			foundFloatSwap = true
			if f.Status != "killed" {
				t.Fatalf("expected 0.5 < x -> 0.5 > x to be killed in --source-only, got %+v", f)
			}
			if got := fullStatus[strconv.Itoa(f.Line)+":"+f.Mutation]; got != "killed" {
				t.Fatalf("0.5 < x -> 0.5 > x got status %q in --full, want killed!", got)
			}
		}
	}
	if !foundFloatSwap {
		t.Fatalf("0.5 < x -> 0.5 > x candidate not found in findings!")
	}

	// 2. Shadowed predeclared `string`, `false`, `got` + `0.go` + relative TMPDIR + SARIF on symlinked dir.
	shadowDir := t.TempDir()
	relTmp := filepath.Join(shadowDir, "reltmp")
	if err := os.MkdirAll(relTmp, 0700); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, shadowDir, map[string]string{
		"go.mod": "module example.org/shadow\n\ngo 1.26.0\n",
		"0!.go": `package shadow

import "encoding/json"

const false = (0 == 0)
const got = 99

func Check(x int) bool {
	return x >= 10
}

func CmpNum(a, b json.Number) bool {
	return a < b
}
`,
		"0_test.go": `package shadow

import (
	"encoding/json"
	"testing"
)

type string int

func TestCheck(t *testing.T) {
	if Check(5) != (0 != 0) || Check(10) != (0 == 0) {
		t.Fatal("check")
	}
	var a, b json.Number
	_ = CmpNum(a, b)
}
`,
	})
	t.Setenv("TMPDIR", relTmp)
	shadowReport, shadowCode := analyzeJSON(t, shadowDir, "--full", "--verify-witnesses", ".")
	if shadowCode != 2 || shadowReport.Incomplete != 0 {
		t.Fatalf("shadowed string/false/got failed (code=%d): %+v", shadowCode, shadowReport)
	}
	verifiedShadow := false
	for _, f := range shadowReport.Findings {
		if f.Status == "input-gap" && f.Witness != nil && f.Witness.Verified {
			verifiedShadow = true
			if strings.Contains(f.Witness.Code, "!= false") {
				t.Fatalf("shadowed false leaked into verified witness code: %s", f.Witness.Code)
			}
		}
	}
	if !verifiedShadow {
		t.Fatalf("expected verified witness under shadowed false/string/got: %+v", shadowReport.Findings)
	}

	// Verify SARIF via cli.Execute preserves locations on symlinked t.TempDir()
	var sarifOut, sarifErr bytes.Buffer
	if code := Execute(context.Background(), []string{"analyze", "--dir", shadowDir, "--format", "sarif", "."}, &sarifOut, &sarifErr); code != 2 {
		t.Fatalf("sarif exit=%d, stderr=%s", code, &sarifErr)
	}
	if !strings.Contains(sarifOut.String(), `"uri":"0!.go"`) && !strings.Contains(sarifOut.String(), `"uri":"0%21.go"`) {
		t.Fatalf("SARIF dropped location on symlinked temp dir: %s", sarifOut.String())
	}

	// 3. Unimported --test-package must compile-check without executing excluded package's TestMain.
	unimpDir := t.TempDir()
	writeFiles(t, unimpDir, map[string]string{
		"go.mod":            "module example.org/unimp\n\ngo 1.26.0\n",
		"lib/lib.go":        "package lib\n\nfunc Double(x int) int { return x + x }\n",
		"lib/lib_test.go":   "package lib\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) {\n\tos.Exit(99)\n}\n",
		"app/app.go":        "package app\n\nfunc Ok() bool { return true }\n",
		"app/app_test.go":   "package app\n\nimport \"testing\"\n\nfunc TestOk(t *testing.T) { if !Ok() { t.Fatal() } }\n",
	})
	unimpReport, unimpCode := analyzeJSON(t, unimpDir, "--source-only", "--test-package", "./app", "./lib")
	if unimpCode != 2 || unimpReport.Incomplete != 0 {
		t.Fatalf("excluded TestMain was executed during compile check (code=%d): %+v", unimpCode, unimpReport)
	}

	// 4. Symlinked fixture directory invalidates --checkpoint when target contents change.
	ckptRoot := t.TempDir()
	modDir := filepath.Join(ckptRoot, "mod")
	extData := filepath.Join(ckptRoot, "shared-fixtures")
	if err := os.MkdirAll(filepath.Join(modDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(extData, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extData, "limit.txt"), []byte("10"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(extData, filepath.Join(modDir, "testdata")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, modDir, map[string]string{
		"go.mod": "module example.org/symckpt\n\ngo 1.26.0\n",
		"lim.go": "package symckpt\n\nfunc Allow(x int) bool { return x >= 10 }\n",
		"lim_test.go": `package symckpt

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestAllow(t *testing.T) {
	raw, err := os.ReadFile("testdata/limit.txt")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if !Allow(n) || Allow(n-1) {
		t.Fatalf("unexpected Allow around %d", n)
	}
}
`,
	})
	ckptFile := filepath.Join(ckptRoot, "ckpt.jsonl")
	r1, c1 := analyzeJSON(t, modDir, "--checkpoint", ckptFile, ".")
	if c1 != 0 || r1.Killed == 0 {
		t.Fatalf("initial symlinked fixture run failed (code=%d): %+v", c1, r1)
	}
	r1Resume, c1Resume := analyzeJSON(t, modDir, "--checkpoint", ckptFile, ".")
	if c1Resume != 0 || r1Resume.Resumed == 0 {
		t.Fatalf("expected unchanged symlinked fixture run to resume (code=%d): %+v", c1Resume, r1Resume)
	}
	// Mutate ONLY the external symlinked fixture file and verify checkpoint fingerprint rejects stale reuse.
	if err := os.WriteFile(filepath.Join(extData, "limit.txt"), []byte("20"), 0600); err != nil {
		t.Fatal(err)
	}
	var ckptOut, ckptErr bytes.Buffer
	ckptCode := Execute(context.Background(), []string{"analyze", "--dir", modDir, "--checkpoint", ckptFile, "."}, &ckptOut, &ckptErr)
	if ckptCode != 1 || !strings.Contains(ckptErr.String(), "checkpoint fingerprint does not match") {
		t.Fatalf("expected external symlinked directory edit to reject stale checkpoint (code=%d, stderr=%q)", ckptCode, ckptErr.String())
	}

	// 5. --since with only test edits must retain incomplete verdicts (exit 3, not exit 0).
	sinceIncDir := t.TempDir()
	writeFiles(t, sinceIncDir, map[string]string{
		"go.mod": "module example.org/sinceinc\n\ngo 1.26.0\n",
		"loop.go": `package sinceinc

func Spin(x int) int {
	for x < 0 {
	}
	return x
}
`,
		"loop_test.go": `package sinceinc

import "testing"

func TestSpin(t *testing.T) {
	if Spin(0) != 0 {
		t.Fatal()
	}
}
`,
	})
	runGit(t, sinceIncDir, "init", "-q")
	runGit(t, sinceIncDir, "add", ".")
	runGit(t, sinceIncDir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "base")
	writeFiles(t, sinceIncDir, map[string]string{
		"loop_test.go": `package sinceinc

import "testing"

func TestSpin(t *testing.T) {
	if Spin(0) != 0 || Spin(1) != 1 {
		t.Fatal()
	}
}
`,
	})
	incReport, incCode := analyzeJSON(t, sinceIncDir, "--since=HEAD", "--timeout=250ms", "--timeout-recheck=none", ".")
	if incCode != 3 || incReport.Incomplete == 0 {
		t.Fatalf("expected --since with unconfirmed timeout in unchanged production code to exit 3 with Incomplete>0, got code=%d, report=%+v",
			incCode, incReport)
	}
}
