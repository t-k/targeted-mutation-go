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
	for _, candidate := range plan {
		changes := false
		for _, value := range samples {
			changes = changes || evaluateOracle(op, value, pivot) != evaluateOracle(candidate.Op, value, candidate.Pivot)
		}
		if got := candidate.Infects(op, summary); got != changes {
			t.Fatalf("%v pivot %d samples %v: %+v infection=%v want %v", op, pivot, samples, candidate, got, changes)
		}
		if candidate.Witness != nil {
			value := *candidate.Witness
			if value < low || value > high || evaluateOracle(op, value, pivot) == evaluateOracle(candidate.Op, value, candidate.Pivot) {
				t.Fatalf("invalid witness %d for %+v", value, candidate)
			}
		}
	}
	if len(samples) == 0 && len(plan) != 0 {
		t.Fatalf("unreached comparison planned %v", plan)
	}
	if len(samples) == 0 || op == EQ || op == NE {
		return
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
