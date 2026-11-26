package campaign

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

func TestCoverageMapBlockMatching(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "math.go")
	src := []byte("package m\nfunc Unreached(a, b int) int {\n\treturn a + b\n}\n")
	coverPath := filepath.Join(dir, "cov.out")
	coverData := "mode: set\nexample.org/m/math.go:2.32,4.2 1 0\n"
	if err := os.WriteFile(coverPath, []byte(coverData), 0600); err != nil {
		t.Fatal(err)
	}
	pkgs := []target.Package{{
		Path:  "example.org/m",
		Files: []*target.File{{Path: filePath, Data: src}},
	}}
	cm := loadCoverageProfile(coverPath, pkgs)
	if cm == nil {
		t.Fatal("expected coverageMap")
	}
	plusOff := len("package m\nfunc Unreached(a, b int) int {\n\treturn a ")
	edit := &target.Edit{
		File: filePath, Line: 3, Column: 11, StartLine: 3, EndLine: 3,
		Start: plusOff, End: plusOff + 1, Kind: "source-operator",
	}
	if !cm.isUncovered(filePath, src, edit) {
		t.Fatal("expected edit inside count=0 block to be uncovered")
	}
	// File containing a //line directive must conservatively return false.
	lineDirSrc := append([]byte("//line virtual.go:1\n"), src...)
	if cm.isUncovered(filePath, lineDirSrc, edit) {
		t.Fatal("expected //line directive to disable coverage pruning")
	}
	// Overlapping a count>0 block must return false.
	cm.files[filePath] = append(cm.files[filePath], coverBlock{startLine: 3, startCol: 1, endLine: 3, endCol: 20, count: 1})
	if cm.isUncovered(filePath, src, edit) {
		t.Fatal("expected edit overlapping count>0 block not to be uncovered")
	}
}

func TestCoveragePrunesUnreachedSourceMutationsWithoutBuilding(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.org/covprune\n\ngo 1.26.0\n")
	write("cov.go", `package covprune

func Used(a, b int) int {
	return a + b
}

func Unused(a, b int) int {
	return a + b
}
`)
	write("cov_test.go", `package covprune

import "testing"

func TestUsed(t *testing.T) {
	if Used(2, 3) != 5 {
		t.Fatal("bad sum")
	}
}
`)
	// Target only Unused (line 8) in --full mode: every source-operator edit on a + b
	// is inside an uncovered block and should be pruned with 0 source runs!
	rep, err := Run(context.Background(), Config{
		Dir:      dir,
		Patterns: []string{"."},
		Lines:    []string{"cov.go:8"},
		Full:     true,
		Jobs:     1,
		ExcludeRules: []string{
			"return-zero",
			"empty-block",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.SourceRuns != 0 {
		t.Fatalf("expected 0 source runs for uncovered source-operator edits, got %d (findings=%+v)", rep.SourceRuns, rep.Findings)
	}
	if rep.Survived == 0 {
		t.Fatalf("expected observed source-survivors on uncovered line, got %+v", rep)
	}
	for _, f := range rep.Findings {
		if f.Status != "source-survivor" || f.Evidence != "observed" {
			t.Fatalf("expected source-survivor [observed], got %+v", f)
		}
	}

	// With --verify-survivors, coverage pruning is disabled and source runs execute normally.
	verified, err := Run(context.Background(), Config{
		Dir:             dir,
		Patterns:        []string{"."},
		Lines:           []string{"cov.go:8"},
		Full:            true,
		VerifySurvivors: true,
		Jobs:            1,
		ExcludeRules:    []string{"return-zero", "empty-block"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if verified.SourceRuns == 0 {
		t.Fatalf("expected source runs when --verify-survivors is set, got 0")
	}
	if verified.Survived != rep.Survived {
		t.Fatalf("verdict count mismatch: pruned=%d verified=%d", rep.Survived, verified.Survived)
	}
}

func TestArithmeticSchemataMatchesSourceOnly(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.org/arith\n\ngo 1.26.0\n")
	write("arith.go", `package arith

func Add(a, b int) int {
	return a + b
}

func Mask(a, b uint8) uint8 {
	return a & b
}

func AddZero(a int) int {
	return a + 0
}
`)
	write("arith_test.go", `package arith

import "testing"

func TestArith(t *testing.T) {
	if Add(6, 2) != 8 {
		t.Fatal("add")
	}
	if Mask(0x0f, 0x06) != 0x06 {
		t.Fatal("mask")
	}
	if AddZero(5) != 5 {
		t.Fatal("zero")
	}
}
`)
	fullRep, err := Run(context.Background(), Config{
		Dir:            dir,
		Patterns:       []string{"."},
		Full:           true,
		Jobs:           2,
		KilledFindings: true,
		ExcludeRules:   []string{"return-zero", "empty-block", "integer-step"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fullRep.SourceRuns != 0 {
		t.Fatalf("expected 0 source runs for arithmetic schemata in --full mode, got %d (replays=%d)", fullRep.SourceRuns, fullRep.Replays)
	}
	if fullRep.Replays == 0 {
		t.Fatalf("expected probe replays for arithmetic schemata, got 0")
	}
	srcRep, err := Run(context.Background(), Config{
		Dir:            dir,
		Patterns:       []string{"."},
		SourceOnly:     true,
		Jobs:           2,
		KilledFindings: true,
		ExcludeRules:   []string{"return-zero", "empty-block", "integer-step"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fullRep.Killed != srcRep.Killed || fullRep.Survived != srcRep.Survived || fullRep.Incomplete != 0 {
		t.Fatalf("full (%d killed, %d survived, %d incomplete) != source-only (%d killed, %d survived)",
			fullRep.Killed, fullRep.Survived, fullRep.Incomplete, srcRep.Killed, srcRep.Survived)
	}
}
