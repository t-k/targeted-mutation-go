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
