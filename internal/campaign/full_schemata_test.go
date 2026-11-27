package campaign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

func TestFullSchemataNestedLogicShiftAssignAndPruningParity(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.org/fullschemata\n\ngo 1.26.0\n")
	write("ops.go", `package fullschemata

type State struct {
	Flags uint64
	Count int
}

const (
	FlagA uint64 = 1
	FlagB uint64 = 2
)

func NestedBitwiseCmp(s *State) bool {
	if s.Flags&FlagA == 0 || s.Count > 10 {
		return true
	}
	return false
}

func ShortCircuitPanic(p *int) bool {
	if p != nil && *p > 0 {
		return true
	}
	return false
}

func PureLogicAlwaysTrue(a, b int) bool {
	if a > 0 && b > 0 {
		return true
	}
	return false
}

func PureLogicOr(a, b int) bool {
	if a == 0 || b == 0 {
		return true
	}
	return false
}

func GroupedLogic(a, b, c bool) bool {
	return a || b && c
}

func BoolCondAndSoleLocal(ok bool, x int) int {
	sole := x > 5
	if ok {
		return 1
	}
	if sole {
		return 2
	}
	return 0
}

func ArithPruneSameResult(a, b int) int {
	return a * b
}

func ShiftOps(x uint32, n uint) uint32 {
	return (x << n) + (x >> 0)
}

func CompoundAndIncDec(s *State, m map[string]int) int {
	s.Flags |= FlagA
	s.Count += 2
	s.Count++
	m["k"] += 1
	return s.Count
}

func CompoundDivPanic(x, y int) int {
	x /= y
	return x
}

func CompoundAddZero(x int) int {
	x *= 0
	return x
}

func CompoundNestedLHS(a []int, i int) int {
	a[i+1] += 2
	return a[i+1]
}

func LogicLeftPanic(xs []bool, flag bool) bool {
	return xs[0] && flag
}

type Box struct{ X any }

func StructIfacePanic(a, b Box, flag bool) bool {
	return (a == b) && flag
}

func ArrayIfacePanic(a, b [1]any, flag bool) bool {
	return (a == b) || flag
}

func TypeSwitchLocal(x any, flag bool) bool {
	switch v := x.(type) {
	case bool:
		return v && flag
	default:
		return false
	}
}

func ShadowedBoolLogic(bool int, a, b bool) bool {
	return a && b && bool > 0
}

func UntypedRuneShift(x uint64) uint64 {
	const n = 'a' + (1 << 32)
	return x << n
}

func UnreachedAll(a, b int) bool {
	return a&1 == 0 && b > 0
}
`)
	write("ops_test.go", `package fullschemata

import "testing"

func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}

func TestOps(t *testing.T) {
	s := &State{Flags: FlagA, Count: 5}
	if NestedBitwiseCmp(s) {
		t.Fatal("expected false")
	}
	if !NestedBitwiseCmp(&State{Flags: 0, Count: 5}) {
		t.Fatal("expected true on flag")
	}
	if !NestedBitwiseCmp(&State{Flags: FlagA, Count: 11}) {
		t.Fatal("expected true on count")
	}
	v := 3
	if !ShortCircuitPanic(&v) || ShortCircuitPanic(nil) {
		t.Fatal("short circuit")
	}
	if !PureLogicAlwaysTrue(1, 2) {
		t.Fatal("pure logic always true")
	}
	if !PureLogicOr(0, 1) || PureLogicOr(1, 2) {
		t.Fatal("pure logic or")
	}
	if !GroupedLogic(false, true, true) || GroupedLogic(false, false, true) || !GroupedLogic(true, false, false) {
		t.Fatal("grouped logic")
	}
	if BoolCondAndSoleLocal(false, 10) != 2 || BoolCondAndSoleLocal(false, 1) != 0 {
		t.Fatal("bool cond")
	}
	if ArithPruneSameResult(2, 2) != 4 {
		t.Fatal("arith prune")
	}
	if ShiftOps(4, 1) != 12 {
		t.Fatal("shift")
	}
	m := map[string]int{"k": 1}
	st := &State{Flags: FlagA, Count: 1}
	if CompoundAndIncDec(st, m) != 4 || st.Flags != FlagA || m["k"] != 2 {
		t.Fatal("compound")
	}
	mustPanic(t, func() { _ = CompoundDivPanic(1, 0) })
	if CompoundDivPanic(6, 2) != 3 {
		t.Fatal("compound div")
	}
	if CompoundAddZero(5) != 0 {
		t.Fatal("compound mul zero")
	}
	arr := []int{10, 20, 30}
	if CompoundNestedLHS(arr, 1) != 32 || arr[2] != 32 || arr[0] != 10 {
		t.Fatal("compound nested lhs")
	}
	mustPanic(t, func() { _ = LogicLeftPanic(nil, true) })
	if !LogicLeftPanic([]bool{true}, true) || LogicLeftPanic([]bool{false}, true) {
		t.Fatal("logic left panic")
	}
	mustPanic(t, func() { _ = StructIfacePanic(Box{[]int{1}}, Box{[]int{1}}, true) })
	if !StructIfacePanic(Box{1}, Box{1}, true) {
		t.Fatal("struct iface")
	}
	mustPanic(t, func() { _ = ArrayIfacePanic([1]any{[]int{1}}, [1]any{[]int{1}}, false) })
	if !ArrayIfacePanic([1]any{1}, [1]any{2}, true) {
		t.Fatal("array iface")
	}
	if TypeSwitchLocal(false, true) || !TypeSwitchLocal(true, true) {
		t.Fatal("type switch local")
	}
	if !ShadowedBoolLogic(1, true, true) || ShadowedBoolLogic(0, true, true) {
		t.Fatal("shadowed bool")
	}
	if UntypedRuneShift(1) != 0 {
		t.Fatal("untyped rune shift")
	}
}
`)
	exclude := []string{"empty-block", "statement-remove", "discard-assignment", "return-zero", "integer-step", "string-literal"}

	fullRep, err := Run(context.Background(), Config{
		Dir:            dir,
		Patterns:       []string{"."},
		Full:           true,
		Jobs:           2,
		Audit:          4,
		KilledFindings: true,
		ExcludeRules:   exclude,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fullRep.Incomplete != 0 {
		t.Fatalf("expected 0 incomplete in --full, got %d: %+v", fullRep.Incomplete, fullRep.Findings)
	}
	if fullRep.Audited != 4 {
		t.Fatalf("expected Audited=4, got %d", fullRep.Audited)
	}
	if fullRep.Skipped == 0 {
		t.Fatalf("expected observational pruning to skip replays (Skipped > 0), got 0")
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
	if fullRep.SourceRuns >= srcRep.SourceRuns/2 {
		t.Fatalf("expected --full to cut source runs by >50%%: full=%d source-only=%d", fullRep.SourceRuns, srcRep.SourceRuns)
	}

	verdict := func(status string) string {
		switch status {
		case "input-gap", "assertion-gap", "source-survivor":
			return "survived"
		}
		return status
	}
	fullMap := make(map[string]Finding, len(fullRep.Findings))
	for _, f := range fullRep.Findings {
		if f.Rule == "margin" {
			continue
		}
		fullMap[f.ID] = f
	}
	srcMap := make(map[string]Finding, len(srcRep.Findings))
	for _, f := range srcRep.Findings {
		srcMap[f.ID] = f
	}
	isCmpOp := func(op string) bool {
		switch op {
		case "<", "<=", ">", ">=", "==", "!=":
			return true
		}
		return false
	}
	matched := 0
	for id, want := range srcMap {
		got, ok := fullMap[id]
		if !ok {
			if want.Rule == "source-operator" && isCmpOp(want.Expression) {
				continue
			}
			t.Errorf("missing candidate in --full: %s:%d [%s] (%s -> %s)", want.File, want.Line, want.Rule, want.Expression, want.Mutation)
			continue
		}
		matched++
		if verdict(got.Status) != verdict(want.Status) {
			t.Errorf("verdict mismatch for %s:%d [%s] (%s -> %s): full=%s (%s), source-only=%s (%s)",
				want.File, want.Line, want.Rule, want.Expression, want.Mutation, got.Status, got.Evidence, want.Status, want.Evidence)
		}
	}
	if matched == 0 {
		t.Fatal("expected matching findings between --full and --source-only")
	}

	// Verify --verify-survivors disables observational pruning for schemata.
	verifyRep, err := Run(context.Background(), Config{
		Dir:             dir,
		Patterns:        []string{"."},
		Full:            true,
		VerifySurvivors: true,
		Jobs:            2,
		KilledFindings:  true,
		ExcludeRules:    exclude,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifyRep.Incomplete != 0 {
		t.Fatalf("expected 0 incomplete with --verify-survivors, got %d", verifyRep.Incomplete)
	}
	if verifyRep.Replays <= fullRep.Replays {
		t.Fatalf("expected --verify-survivors to perform more replays than pruned --full: verify=%d full=%d", verifyRep.Replays, fullRep.Replays)
	}
}

func TestFullSchemataPICTRows(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "verification", "pict", "full-schemata.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected PICT rows in full-schemata.tsv, got %d lines", len(lines))
	}
	for i, line := range lines[1:] {
		cols := strings.Split(line, "\t")
		if len(cols) != 4 {
			t.Fatalf("row %d: expected 4 columns, got %d (%q)", i+1, len(cols), line)
		}
		siteKind, obs, safety, mode := cols[0], cols[1], cols[2], cols[3]
		t.Run(strings.Join(cols, "/"), func(t *testing.T) {
			pure := safety == "pure-droppable"
			var site target.Site
			var op uint64
			var seen analysis.Summary
			switch siteKind {
			case "nested-arith-in-cmp":
				site = target.Site{Arith: true, ArithOp: 6}
				op = 7
				switch obs {
				case "infecting":
					seen = analysis.Summary{Flags: 1, Below: 1 << op}
				case "non-infecting":
					seen = analysis.Summary{Flags: 1, Below: 0}
				case "unreached":
					seen = analysis.Summary{}
				}
			case "logic-and":
				site = target.Site{Logic: true, ArithOp: 1, PureLeft: pure, PureRight: pure}
				op = 1
				switch obs {
				case "infecting":
					seen = analysis.Summary{Flags: 32 | 2}
				case "non-infecting":
					seen = analysis.Summary{Flags: 32 | 4}
				case "short-circuit":
					seen = analysis.Summary{Flags: 32 | 1}
				case "unreached":
					seen = analysis.Summary{}
				}
			case "logic-or":
				site = target.Site{Logic: true, ArithOp: 2, PureLeft: pure, PureRight: pure}
				op = 1
				switch obs {
				case "infecting":
					seen = analysis.Summary{Flags: 32 | 2}
				case "non-infecting":
					seen = analysis.Summary{Flags: 32 | 4}
				case "short-circuit":
					seen = analysis.Summary{Flags: 32 | 1}
				case "unreached":
					seen = analysis.Summary{}
				}
			case "bool-cond":
				site = target.Site{Bool: true, Pure: pure}
				op = 2
				switch obs {
				case "infecting":
					seen = analysis.Summary{Flags: 2}
				case "non-infecting":
					seen = analysis.Summary{Flags: 1}
				case "unreached":
					seen = analysis.Summary{}
				}
			case "shift":
				site = target.Site{Shift: true, ArithOp: 10}
				op = 11
				switch obs {
				case "infecting":
					seen = analysis.Summary{Flags: 1, Below: 1 << op}
				case "non-infecting":
					seen = analysis.Summary{Flags: 1, Below: 0}
				case "unreached":
					seen = analysis.Summary{}
				}
			case "compound-assign":
				site = target.Site{Assign: true, ArithOp: 7}
				op = 10
				switch obs {
				case "infecting":
					seen = analysis.Summary{Flags: 1, Below: 1 << op}
				case "non-infecting":
					seen = analysis.Summary{Flags: 1, Below: 0}
				case "unreached":
					seen = analysis.Summary{}
				}
			default:
				t.Fatalf("unknown SiteKind %q", siteKind)
			}

			infects := schemataInfects(&site, op, seen)
			wantInfects := obs == "infecting" || obs == "short-circuit" ||
				(obs == "non-infecting" && !pure && (siteKind == "logic-and" || siteKind == "logic-or" || siteKind == "bool-cond"))
			if obs == "unreached" {
				wantInfects = false
			}
			if infects != wantInfects {
				t.Fatalf("schemataInfects = %v, want %v (site=%+v, seen=%+v)", infects, wantInfects, site, seen)
			}
			cfg := Config{
				Full:            true,
				VerifySurvivors: mode == "verify-survivors",
				Conventional:    mode == "conventional",
			}
			sess := &session{cfg: cfg}
			job := sourceJob{
				edit:    &target.Edit{Site: &site, ArithOp: op},
				finding: Finding{Rule: "test"},
			}
			if obs == "unreached" && mode == "full" {
				res := sess.runSourceComparison(context.Background(), job, nil, commandResult{})
				if res.finding.Status != "source-survivor" || res.finding.Evidence != "observed" || res.skipped != 1 {
					t.Fatalf("expected observed skip for unreached schemata in full mode, got %+v", res)
				}
			} else if !infects && obs != "unreached" && mode == "full" {
				bin := &consumer{pkg: "example.org/p", seen: map[int]analysis.Summary{site.ID: seen}}
				res := sess.runSourceComparison(context.Background(), job, []*consumer{bin}, commandResult{})
				if res.finding.Status != "source-survivor" || res.finding.Evidence != "observed" || res.skipped != 1 || res.replays != 0 {
					t.Fatalf("expected observed skip for non-infecting schemata in full mode, got %+v", res)
				}
			}
		})
	}
}
