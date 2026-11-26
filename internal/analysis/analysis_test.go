package analysis

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestThresholdPICT(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/threshold.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] {
		t.Run(strings.ReplaceAll(row, "\t", "/"), func(t *testing.T) {
			f := strings.Fields(row)
			op := map[string]Operator{"ge": GE, "gt": GT, "le": LE, "lt": LT}[f[0]]
			var lo, hi uint64
			switch f[1] {
			case "signed":
				lo, hi = RankSigned(-128), RankSigned(127)
			case "unsigned":
				lo, hi = 0, 255
			case "full":
				lo, hi = 0, ^uint64(0)
			}
			pivot := lo + (hi-lo)/2
			switch f[2] {
			case "low":
				pivot = lo
			case "high":
				pivot = hi
			}
			var samples []uint64
			switch f[3] {
			case "both":
				samples = []uint64{pivot - 2, pivot + 2}
			case "equal":
				samples = []uint64{pivot}
			case "below":
				samples = []uint64{pivot - 1, lo}
			case "above":
				samples = []uint64{hi, pivot + 1}
			}
			checkPlan(t, op, lo, hi, pivot, samples)
		})
	}
}

func TestThresholdExhaustive(t *testing.T) {
	const size = 8
	for mask := range 1 << size {
		var samples []uint64
		for value := range size {
			if mask&(1<<value) != 0 {
				samples = append(samples, uint64(value))
			}
		}
		for pivot := range size {
			for _, op := range []Operator{LT, LE, GT, GE, EQ, NE} {
				checkPlan(t, op, 0, size-1, uint64(pivot), samples)
			}
		}
	}
}

func checkPlan(t *testing.T, op Operator, low, high, pivot uint64, samples []uint64) {
	t.Helper()
	var summary Summary
	for _, value := range samples {
		summary.Observe(value, pivot)
	}
	plan := Plan(op, &Domain{Low: low, High: high, Pivot: pivot}, summary, true)
	candidates := append([]Candidate(nil), plan...)
	if pivot > low {
		at := pivot - 1
		candidates = append(candidates, Candidate{Op: op, Pivot: at, Shift: -1, Witness: &at})
	}
	if pivot < high {
		at := pivot + 1
		candidates = append(candidates, Candidate{Op: op, Pivot: at, Shift: 1, Witness: &at})
	}
	for _, candidate := range candidates {
		changes := false
		for _, value := range samples {
			changes = changes || evaluateOracle(op, value, pivot) != evaluateOracle(candidate.Op, value, candidate.Pivot)
		}
		if got := candidate.Infects(op, summary); got != changes {
			t.Fatalf("%v pivot %d samples %v: %+v infection=%v want %v", op, pivot, samples, candidate, got, changes)
		}
		if candidate.Kind != "" && candidate.Witness != nil {
			value := *candidate.Witness
			if value < low || value > high || evaluateOracle(op, value, pivot) == evaluateOracle(candidate.Op, value, candidate.Pivot) {
				t.Fatalf("invalid witness %d for %+v", value, candidate)
			}
		}
	}
	if len(samples) == 0 && len(plan) != 0 {
		t.Fatalf("unreached comparison planned %v", plan)
	}
	if len(samples) == 0 {
		return
	}
	if plan[0].Kind != "boundary" && op.Boundary() != 0 {
		t.Fatalf("unexpected candidate list %v", plan)
	}
	negations := 0
	for _, c := range plan {
		if c.Kind == "negation" {
			negations++
			if !c.Infects(op, summary) {
				t.Fatalf("negation does not affect observations: %v", c)
			}
		}
	}
	if negations != 1 {
		t.Fatalf("expected exactly one negation: %v", plan)
	}
	if op == EQ || op == NE {
		return
	}
	lo, hi, ok := ThresholdInterval(op, Domain{Low: low, High: high, Pivot: pivot}, summary)
	if !ok {
		t.Fatal("missing ordered interval")
	}
	checkInterval := func(alternative uint64) {
		t.Helper()
		if alternative < low || alternative > high {
			return
		}
		consistent := true
		for _, value := range samples {
			consistent = consistent && evaluateOracle(op, value, pivot) == evaluateOracle(op, value, alternative)
		}
		if consistent != (lo <= alternative && alternative <= hi) {
			t.Fatalf("interval [%d,%d] misclassified %d: op %v samples %v", lo, hi, alternative, op, samples)
		}
	}
	for _, alternative := range []uint64{low, lo - 1, lo, pivot, hi, hi + 1, high} {
		checkInterval(alternative)
	}
	if high-low <= 8 {
		for alternative := low; ; alternative++ {
			checkInterval(alternative)
			if alternative == high {
				break
			}
		}
	}
	for _, alternative := range []uint64{pivot - 1, pivot + 1} {
		if alternative < low || alternative > high || alternative == pivot || (pivot == 0 && alternative == ^uint64(0)) || (pivot == ^uint64(0) && alternative == 0) {
			continue
		}
		for _, value := range samples {
			if evaluateOracle(op, value, pivot) != evaluateOracle(op, value, alternative) {
				goto next
			}
		}
		for _, candidate := range plan {
			if (candidate.Kind == "threshold" && candidate.Pivot == alternative) ||
				(candidate.Kind == "boundary" && candidate.Infects(op, summary) == false &&
					((op == GE || op == LT) == (alternative > pivot))) {
				goto next
			}
		}
		t.Fatalf("missing equivalent adjacent threshold %s for %v samples %v", strconv.FormatUint(alternative, 10), op, samples)
	next:
	}
}

func evaluateOracle(op Operator, x, y uint64) bool {
	switch op {
	case LT:
		return x < y
	case LE:
		return x <= y
	case GT:
		return x > y
	case GE:
		return x >= y
	case EQ:
		return x == y
	case NE:
		return x != y
	default:
		panic("invalid operator")
	}
}

func TestThresholdIntervalRejectsEquality(t *testing.T) {
	domain := Domain{Low: 0, High: 7, Pivot: 4}
	var observed Summary
	observed.Observe(4, 4)
	for _, op := range []Operator{EQ, NE} {
		if _, _, ok := ThresholdInterval(op, domain, observed); ok {
			t.Fatalf("%v cannot be represented as a general interval", op)
		}
	}
}

func TestRankAndMergedExtremes(t *testing.T) {
	var previous uint64
	for i, value := range []int64{-1 << 63, -1, 0, 1, 1<<63 - 1} {
		rank := RankSigned(value)
		if UnrankSigned(rank) != value || i > 0 && rank <= previous {
			t.Fatalf("invalid integer rank %d for %d", rank, value)
		}
		previous = rank
	}
	a := Summary{Flags: Less | Greater, Below: 0, Above: ^uint64(0)}
	b := Summary{Flags: Less | Equal | Greater, Below: 10, Above: 12}
	a.Merge(b)
	if a != b {
		t.Fatalf("got merged summary %+v, want %+v", a, b)
	}
	var s1, s2 Summary
	s1.Observe(3, 10)
	s1.Observe(15, 10)
	s2.Observe(9, 10)
	s2.Observe(11, 10)
	s1.Merge(s2)
	if s1.Below != 9 || s1.BelowGap != 1 || s1.Above != 11 || s1.AboveGap != 1 {
		t.Fatalf("unexpected merged gaps: %+v", s1)
	}
}

func TestPrevAndNextPossibleExhaustive8Bit(t *testing.T) {
	masks := []uint64{0, 1, 0x0f, 0xf0, 0x55, 0xfe, ^uint64(5), ^uint64(0x18)}
	for _, signed := range []bool{false, true} {
		typeLo, typeHi := uint64(0), uint64(255)
		if signed {
			typeLo, typeHi = RankSigned(-128), RankSigned(127)
		}
		bounds := [][2]uint64{
			{typeLo, typeHi},
			{typeLo + 3, typeHi - 5},
			{typeLo + 130, typeHi - 10},
		}
		if signed {
			bounds = append(bounds,
				[2]uint64{RankSigned(0), RankSigned(127)},
				[2]uint64{RankSigned(-128), RankSigned(-1)},
				[2]uint64{RankSigned(-10), RankSigned(10)},
			)
		}
		for _, b := range bounds {
			for _, zeroBits := range masks {
				d := Domain{
					Low:        typeLo,
					High:       typeHi,
					WidthMask:  0xff,
					Signed:     signed,
					Attainable: &Attainable{Low: b[0], High: b[1], ZeroBits: zeroBits},
				}
				for raw := range 256 {
					r := uint64(raw)
					if signed {
						r = RankSigned(int64(int8(raw)))
					}
					// Brute-force PrevPossible
					wantPrev, wantPrevOK := uint64(0), false
					for cand := r; ; cand-- {
						if d.Possible(cand) {
							wantPrev, wantPrevOK = cand, true
							break
						}
						if cand == typeLo {
							break
						}
					}
					gotPrev, gotPrevOK := d.PrevPossible(r)
					if gotPrev != wantPrev || gotPrevOK != wantPrevOK {
						t.Fatalf("signed=%v b=%v zero=%#x PrevPossible(%#x) = (%#x, %v), want (%#x, %v)",
							signed, b, zeroBits, r, gotPrev, gotPrevOK, wantPrev, wantPrevOK)
					}
					// Brute-force NextPossible
					wantNext, wantNextOK := uint64(0), false
					for cand := r; ; cand++ {
						if d.Possible(cand) {
							wantNext, wantNextOK = cand, true
							break
						}
						if cand == typeHi {
							break
						}
					}
					gotNext, gotNextOK := d.NextPossible(r)
					if gotNext != wantNext || gotNextOK != wantNextOK {
						t.Fatalf("signed=%v b=%v zero=%#x NextPossible(%#x) = (%#x, %v), want (%#x, %v)",
							signed, b, zeroBits, r, gotNext, gotNextOK, wantNext, wantNextOK)
					}
				}
			}
		}
	}
}

func TestPrevAndNextPossible64BitEdgeCases(t *testing.T) {
	u := Domain{Low: 0, High: ^uint64(0), Attainable: &Attainable{Low: 0, High: ^uint64(0), ZeroBits: 1}}
	if got, ok := u.NextPossible(^uint64(0)); ok {
		t.Fatalf("expected no next possible above MaxUint64 with odd forbidden, got %#x", got)
	}
	if got, ok := u.PrevPossible(^uint64(0)); !ok || got != ^uint64(1) {
		t.Fatalf("PrevPossible(MaxUint64) = (%#x, %v), want (%#x, true)", got, ok, ^uint64(1))
	}
	s := Domain{Low: RankSigned(-1 << 63), High: RankSigned(1<<63 - 1), Signed: true, Attainable: &Attainable{Low: RankSigned(-1 << 63), High: RankSigned(1<<63 - 1), ZeroBits: 1}}
	if got, ok := s.PrevPossible(RankSigned(-1)); !ok || got != RankSigned(-2) {
		t.Fatalf("PrevPossible(-1) = (%#x, %v), want (%#x, true)", got, ok, RankSigned(-2))
	}
	if got, ok := s.NextPossible(RankSigned(-1)); !ok || got != RankSigned(0) {
		t.Fatalf("NextPossible(-1) = (%#x, %v), want (%#x, true)", got, ok, RankSigned(0))
	}
	if got, ok := s.NextPossible(RankSigned(1<<63 - 1)); ok {
		t.Fatalf("expected no next possible above MaxInt64 with bit 0 forbidden, got %#x", got)
	}
}

func TestInfectsExhaustiveWithHolesAndMargins(t *testing.T) {
	const size = 8
	for mask := range 1 << size {
		var samples []uint64
		for value := range size {
			if mask&(1<<value) != 0 {
				samples = append(samples, uint64(value))
			}
		}
		for pivot := range size {
			var summary Summary
			for _, value := range samples {
				summary.Observe(value, uint64(pivot))
			}
			for _, op := range []Operator{LT, LE, GT, GE, EQ, NE} {
				var candidates []Candidate
				for target := range size {
					if target < pivot {
						if op == GE || op == LT || target == pivot-1 {
							candidates = append(candidates, Candidate{Kind: "threshold", Op: op, Pivot: uint64(target), Shift: -1})
						}
						if b := op.Boundary(); b != 0 && (op == GT || op == LE) {
							candidates = append(candidates, Candidate{Kind: "threshold", Op: b, Pivot: uint64(target), Shift: -1})
						}
					} else if target > pivot {
						if op == GT || op == LE || target == pivot+1 {
							candidates = append(candidates, Candidate{Kind: "threshold", Op: op, Pivot: uint64(target), Shift: 1})
						}
						if b := op.Boundary(); b != 0 && (op == GE || op == LT) {
							candidates = append(candidates, Candidate{Kind: "threshold", Op: b, Pivot: uint64(target), Shift: 1})
						}
					}
				}
				for _, c := range candidates {
					changes := false
					for _, value := range samples {
						origVal := evaluateOracle(op, value, uint64(pivot))
						mutVal := evaluateOracle(c.Op, value, c.Pivot)
						if origVal != mutVal {
							// Note: when c.Op == op.Boundary() and target crosses over pivot,
							// origPivot itself wasn't in the hole in this loop; skip when value == uint64(pivot) for c.Op != op && op != EQ && op != NE.
							if c.Op != op && value == uint64(pivot) {
								goto skipCand
							}
							changes = true
						}
					}
					if got := c.Infects(op, summary); got != changes {
						t.Fatalf("%v pivot=%d samples=%v cand=%+v: Infects=%v want %v", op, pivot, samples, c, got, changes)
					}
				skipCand:
				}
			}
		}
	}
}

func TestPlanSiteFloatAndMargin(t *testing.T) {
	// Float: NaN (Unordered) distinguishes NE from ordered/EQ, but does not distinguish LT from GE.
	nanOnly := Summary{Flags: Unordered}
	floatCands := PlanSite(LT, nil, false, true, nanOnly, true)
	if len(floatCands) != 2 || floatCands[0].Kind != "boundary" || !floatCands[0].Float || floatCands[1].Kind != "negation" || !floatCands[1].Float {
		t.Fatalf("unexpected float candidates: %+v", floatCands)
	}
	if floatCands[0].Infects(LT, nanOnly) || floatCands[1].Infects(LT, nanOnly) {
		t.Fatalf("NaN alone should not kill LT->LE or LT->GE")
	}
	if !(Candidate{Op: LT, Float: true}).Infects(NE, nanOnly) {
		t.Fatalf("NaN should kill NE->LT")
	}
	if (Candidate{Op: LE, Float: true}).Infects(EQ, nanOnly) {
		t.Fatalf("NaN should not kill EQ->LE")
	}
	if prev := PreviewSite(LT, nil, false, true, true); len(prev) != 2 {
		t.Fatalf("PreviewSite float = %+v", prev)
	}

	// Margin: variable RHS emits margin candidate unless gap == 1 was observed.
	if prev := PreviewSite(GE, nil, true, false, true); len(prev) != 3 || prev[2].Kind != "margin" || prev[2].Shift != -1 {
		t.Fatalf("PreviewSite margin GE = %+v", prev)
	}
	if prev := PreviewSite(EQ, nil, true, false, true); len(prev) != 1 {
		t.Fatalf("PreviewSite margin EQ should not emit margin: %+v", prev)
	}
	var far Summary
	far.Observe(5, 10)
	if cands := PlanSite(GE, nil, true, false, far, true); len(cands) != 3 || cands[2].Kind != "margin" {
		t.Fatalf("expected margin when BelowGap=5: %+v", cands)
	}
	var tight Summary
	tight.Observe(9, 10)
	if cands := PlanSite(GE, nil, true, false, tight, true); len(cands) != 2 {
		t.Fatalf("expected margin dropped when BelowGap=1: %+v", cands)
	}
	var aboveTight Summary
	aboveTight.Observe(11, 10)
	if cands := PlanSite(LE, nil, true, false, aboveTight, true); len(cands) != 2 {
		t.Fatalf("expected margin-up dropped when AboveGap=1: %+v", cands)
	}

	// Hole-jumping: (x & 0xF0) >= 0x18 (24) drops boundary, emits GE 16 and GT 32.
	d := Domain{
		Low:        0,
		High:       255,
		WidthMask:  0xff,
		Pivot:      0x18,
		Attainable: &Attainable{Low: 0, High: 0xf0, ZeroBits: ^uint64(0xf0)},
	}
	var seenZero Summary
	seenZero.Observe(0, 0x18)
	cands := Plan(GE, &d, seenZero, true)
	if len(cands) != 3 || cands[0].Kind != "negation" ||
		cands[1].Kind != "threshold" || cands[1].Op != GE || cands[1].Pivot != 0x10 ||
		cands[2].Kind != "threshold" || cands[2].Op != GT || cands[2].Pivot != 0x20 {
		t.Fatalf("unexpected hole-jumping plan: %+v", cands)
	}
}
