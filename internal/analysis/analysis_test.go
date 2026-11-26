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
}
