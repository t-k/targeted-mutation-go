package campaign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCampaignRunsPackageConsumersAndPreservesSources(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "go.mod", "module example.org/shop\n\ngo 1.26.0\n")
	production := `package price

type Amount uint64

func Free(yen Amount) bool { return yen >= 5000 }

func Weak(a, b int) bool { return a > b }

func NotUsed(yen int) bool { return yen >= 40 }
`
	fixture(t, dir, "price/price.go", production)
	fixture(t, dir, "checkout/checkout.go", `package checkout
import "example.org/shop/price"
func free(yen price.Amount) bool { return price.Free(yen) }
`)
	fixture(t, dir, "checkout/checkout_test.go", `package checkout
import (
	"os"
	"testing"
	"example.org/shop/price"
)
func TestMain(m *testing.M) { os.Exit(m.Run()) }
func TestPrices(t *testing.T) {
	t.Parallel()
	t.Run("paid", func(t *testing.T) {
		t.Parallel()
		if free(4800) { t.Fatal("low price should pay") }
	})
	t.Run("free", func(t *testing.T) {
		t.Parallel()
		if !free(5000) { t.Fatal("boundary price should be free") }
	})
}
func TestWeakCheck(t *testing.T) {
	if price.Weak(0, 1) && price.Weak(1, 0) { t.Fatal("both directions are true") }
}
`)
	report, err := Run(context.Background(), Config{
		Dir: dir, Patterns: []string{"./..."}, Jobs: 2, Timeout: 30 * time.Second,
		VerifySurvivors: true, ShowUnreached: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Builds != 1 || report.Sites != 3 {
		t.Fatalf("unexpected campaign counts: %+v", report)
	}
	var input, assertion, unreached bool
	for _, f := range report.Findings {
		switch f.Status {
		case "input-gap":
			if f.Witness != nil && f.Witness.Input == "4999" && f.Witness.Original == false &&
				f.Witness.Mutant && f.Witness.Call == "Free(4999)" &&
				f.Evidence == "replayed" && len(f.Packages) == 1 && f.Packages[0] == "example.org/shop/checkout" {
				input = true
			}
		case "assertion-gap":
			assertion = assertion || strings.Contains(f.Expression, "a > b") && f.Evidence == "replayed"
		case "unreached":
			unreached = unreached || strings.Contains(f.Expression, ">= 40")
		}
	}
	if !input || !assertion || !unreached || report.Killed < 2 {
		t.Fatalf("unexpected findings and counters: %+v", report)
	}
	got, err := os.ReadFile(filepath.Join(dir, "price", "price.go"))
	if err != nil || string(got) != production {
		t.Fatalf("target changed: %s, %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "price"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("target directory changed: %v, %v", entries, err)
	}
}

func TestCampaignRejectsFailingOrdinaryBaseline(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "go.mod", "module example.org/fail\n\ngo 1.26.0\n")
	fixture(t, dir, "fail.go", "package fail\nfunc Ready(a int) bool { return a >= 1 }\n")
	fixture(t, dir, "fail_test.go", "package fail\nimport \"testing\"\nfunc TestFailure(t *testing.T) { t.Fatal(\"baseline sentinel\") }\n")
	_, err := Run(context.Background(), Config{Dir: dir, Patterns: []string{"."}, Jobs: 1, Timeout: 30 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "baseline sentinel") {
		t.Fatalf("unexpected baseline error: %v", err)
	}
}

func fixture(t *testing.T, dir, name, contents string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCampaignKeepsNamedBoolAndIgnoresTestLikeStdout(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "go.mod", "module example.org/named\n\ngo 1.26.0\n")
	fixture(t, dir, "named.go", `package named
type Flag bool
var atomic = 1
func Ready(n int) Flag { return n > 0 }
`)
	fixture(t, dir, "named_test.go", `package named
import ("fmt"; "testing")
func TestReady(t *testing.T) {
	fmt.Println("=== RUN   sample")
	fmt.Println("--- FAIL: fabricated (0.00s)")
	if !Ready(1) { t.Fatal("expected ready") }
}
`)
	report, err := Run(context.Background(), Config{Dir: dir, VerifySurvivors: true})
	if err != nil || report.Killed != 1 || report.Incomplete != 0 {
		t.Fatalf("named bool/stdout was not preserved: %+v, %v", report, err)
	}
}

func TestCampaignRejectsMutationsThatSuppressTests(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "go.mod", "module example.org/suppress\n\ngo 1.26.0\n")
	fixture(t, dir, "check.go", `package suppress
func Start(a, b bool) bool { return a || b }
func Gate(n int) bool { return n >= 0 }
`)
	fixture(t, dir, "check_test.go", `package suppress
import ("os"; "testing")
func TestMain(m *testing.M) {
	if !Start(true, false) { os.Exit(0) }
	os.Exit(m.Run())
}
func TestGate(t *testing.T) {
	if !Gate(0) { t.Skip("gate skipped the assertion") }
}
`)
	report, err := Run(context.Background(), Config{Dir: dir, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	var source, probe int
	for _, finding := range report.Findings {
		if finding.Status == "unstable" && finding.Rule == "source-operator" {
			source++
		}
		if finding.Status == "unstable" && (finding.Rule == "boundary" || finding.Rule == "negation") {
			probe++
		}
	}
	if source != 1 || probe != 2 {
		t.Fatalf("suppressed tests were not detected: %+v", report)
	}
}

func TestCampaignScopesAndRevertsGitHunks(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	fixture(t, dir, "go.mod", "module example.org/git\n\ngo 1.26.0\n")
	original := `package changes
func Scope(n int) bool { return n >=
	2 }
func Unchanged(n int) bool { return n >= 4 }
`
	fixture(t, dir, "changes.go", original)
	fixture(t, dir, "changes_test.go", `package changes
import "testing"
func TestScope(t *testing.T) {
	if Scope(0) || !Scope(9) { t.Fatal("wrong scope") }
}
`)
	for _, args := range [][]string{
		{"init", "-b", "main"}, {"add", "."},
		{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.org", "-c", "commit.gpgsign=false", "commit", "-m", "baseline"},
	} {
		if _, err := git(ctx, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	changed := strings.Replace(original, "\t2 }", "\t3 }", 1)
	fixture(t, dir, "changes.go", changed)
	fixture(t, dir, "added.go", "package changes\nfunc Added(n int) bool { return n >= 5 }\n")
	report, err := Run(ctx, Config{Dir: dir, Since: "HEAD", UndoChanges: true, ShowUnreached: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Sites != 2 || report.SourceRuns != 1 || report.Unreached != 1 {
		t.Fatalf("incorrect diff scope: %+v", report)
	}
	for _, finding := range report.Findings {
		if finding.Status == "untested-change" && finding.File == "changes.go" && finding.Line == 3 {
			return
		}
	}
	t.Fatalf("missing Git revert survivor: %+v", report)
}

func TestTestOnlyConsumerDetectsTheBoundary(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "go.mod", "module example.org/consumer\n\ngo 1.26.0\n")
	fixture(t, dir, "limit/limit.go", "package limit\nfunc Free(x int) bool { return x >= 50 }\n")
	fixture(t, dir, "limit/limit_test.go", `package limit
import "testing"
func TestLoose(t *testing.T) { if Free(0) || !Free(100) { t.Fatal("loose limit") } }
`)
	fixture(t, dir, "integration/integration_test.go", `package integration_test
import ("testing"; "example.org/consumer/limit")
func TestBoundary(t *testing.T) { if !limit.Free(50) { t.Fatal("wrong boundary") } }
`)
	result, err := Run(context.Background(), Config{Dir: dir, Comparison: "boundary", KilledFindings: true})
	if err != nil || result.Killed != 1 || result.Builds != 2 {
		t.Fatalf("test-only consumer was omitted: %+v, %v", result, err)
	}
}

func TestProfileFlagsAndEmptyScopeBaseline(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "go.mod", "module example.org/empty\n\ngo 1.26.0\n")
	fixture(t, dir, "empty.go", "package empty\nfunc Value() int { return 1 }\n")
	fixture(t, dir, "empty_test.go", "package empty\nimport \"testing\"\nfunc TestBroken(t *testing.T) { t.Fatal(\"intentionally failing\") }\n")
	_, err := Run(context.Background(), Config{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "intentionally failing") {
		t.Errorf("empty scope did not validate its baseline: %v", err)
	}
	for _, option := range []string{"-coverprofile=coverage.out", "-debug-runtime-trace=coverage.out", "-cpu=1"} {
		fixture(t, dir, "coverage.out", "original data")
		t.Setenv("GOFLAGS", option)
		_, err = Run(context.Background(), Config{Dir: dir})
		if err == nil || !strings.Contains(err.Error(), "GOFLAGS") {
			t.Errorf("%s should be rejected: %v", option, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "coverage.out"))
		if err != nil || string(data) != "original data" {
			t.Errorf("%s overwrote the fixture: %d bytes, %v", option, len(data), err)
		}
	}
}
