package campaign

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestArithmeticSchemataPerCandidateSoundnessAndAuditAndList(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.org/arithsound\n\ngo 1.26.0\n")
	write("arith.go", `package arithsound

type Amount int16

func DivPanicOnly(a, b int) int {
	return a / b
}

func DivBoth(a, b int) int {
	return a / b
}

func SignedMinDiv(a, b int8) int8 {
	return a / b
}

func NamedAdd(a, b Amount) Amount {
	return a + b
}

func ShiftCtx(n uint) uint8 {
	return (1 << n) + 1
}

func AddZero(a int) int {
	return a + 0
}

func UnreachedAdd(a, b int) int {
	return a + b
}
`)
	write("arith_test.go", `package arithsound

import (
	"math"
	"runtime"
	"testing"
)

var expectedProcs = runtime.GOMAXPROCS(0)

func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}

func TestArithSoundness(t *testing.T) {
	if runtime.GOMAXPROCS(0) != expectedProcs {
		t.Fatalf("GOMAXPROCS changed: got %d, want %d", runtime.GOMAXPROCS(0), expectedProcs)
	}
	mustPanic(t, func() { _ = DivPanicOnly(1, 0) })
	mustPanic(t, func() { _ = DivBoth(1, 0) })
	if DivBoth(6, 2) != 3 {
		t.Fatal("div")
	}
	if SignedMinDiv(math.MinInt8, -1) != math.MinInt8 {
		t.Fatal("minint8")
	}
	if NamedAdd(3, 4) != 7 {
		t.Fatal("named")
	}
	if ShiftCtx(2) != 5 {
		t.Fatal("shift")
	}
	if AddZero(5) != 5 {
		t.Fatal("zero")
	}
}
`)
	exclude := []string{"return-zero", "empty-block", "integer-step"}

	// 1. --full --list must not emit bogus comparison negation candidates for arithmetic sites.
	listed, err := Run(context.Background(), Config{
		Dir:          dir,
		Patterns:     []string{"."},
		Full:         true,
		List:         true,
		ExcludeRules: exclude,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range listed.Findings {
		if f.Rule == "negation" || f.Rule == "boundary" {
			t.Fatalf("arithmetic-only package emitted bogus comparison candidate in --list: %+v", f)
		}
	}

	// 2. --full with --audit 1 must match --source-only per candidate ID and actually audit 1 arithmetic probe.
	fullRep, err := Run(context.Background(), Config{
		Dir:            dir,
		Patterns:       []string{"."},
		Full:           true,
		Jobs:           2,
		Audit:          1,
		KilledFindings: true,
		ExcludeRules:   exclude,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fullRep.Incomplete != 0 {
		t.Fatalf("expected 0 incomplete in --full, got %d: %+v", fullRep.Incomplete, fullRep.Findings)
	}
	if fullRep.Audited != 1 {
		t.Fatalf("expected Audited=1 for arithmetic probe, got %d", fullRep.Audited)
	}
	if fullRep.Replays == 0 {
		t.Fatalf("expected arithmetic probe replays > 0")
	}

	srcRep, err := Run(context.Background(), Config{
		Dir:            dir,
		Patterns:       []string{"."},
		SourceOnly:     true,
		Jobs:           2,
		KilledFindings: true,
		ExcludeRules:   exclude,
	})
	if err != nil {
		t.Fatal(err)
	}
	if srcRep.Incomplete != 0 {
		t.Fatalf("expected 0 incomplete in --source-only, got %d", srcRep.Incomplete)
	}

	byID := func(r Report) map[string]Finding {
		m := make(map[string]Finding, len(r.Findings))
		for _, f := range r.Findings {
			m[f.ID] = f
		}
		return m
	}
	fullMap, srcMap := byID(fullRep), byID(srcRep)
	if len(fullMap) != len(srcMap) {
		t.Fatalf("candidate count mismatch: full=%d source-only=%d", len(fullMap), len(srcMap))
	}
	for id, want := range srcMap {
		got, ok := fullMap[id]
		if !ok {
			t.Fatalf("missing candidate %s (%s:%d %s->%s) in --full", id, want.File, want.Line, want.Expression, want.Mutation)
		}
		if got.Status != want.Status {
			t.Errorf("verdict mismatch for %s:%d (%s -> %s): full=%s (%s), source-only=%s (%s)",
				want.File, want.Line, want.Expression, want.Mutation, got.Status, got.Evidence, want.Status, want.Evidence)
		}
	}
}

func TestBoolSchemataPerCandidateSoundnessAndEdgeCases(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.org/boolsound\n\ngo 1.26.0\n")
	write("logic.go", `package boolsound

type Flag bool

func CheckErr(err error) bool {
	if err != nil {
		return false
	}
	return true
}

func Invert(ok bool) bool {
	return !ok
}

func PtrCheck(p *int) bool {
	return p == nil
}

func SwitchAndMap(b bool) int {
	m := map[bool]int{true: 1, false: 2}
	switch b {
	case true:
		return m[true]
	default:
		return m[false]
	}
}

func NamedBool(f Flag) Flag {
	return !f
}

func UnreachedBool(ok bool) bool {
	return !ok
}
`)
	write("logic_test.go", `package boolsound

import (
	"errors"
	"testing"
)

func TestLogic(t *testing.T) {
	if CheckErr(errors.New("boom")) || !CheckErr(nil) {
		t.Fatal("err")
	}
	if Invert(true) || !Invert(false) {
		t.Fatal("invert")
	}
	x := 1
	if !PtrCheck(nil) || PtrCheck(&x) {
		t.Fatal("ptr")
	}
	if SwitchAndMap(true) != 1 || SwitchAndMap(false) != 2 {
		t.Fatal("switch/map")
	}
	if NamedBool(true) != false || NamedBool(false) != true {
		t.Fatal("named")
	}
}
`)
	exclude := []string{"return-zero", "empty-block", "empty-case", "integer-step"}

	listed, err := Run(context.Background(), Config{
		Dir:          dir,
		Patterns:     []string{"."},
		Full:         true,
		List:         true,
		ExcludeRules: exclude,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range listed.Findings {
		if f.Rule == "negation" || f.Rule == "boundary" {
			t.Fatalf("bool-only package emitted bogus comparison candidate in --list: %+v", f)
		}
	}

	fullRep, err := Run(context.Background(), Config{
		Dir:            dir,
		Patterns:       []string{"."},
		Full:           true,
		Jobs:           2,
		Audit:          2,
		KilledFindings: true,
		ExcludeRules:   exclude,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fullRep.Incomplete != 0 {
		t.Fatalf("expected 0 incomplete in --full, got %d: %+v", fullRep.Incomplete, fullRep.Findings)
	}
	if fullRep.Audited != 2 {
		t.Fatalf("expected Audited=2, got %d", fullRep.Audited)
	}
	if fullRep.Replays == 0 {
		t.Fatalf("expected bool schemata probe replays > 0")
	}

	srcRep, err := Run(context.Background(), Config{
		Dir:            dir,
		Patterns:       []string{"."},
		SourceOnly:     true,
		Jobs:           2,
		KilledFindings: true,
		ExcludeRules:   exclude,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fullRep.SourceRuns >= srcRep.SourceRuns {
		t.Fatalf("expected bool schemata to reduce source runs: full=%d source-only=%d", fullRep.SourceRuns, srcRep.SourceRuns)
	}

	byID := func(r Report) map[string]Finding {
		m := make(map[string]Finding, len(r.Findings))
		for _, f := range r.Findings {
			m[f.ID] = f
		}
		return m
	}
	fullMap, srcMap := byID(fullRep), byID(srcRep)
	if len(fullMap) != len(srcMap) {
		t.Fatalf("candidate count mismatch: full=%d source-only=%d", len(fullMap), len(srcMap))
	}
	for id, want := range srcMap {
		got, ok := fullMap[id]
		if !ok {
			t.Fatalf("missing candidate %s (%s:%d %s->%s) in --full", id, want.File, want.Line, want.Expression, want.Mutation)
		}
		if got.Status != want.Status {
			t.Errorf("verdict mismatch for %s:%d (%s -> %s): full=%s (%s), source-only=%s (%s)",
				want.File, want.Line, want.Expression, want.Mutation, got.Status, got.Evidence, want.Status, want.Evidence)
		}
	}
}
