package target

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
)

func TestAttainabilityPICT(t *testing.T) {
	table, err := os.ReadFile("../../verification/pict/attainability.tsv")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(table)), "\n")[1:]
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/attainable\n\ngo 1.26.0\n")
	var source strings.Builder
	source.WriteString("package attainable\n")
	for i, row := range rows {
		fields := strings.Fields(row)
		typ, lhs := "int", "x"
		if strings.HasPrefix(fields[0], "unsigned-") {
			typ = "uint"
		}
		switch {
		case strings.HasSuffix(fields[0], "-mask"):
			lhs = "x & 5"
		case fields[0] == "negative-remainder":
			lhs = "x % -3"
		case strings.HasSuffix(fields[0], "-remainder"):
			lhs = "x % 3"
		}
		pivot := map[string]int{"inside": 1, "outside": 8, "hole": 2}[fields[1]]
		fmt.Fprintf(&source, "func F%d(x %s) bool { return %s >= %d }\n", i, typ, lhs, pivot)
	}
	write(t, dir, "test.go", source.String())
	packages, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{})
	if err != nil {
		t.Fatal(err)
	}
	sites := packages[0].Files[0].Sites
	if len(sites) != len(rows) {
		t.Fatalf("discovered %d sites for %d rows", len(sites), len(rows))
	}
	for i, row := range rows {
		fields := strings.Fields(row)
		site := sites[i]
		targeted := fields[2] == "targeted"
		var seen analysis.Summary
		zero := uint64(0)
		if site.Domain.Signed {
			zero = analysis.RankSigned(0)
		}
		seen.Observe(zero, site.Domain.Pivot)
		present := false
		for _, candidate := range analysis.Plan(site.Op, site.Domain, seen, targeted) {
			present = present || candidate.Kind == "boundary"
		}
		if present != (!targeted || fields[1] == "inside") {
			t.Errorf("%s: boundary candidate present = %t", row, present)
		}
	}
}

func TestAdjacentAttainabilityUsesTheChangedRank(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/adjacent\n\ngo 1.26.0\n")
	write(t, dir, "adjacent.go", `package adjacent
func Keep(x uint8) bool { return x & 5 >= 2 }
func HoleJump(x uint8) bool { return x & 5 >= 3 }
func Min(x int8) bool { return x % -128 >= -128 }
`)
	packages, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{})
	if err != nil {
		t.Fatal(err)
	}
	for i, site := range packages[0].Files[0].Sites {
		seen := analysis.Summary{Flags: analysis.Less}
		if site.Domain.Signed {
			seen = analysis.Summary{Flags: analysis.Greater, Above: analysis.RankSigned(0)}
		}
		var boundary, adjacent bool
		var threshold uint64
		for _, candidate := range analysis.Plan(site.Op, site.Domain, seen, true) {
			boundary = boundary || candidate.Kind == "boundary"
			if candidate.Kind == "threshold" && candidate.Shift < 0 {
				adjacent = true
				threshold = candidate.Pivot
			}
		}
		if i == 0 && (boundary || !adjacent || threshold != 1) {
			t.Errorf("%s: boundary %t, adjacent %t, threshold %d", site.Expression, boundary, adjacent, threshold)
		}
		if i == 1 && (boundary || !adjacent || threshold != 1) {
			t.Errorf("%s: expected hole-jumped threshold 1, got boundary %t adjacent %t threshold %d", site.Expression, boundary, adjacent, threshold)
		}
		if i == 2 && (boundary || adjacent) {
			t.Errorf("%s: unexpected boundary %t adjacent %t", site.Expression, boundary, adjacent)
		}
	}
}

func TestExtendedAttainabilityConstraints(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/ext\n\ngo 1.26.0\n")
	write(t, dir, "ext.go", `package ext
func MaskHigh(x uint8) bool { return (x & 0xF0) >= 0x20 }
func NegMask(x int8) bool { return (x & -2) >= 1 }
func AndNot(x int8) bool { return (x &^ 1) >= 1 }
func NestedAnd(x uint8) bool { return ((x & 0x0f) & 0x06) >= 2 }
func ShiftLeft(x uint8) bool { return (x << 2) >= 4 }
func SliceLen(xs []int) bool { return len(xs) >= 0 }
`)
	packages, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{})
	if err != nil {
		t.Fatal(err)
	}
	sites := packages[0].Files[0].Sites
	if len(sites) != 6 {
		t.Fatalf("discovered %d sites, want 6", len(sites))
	}

	// 0: (x & 0xF0) >= 0x20 -> PrevPossible(0x1F) == 0x10
	var seen0 analysis.Summary
	seen0.Observe(0, 0x20)
	cands0 := analysis.Plan(sites[0].Op, sites[0].Domain, seen0, true)
	if !hasThreshold(cands0, analysis.GE, 0x10) {
		t.Fatalf("MaskHigh missing threshold 0x10: %+v", cands0)
	}

	// 1: (x & -2) >= 1 -> boundary dropped (1 is impossible), thresholds GE 0 and GT 2
	var seen1 analysis.Summary
	seen1.Observe(analysis.RankSigned(-2), analysis.RankSigned(1))
	cands1 := analysis.Plan(sites[1].Op, sites[1].Domain, seen1, true)
	if hasKind(cands1, "boundary") || !hasThreshold(cands1, analysis.GE, analysis.RankSigned(0)) || !hasThreshold(cands1, analysis.GT, analysis.RankSigned(2)) {
		t.Fatalf("NegMask unexpected plan: %+v", cands1)
	}

	// 2: (x &^ 1) >= 1 -> boundary dropped, thresholds GE 0 and GT 2
	cands2 := analysis.Plan(sites[2].Op, sites[2].Domain, seen1, true)
	if hasKind(cands2, "boundary") || !hasThreshold(cands2, analysis.GE, analysis.RankSigned(0)) || !hasThreshold(cands2, analysis.GT, analysis.RankSigned(2)) {
		t.Fatalf("AndNot unexpected plan: %+v", cands2)
	}

	// 3: ((x & 0x0f) & 0x06) >= 2 -> PrevPossible(1) == 0
	var seen3 analysis.Summary
	seen3.Observe(0, 2)
	if got, ok := sites[3].Domain.PrevPossible(1); !ok || got != 0 {
		t.Fatalf("NestedAnd PrevPossible(1) = (%d, %v), want (0, true)", got, ok)
	}
	if sites[3].Domain.Possible(4) {
		// 4 = 0b100 is allowed (0x06 = 0b110), 8 is forbidden
	}
	if sites[3].Domain.Possible(8) {
		t.Fatalf("NestedAnd should forbid 8")
	}

	// 4: (x << 2) >= 4 -> PrevPossible(3) == 0
	if got, ok := sites[4].Domain.PrevPossible(3); !ok || got != 0 {
		t.Fatalf("ShiftLeft PrevPossible(3) = (%d, %v), want (0, true)", got, ok)
	}
	if sites[4].Domain.Possible(2) {
		t.Fatalf("ShiftLeft should forbid 2")
	}

	// 5: len(xs) >= 0 -> Low == RankSigned(0), so no negative threshold below 0
	if sites[5].Domain.Attainable == nil || sites[5].Domain.Attainable.Low != analysis.RankSigned(0) {
		t.Fatalf("SliceLen Attainable = %+v, want Low=RankSigned(0)", sites[5].Domain.Attainable)
	}
	if _, ok := sites[5].Domain.PrevPossible(analysis.RankSigned(-1)); ok {
		t.Fatalf("SliceLen should forbid negative ranks")
	}
	_ = seen3
}

func TestCompositeRemainderBitwiseExactnessAndLeftConstantFloat(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

type Score float32

func EvenMod4(x uint8) bool   { return (x<<1)%4 >= 2 }
func BitMod3(x uint8) bool    { return (x&4)%3 >= 1 }
func NegBitMod5(x int8) bool  { return (x&-124)%5 >= 0 }
func ShiftMask(x uint8) bool  { return ((x&5)<<1) >= 8 }
func LeftFloat(x Score) bool  { return 0.5 < x }
func ConstIdx(i int) bool     { return ("A"[i]<<1)%4 >= 2 }
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/exact\n\ngo 1.26.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || len(pkgs[0].Files) != 1 {
		t.Fatalf("unexpected packages: %+v", pkgs)
	}
	probeFiles := Runtime(pkgs[0])
	if len(probeFiles) != 1 {
		t.Fatalf("expected 1 probe runtime file, got %d", len(probeFiles))
	}
	probeBase := filepath.Base(probeFiles[0].Path)
	if probeBase[0] != '0' || !(probeBase < "0.go") {
		t.Fatalf("probe file %q must start with '0' (SafeArg) and sort before 0.go", probeBase)
	}

	var sites []*Site
	for _, s := range pkgs[0].Files[0].Sites {
		if !s.IsSchemata() {
			sites = append(sites, s)
		}
	}
	if len(sites) != 6 {
		t.Fatalf("expected 6 comparison sites, got %d (total %d)", len(sites), len(pkgs[0].Files[0].Sites))
	}
	if sites[5].Domain == nil || sites[5].Domain.Exact() {
		t.Fatalf("ConstIdx ('A'[i]<<1)%%4 must be marked Conservative (!Exact), got %+v", sites[5].Domain)
	}
	constCands := analysis.Plan(sites[5].Op, sites[5].Domain, analysis.Summary{Flags: analysis.Equal}, true)
	if hasKind(constCands, "threshold") {
		t.Fatalf("ConstIdx must not propose unreachable threshold witness: %+v", constCands)
	}

	// Exhaustively check uint8/int8 attainable sets against ground truth across all 256 inputs.
	checkUint8 := func(name string, d *analysis.Domain, fn func(uint8) uint8) {
		t.Helper()
		want := make(map[uint64]bool)
		for i := range 256 {
			want[uint64(fn(uint8(i)))] = true
		}
		for v := uint64(0); v < 256; v++ {
			if got := d.Possible(v); got != want[v] {
				t.Fatalf("%s: Possible(%d) = %v, want %v", name, v, got, want[v])
			}
		}
	}
	checkUint8("EvenMod4", sites[0].Domain, func(x uint8) uint8 { return (x << 1) % 4 })
	checkUint8("BitMod3", sites[1].Domain, func(x uint8) uint8 { return (x & 4) % 3 })
	checkUint8("ShiftMask", sites[3].Domain, func(x uint8) uint8 { return (x & 5) << 1 })

	wantInt8 := make(map[uint64]bool)
	for i := -128; i <= 127; i++ {
		wantInt8[analysis.RankSigned(int64((int8(i)&-124)%5))] = true
	}
	for i := -128; i <= 127; i++ {
		r := analysis.RankSigned(int64(i))
		if got := sites[2].Domain.Possible(r); got != wantInt8[r] {
			t.Fatalf("NegBitMod5: Possible(%d) = %v, want %v", i, got, wantInt8[r])
		}
	}

	// Verify (x<<1)%4 >= 2 jumps across impossible 1 to 0.
	cands := analysis.Plan(sites[0].Op, sites[0].Domain, analysis.Summary{Flags: analysis.Equal}, true)
	if hasThreshold(cands, analysis.GE, 1) || !hasThreshold(cands, analysis.GE, 0) {
		t.Fatalf("EvenMod4 should jump across 1 to 0, got %+v", cands)
	}

	// LeftFloat: 0.5 < x must reverse cleanly to x > 0.5 with Reversed == true.
	if !sites[4].Float || !sites[4].Reversed || sites[4].Left != "x" || sites[4].Right != "0.5" || sites[4].Op != analysis.GT {
		t.Fatalf("LeftFloat site corrupted: %+v", sites[4])
	}
	foundReverseGT := false
	for _, edit := range pkgs[0].Files[0].Edits {
		if edit.Site == sites[4] && edit.After == ">" {
			foundReverseGT = true
			if edit.Operator != analysis.LT {
				t.Fatalf("0.5 < x mutated to 0.5 > x must evaluate x < 0.5 (LT), got %v", edit.Operator)
			}
		}
	}
	if !foundReverseGT {
		t.Fatalf("missing probe-routed '>' edit for LeftFloat")
	}
}

func hasKind(cands []analysis.Candidate, kind string) bool {
	for _, c := range cands {
		if c.Kind == kind {
			return true
		}
	}
	return false
}

func hasThreshold(cands []analysis.Candidate, op analysis.Operator, pivot uint64) bool {
	for _, c := range cands {
		if c.Kind == "threshold" && c.Op == op && c.Pivot == pivot {
			return true
		}
	}
	return false
}
