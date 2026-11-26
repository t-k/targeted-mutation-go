package campaign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

func TestCanonicalBaselineKeyPermutedEvents(t *testing.T) {
	packages := map[string]string{"example.org/a": "pass", "example.org/b": "pass"}
	invA := map[string][]string{
		"example.org/a": {"run:TestA", "run:TestA/sub1", "run:TestA/sub2", "pass:TestA/sub2", "pass:TestA/sub1", "pass:TestA"},
		"example.org/b": {"run:TestB", "pass:TestB"},
	}
	invB := map[string][]string{
		"example.org/b": {"pass:TestB", "run:TestB"},
		"example.org/a": {"pass:TestA", "pass:TestA/sub1", "run:TestA/sub2", "pass:TestA/sub2", "run:TestA/sub1", "run:TestA"},
	}
	invC := map[string][]string{
		"example.org/a": {"run:TestA", "pass:TestA"},
		"example.org/b": {"run:TestB", "pass:TestB"},
	}
	keyA := canonicalBaselineKey(packages, invA)
	keyB := canonicalBaselineKey(packages, invB)
	if keyA != keyB {
		t.Fatalf("canonicalBaselineKey differed on permuted events: %s vs %s", keyA, keyB)
	}
	if keyC := canonicalBaselineKey(packages, invC); keyC == keyA {
		t.Fatalf("canonicalBaselineKey failed to distinguish different inventories")
	}
	if len(invA["example.org/a"]) == 0 || invA["example.org/a"][0] != "run:TestA" {
		t.Fatalf("canonicalBaselineKey mutated caller slice: %v", invA["example.org/a"])
	}
}

func TestOpenCheckpointSymlinkAndIgnoredTrees(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	targetFile := filepath.Join(outside, "shared.txt")
	if err := os.WriteFile(targetFile, []byte("v1"), 0600); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(outside, "linkeddir")
	if err := os.MkdirAll(subDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "inside.txt"), []byte("dir"), 0600); err != nil {
		t.Fatal(err)
	}

	fixture(t, dir, "go.mod", "module example.org/symlink\n\ngo 1.26.0\n")
	fixture(t, dir, "main.go", "package symlink\nfunc Ready(x int) bool { return x >= 1 }\n")
	fixture(t, dir, "main_test.go", "package symlink\nimport \"testing\"\nfunc TestReady(t *testing.T) { if !Ready(1) { t.Fatal(\"ready\") } }\n")
	fixture(t, dir, ".worktree/w/ignored.go", "package ignored\n")
	fixture(t, dir, "node_modules/pkg/index.js", "module.exports = 1;\n")
	fixture(t, dir, "docs.local/log.md", "# log\n")

	if err := os.Symlink(subDir, filepath.Join(dir, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing-target"), filepath.Join(dir, "brokenlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetFile, filepath.Join(dir, "filelink.txt")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	env, err := environment(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(dir, "checkpoint.json")
	s := session{cfg: Config{Dir: dir, Patterns: []string{"./..."}, Checkpoint: journalPath, Jobs: 1, Timeout: 5 * time.Second, BuildTimeout: time.Minute}, env: env}
	if err := s.openCheckpoint(ctx); err != nil {
		t.Fatalf("openCheckpoint failed with symlinks: %v", err)
	}
	if err := s.journal.Close(); err != nil {
		t.Fatal(err)
	}

	fp1, err := s.campaignFingerprint(ctx, journalPath)
	if err != nil {
		t.Fatal(err)
	}

	// Ignored directories should not invalidate fingerprint.
	fixture(t, dir, ".worktree/w/ignored.go", "package ignored_changed\n")
	fixture(t, dir, "node_modules/pkg/index.js", "module.exports = 2;\n")
	fixture(t, dir, "docs.local/log.md", "# updated log\n")
	fp2, err := s.campaignFingerprint(ctx, journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Fatalf("ignored directory change invalidated fingerprint: %s vs %s", fp1, fp2)
	}

	// Changing regular-file symlink target contents must invalidate fingerprint.
	if err := os.WriteFile(targetFile, []byte("v2"), 0600); err != nil {
		t.Fatal(err)
	}
	fp3, err := s.campaignFingerprint(ctx, journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == fp3 {
		t.Fatalf("changing regular-file symlink target content did not change fingerprint")
	}
}

func TestVerifyWitnessGroupShadowingAndFallback(t *testing.T) {
	dir := t.TempDir()
	source := `package shadow
var testing = 1
var true = 1
var false = 2
func t(x int) bool { return x >= 10 }
`
	fixture(t, dir, "go.mod", "module example.org/witnessshadow\n\ngo 1.26.0\n")
	fixture(t, dir, "shadow.go", source)
	fixture(t, dir, "shadow_test.go", `package shadow
import testpkg "testing"
func TestShadow(t *testpkg.T) {}
`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	env, err := environment(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	packages, err := target.Discover(ctx, dir, []string{"./..."}, "", nil, target.Discovery{})
	if err != nil {
		t.Fatal(err)
	}
	s := session{
		cfg:     Config{Dir: dir, Jobs: 1, Timeout: 5 * time.Second, BuildTimeout: time.Minute, VerifyWitnesses: true},
		env:     env,
		scratch: t.TempDir(),
	}
	s.packages = packages
	s.testPackages = packages
	s.replacements = s.collect(nil)
	s.overlay, err = makeOverlay(s.scratch, s.replacements)
	if err != nil {
		t.Fatal(err)
	}
	var validSite *target.Site
	for _, site := range s.sites {
		if site.Callable == "t" {
			validSite = site
			break
		}
	}
	if validSite == nil {
		t.Fatalf("missing site for func t: %+v", s.sites)
	}
	// Pick boundary candidate (>= -> >) so input 10 gives original true, mutant false.
	var cand analysis.Candidate
	for _, c := range analysis.Plan(validSite.Op, validSite.Domain, analysis.Summary{}, true) {
		if c.Kind == "boundary" && c.Witness != nil && *c.Witness == 10 {
			cand = c
			break
		}
	}
	report := Report{
		Schema: 1,
		Findings: []Finding{
			{
				ID:     "valid-shadowed",
				Status: "input-gap",
				Witness: &Witness{
					Input:    "10",
					Operand:  validSite.Left,
					Original: true,
					Mutant:   false,
					Call:     "t(10)",
				},
			},
			{
				ID:     "broken-call",
				Status: "input-gap",
				Witness: &Witness{
					Input:    "10",
					Operand:  validSite.Left,
					Original: true,
					Mutant:   false,
					Call:     "undefinedWitnessFunc(10)",
				},
			},
		},
	}
	probes := []probeJob{
		{site: validSite, candidate: cand, finding: report.Findings[0]},
		{site: validSite, candidate: cand, finding: report.Findings[1]},
	}
	s.verifyWitnesses(ctx, &report, probes)
	if !report.Findings[0].Witness.Verified || !strings.Contains(report.Findings[0].Witness.Code, "tmtT.Fatalf") {
		t.Fatalf("expected valid shadowed witness to verify via fallback, got %+v", report.Findings[0])
	}
	if report.Findings[1].Witness.Verified || !strings.Contains(report.Findings[1].Detail, "witness verification:") {
		t.Fatalf("expected broken witness to report detail, got %+v", report.Findings[1])
	}
	if report.Incomplete != 1 {
		t.Fatalf("expected Incomplete == 1, got %d", report.Incomplete)
	}
}

func TestSuggestProbeAssertionIgnoresParentSetupOnSubtests(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "go.mod", "module example.org/subtestsetup\n\ngo 1.26.0\n")
	fixture(t, dir, "ready.go", "package subtestsetup\nfunc Ready(x int) bool { return x >= 1 }\n")
	fixture(t, dir, "ready_test.go", `package subtestsetup
import "testing"
func TestParent(t *testing.T) {
	_ = Ready(1)
	t.Run("unrelated", func(t *testing.T) {})
}
`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report, err := Run(ctx, Config{
		Dir:               dir,
		Comparison:        "boundary",
		Jobs:              1,
		Timeout:           5 * time.Second,
		BuildTimeout:      time.Minute,
		SuggestAssertions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %+v", report)
	}
	f := report.Findings[0]
	if f.Status != "assertion-gap" {
		t.Fatalf("expected assertion-gap, got %+v", f)
	}
	if len(f.Tests) != 1 || f.Tests[0] != "TestParent" {
		t.Fatalf("expected attribution to TestParent only, got %v", f.Tests)
	}
}
