// Package analysis computes mutation candidates from integer comparison observations.
package analysis

type Operator uint8

const (
	LT Operator = iota + 1
	LE
	GT
	GE
	EQ
	NE
	AlwaysFalse
	AlwaysTrue
)

const (
	Less uint64 = 1 << iota
	Equal
	Greater
)

func (op Operator) String() string {
	return [...]string{"", "<", "<=", ">", ">=", "==", "!=", "false", "true"}[op]
}

func (op Operator) Mask() uint64 {
	return [...]uint64{0, Less, Less | Equal, Greater, Greater | Equal, Equal, Less | Greater, 0, Less | Equal | Greater}[op]
}

func (op Operator) Negate() Operator {
	return [...]Operator{0, GE, GT, LE, LT, NE, EQ, AlwaysTrue, AlwaysFalse}[op]
}

func (op Operator) Reverse() Operator {
	return [...]Operator{0, GT, GE, LT, LE, EQ, NE, AlwaysFalse, AlwaysTrue}[op]
}

func (op Operator) Boundary() Operator {
	return [...]Operator{0, LE, LT, GE, GT, 0, 0, 0, 0}[op]
}

func RankSigned(value int64) uint64 {
	return uint64(value) ^ (1 << 63)
}

func UnrankSigned(rank uint64) int64 {
	return int64(rank ^ (1 << 63))
}

type Domain struct {
	Low, High, Pivot uint64
	Signed           bool
}

type Summary struct {
	Flags, Below, Above uint64
}

func (s *Summary) Observe(lhs, rhs uint64) {
	if lhs < rhs {
		if s.Flags&Less == 0 || lhs > s.Below {
			s.Below = lhs
		}
		s.Flags |= Less
	} else if lhs > rhs {
		if s.Flags&Greater == 0 || lhs < s.Above {
			s.Above = lhs
		}
		s.Flags |= Greater
	} else {
		s.Flags |= Equal
	}
}

func (s *Summary) Merge(other Summary) {
	if other.Flags&Less != 0 && (s.Flags&Less == 0 || other.Below > s.Below) {
		s.Below = other.Below
	}
	if other.Flags&Greater != 0 && (s.Flags&Greater == 0 || other.Above < s.Above) {
		s.Above = other.Above
	}
	s.Flags |= other.Flags
}

type Candidate struct {
	Kind    string
	Op      Operator
	Pivot   uint64
	Shift   int
	Witness *uint64
}

func (c Candidate) Infects(original Operator, seen Summary) bool {
	if c.Shift == 0 {
		return seen.Flags&(c.Op.Mask()^original.Mask()) != 0
	}
	checksNeighbor := original == EQ || original == NE
	if !checksNeighbor && (original == GE || original == LT) == (c.Shift > 0) {
		return seen.Flags&Equal != 0
	}
	if checksNeighbor && seen.Flags&Equal != 0 {
		return true
	}
	if c.Shift < 0 {
		return seen.Flags&Less != 0 && seen.Below == c.Pivot
	}
	return seen.Flags&Greater != 0 && seen.Above == c.Pivot
}

func Plan(op Operator, domain *Domain, seen Summary, targeted bool) []Candidate {
	if seen.Flags == 0 {
		return nil
	}
	var candidates []Candidate
	var pivot uint64
	if domain != nil {
		pivot = domain.Pivot
	}
	if opposite := op.Boundary(); opposite != 0 {
		boundary := Candidate{Kind: "boundary", Op: opposite, Pivot: pivot}
		if domain != nil {
			boundary.Witness = &pivot
		}
		candidates = append(candidates, boundary)
	}
	candidates = append(candidates, Candidate{Kind: "negation", Op: op.Negate(), Pivot: pivot})
	if !targeted || domain == nil || op.Boundary() == 0 {
		return candidates
	}

	// The boundary operator already represents the adjacent change on one side.
	if (op == GE || op == LT) && domain.Low < pivot {
		value := pivot - 1
		candidate := Candidate{Kind: "threshold", Op: op, Pivot: value, Shift: -1, Witness: &value}
		if !candidate.Infects(op, seen) {
			candidates = append(candidates, candidate)
		}
	} else if (op == GT || op == LE) && pivot < domain.High {
		value := pivot + 1
		candidate := Candidate{Kind: "threshold", Op: op, Pivot: value, Shift: 1, Witness: &value}
		if !candidate.Infects(op, seen) {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

// ThresholdInterval returns the inclusive set of thresholds consistent with the observed decisions.
func ThresholdInterval(op Operator, d Domain, seen Summary) (low, high uint64, ok bool) {
	if op.Boundary() == 0 {
		return 0, 0, false
	}
	low, high = d.Low, d.High
	if op == GE || op == LT {
		if seen.Flags&Less != 0 {
			low = seen.Below + 1
		}
		if seen.Flags&Equal != 0 {
			high = d.Pivot
		} else if seen.Flags&Greater != 0 {
			high = seen.Above
		}
	} else {
		if seen.Flags&Equal != 0 {
			low = d.Pivot
		} else if seen.Flags&Less != 0 {
			low = seen.Below
		}
		if seen.Flags&Greater != 0 {
			high = seen.Above - 1
		}
	}
	return low, high, true
}

func Evaluate(op Operator, lhs, rhs uint64) bool {
	switch {
	case lhs < rhs:
		return op.Mask()&Less != 0
	case lhs > rhs:
		return op.Mask()&Greater != 0
	default:
		return op.Mask()&Equal != 0
	}
}
