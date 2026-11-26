// Package analysis computes mutation candidates from integer comparison observations.
package analysis

import (
	"math/bits"
	"slices"
)

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
	_
	_
	Unordered
)

func (op Operator) String() string {
	return [...]string{"", "<", "<=", ">", ">=", "==", "!=", "false", "true"}[op]
}

func (op Operator) Mask() uint64 {
	return [...]uint64{0, Less, Less | Equal, Greater, Greater | Equal, Equal, Less | Greater, 0, Less | Equal | Greater}[op]
}

func (op Operator) FloatMask() uint64 {
	return [...]uint64{0, Less, Less | Equal, Greater, Greater | Equal, Equal, Less | Greater | Unordered, 0, Less | Equal | Greater | Unordered}[op]
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
	WidthMask        uint64
	Signed           bool
	Attainable       *Attainable
}

// Attainable bounds the ranks and the set bits of the operand's value.
type Attainable struct {
	Low, High    uint64
	ZeroBits     uint64
	ExactRanks   []uint64
	Conservative bool
}

func (d Domain) Exact() bool {
	return d.Attainable == nil || !d.Attainable.Conservative
}

func (d Domain) widthMask() uint64 {
	if d.WidthMask == 0 {
		return ^uint64(0)
	}
	return d.WidthMask
}

func (d Domain) bounds() (lo, hi, forbidden uint64, ok bool) {
	lo, hi = d.Low, d.High
	if d.Attainable != nil {
		if d.Attainable.Low > lo {
			lo = d.Attainable.Low
		}
		if d.Attainable.High < hi {
			hi = d.Attainable.High
		}
		forbidden = d.Attainable.ZeroBits & d.widthMask()
	}
	return lo, hi, forbidden, lo <= hi
}

func (d Domain) Possible(rank uint64) bool {
	if rank < d.Low || rank > d.High {
		return false
	}
	if d.Attainable == nil {
		return true
	}
	if len(d.Attainable.ExactRanks) > 0 {
		_, ok := slices.BinarySearch(d.Attainable.ExactRanks, rank)
		return ok
	}
	value := rank
	if d.Signed {
		value = uint64(UnrankSigned(rank))
	}
	return d.Attainable.Low <= rank && rank <= d.Attainable.High && (value&d.widthMask())&d.Attainable.ZeroBits == 0
}

func prevMasked(x, forbidden uint64) uint64 {
	conflict := x & forbidden
	if conflict == 0 {
		return x
	}
	k := 63 - bits.LeadingZeros64(conflict)
	bit := uint64(1) << k
	lowMask := bit - 1
	return (x & ^(lowMask | bit)) | (^forbidden & lowMask)
}

func nextMasked(x, forbidden uint64) (uint64, bool) {
	conflict := x & forbidden
	if conflict == 0 {
		return x, true
	}
	k := 63 - bits.LeadingZeros64(conflict)
	candidates := (^forbidden & ^x) & (^uint64(0) << k << 1)
	if candidates == 0 {
		return 0, false
	}
	lowBit := candidates & -candidates
	return (x & ^(lowBit - 1)) | lowBit, true
}

// PrevPossible returns the largest rank <= rank in [d.Low, d.High] satisfying d.Possible.
func (d Domain) PrevPossible(rank uint64) (uint64, bool) {
	if d.Attainable != nil && len(d.Attainable.ExactRanks) > 0 {
		if rank < d.Low {
			return 0, false
		}
		target := min(rank, d.High)
		idx, found := slices.BinarySearch(d.Attainable.ExactRanks, target)
		if found {
			return d.Attainable.ExactRanks[idx], true
		}
		if idx == 0 {
			return 0, false
		}
		v := d.Attainable.ExactRanks[idx-1]
		if v < d.Low {
			return 0, false
		}
		return v, true
	}
	lo, hi, forbidden, ok := d.bounds()
	if !ok || rank < lo {
		return 0, false
	}
	if rank < hi {
		hi = rank
	}
	if forbidden == 0 {
		return hi, true
	}
	if !d.Signed {
		v := prevMasked(hi, forbidden)
		if v < lo {
			return 0, false
		}
		return v, true
	}
	const signBit = uint64(1) << 63
	if hi >= signBit {
		posLo := lo
		if posLo < signBit {
			posLo = signBit
		}
		if v := prevMasked(hi^signBit, forbidden); v >= posLo^signBit {
			return v ^ signBit, true
		}
	}
	if lo < signBit {
		negHi := hi
		if negHi >= signBit {
			negHi = signBit - 1
		}
		if v := prevMasked(negHi^signBit, forbidden); v >= lo^signBit {
			return v ^ signBit, true
		}
	}
	return 0, false
}

// NextPossible returns the smallest rank >= rank in [d.Low, d.High] satisfying d.Possible.
func (d Domain) NextPossible(rank uint64) (uint64, bool) {
	if d.Attainable != nil && len(d.Attainable.ExactRanks) > 0 {
		if rank > d.High {
			return 0, false
		}
		target := max(rank, d.Low)
		idx, _ := slices.BinarySearch(d.Attainable.ExactRanks, target)
		if idx >= len(d.Attainable.ExactRanks) {
			return 0, false
		}
		v := d.Attainable.ExactRanks[idx]
		if v > d.High {
			return 0, false
		}
		return v, true
	}
	lo, hi, forbidden, ok := d.bounds()
	if !ok || rank > hi {
		return 0, false
	}
	if rank > lo {
		lo = rank
	}
	if forbidden == 0 {
		return lo, true
	}
	if !d.Signed {
		v, found := nextMasked(lo, forbidden)
		if !found || v > hi {
			return 0, false
		}
		return v, true
	}
	const signBit = uint64(1) << 63
	if lo < signBit {
		negHi := hi
		if negHi >= signBit {
			negHi = signBit - 1
		}
		if v, found := nextMasked(lo^signBit, forbidden); found && v <= negHi^signBit {
			return v ^ signBit, true
		}
	}
	if hi >= signBit {
		posLo := lo
		if posLo < signBit {
			posLo = signBit
		}
		if v, found := nextMasked(posLo^signBit, forbidden); found && v <= hi^signBit {
			return v ^ signBit, true
		}
	}
	return 0, false
}

type Summary struct {
	Flags, Below, Above, BelowGap, AboveGap uint64
}

func (s *Summary) Observe(lhs, rhs uint64) {
	if lhs < rhs {
		gap := rhs - lhs
		if s.Flags&Less == 0 || lhs > s.Below {
			s.Below = lhs
		}
		if s.Flags&Less == 0 || gap < s.BelowGap {
			s.BelowGap = gap
		}
		s.Flags |= Less
	} else if lhs > rhs {
		gap := lhs - rhs
		if s.Flags&Greater == 0 || lhs < s.Above {
			s.Above = lhs
		}
		if s.Flags&Greater == 0 || gap < s.AboveGap {
			s.AboveGap = gap
		}
		s.Flags |= Greater
	} else {
		s.Flags |= Equal
	}
}

func (s *Summary) Merge(other Summary) {
	if other.Flags&Less != 0 {
		if s.Flags&Less == 0 || other.Below > s.Below {
			s.Below = other.Below
		}
		if other.BelowGap != 0 && (s.Flags&Less == 0 || s.BelowGap == 0 || other.BelowGap < s.BelowGap) {
			s.BelowGap = other.BelowGap
		}
	}
	if other.Flags&Greater != 0 {
		if s.Flags&Greater == 0 || other.Above < s.Above {
			s.Above = other.Above
		}
		if other.AboveGap != 0 && (s.Flags&Greater == 0 || s.AboveGap == 0 || other.AboveGap < s.AboveGap) {
			s.AboveGap = other.AboveGap
		}
	}
	s.Flags |= other.Flags
}

type Candidate struct {
	Kind    string
	Op      Operator
	Pivot   uint64
	Shift   int
	Witness *uint64
	Float   bool
}

func (c Candidate) Infects(original Operator, seen Summary) bool {
	if c.Kind == "margin" {
		if c.Shift < 0 {
			return seen.Flags&Less != 0 && seen.BelowGap == 1
		}
		return seen.Flags&Greater != 0 && seen.AboveGap == 1
	}
	if c.Shift == 0 {
		if c.Float {
			return seen.Flags&(c.Op.FloatMask()^original.FloatMask()) != 0
		}
		return seen.Flags&(c.Op.Mask()^original.Mask()) != 0
	}
	checksNeighbor := original == EQ || original == NE
	if checksNeighbor && seen.Flags&Equal != 0 {
		return true
	}
	if !checksNeighbor && c.Op == original && (original == GE || original == LT) == (c.Shift > 0) {
		return seen.Flags&Equal != 0
	}
	if c.Shift < 0 {
		return seen.Flags&Less != 0 && seen.Below >= c.Pivot
	}
	return seen.Flags&Greater != 0 && seen.Above <= c.Pivot
}

func Plan(op Operator, domain *Domain, seen Summary, targeted bool) []Candidate {
	return PlanSite(op, domain, false, false, seen, targeted)
}

func PlanSite(op Operator, domain *Domain, margin, isFloat bool, seen Summary, targeted bool) []Candidate {
	if seen.Flags == 0 {
		return nil
	}
	return planSite(op, domain, margin, isFloat, seen, targeted)
}

// Preview describes potential mutations before observing any tests.
func Preview(op Operator, domain *Domain, targeted bool) []Candidate {
	return PreviewSite(op, domain, false, false, targeted)
}

// PreviewSite describes potential mutations for a site before observing any tests.
func PreviewSite(op Operator, domain *Domain, margin, isFloat bool, targeted bool) []Candidate {
	return planSite(op, domain, margin, isFloat, Summary{}, targeted)
}

func planSite(op Operator, domain *Domain, margin, isFloat bool, seen Summary, targeted bool) []Candidate {
	if isFloat {
		var candidates []Candidate
		if opposite := op.Boundary(); opposite != 0 {
			candidates = append(candidates, Candidate{Kind: "boundary", Op: opposite, Float: true})
		}
		candidates = append(candidates, Candidate{Kind: "negation", Op: op.Negate(), Float: true})
		return candidates
	}
	var candidates []Candidate
	var pivot uint64
	if domain != nil {
		pivot = domain.Pivot
	}
	if opposite := op.Boundary(); opposite != 0 {
		boundary := Candidate{Kind: "boundary", Op: opposite, Pivot: pivot}
		if domain != nil && domain.Exact() && domain.Possible(pivot) {
			boundary.Witness = &pivot
		}
		if !targeted || domain == nil || domain.Possible(pivot) || boundary.Infects(op, seen) {
			candidates = append(candidates, boundary)
		}
	}
	candidates = append(candidates, Candidate{Kind: "negation", Op: op.Negate(), Pivot: pivot})
	if !targeted || op.Boundary() == 0 {
		return candidates
	}
	if domain == nil {
		if margin {
			if op == GE || op == LT {
				candidate := Candidate{Kind: "margin", Op: op, Shift: -1}
				if !candidate.Infects(op, seen) {
					candidates = append(candidates, candidate)
				}
			} else if op == GT || op == LE {
				candidate := Candidate{Kind: "margin", Op: op, Shift: 1}
				if !candidate.Infects(op, seen) {
					candidates = append(candidates, candidate)
				}
			}
		}
		return candidates
	}
	if !domain.Exact() {
		return candidates
	}

	// The boundary operator already represents the adjacent change on the possible side.
	if op == GE || op == LT {
		if domain.Low < pivot {
			if value, ok := domain.PrevPossible(pivot - 1); ok {
				candidate := Candidate{Kind: "threshold", Op: op, Pivot: value, Shift: -1, Witness: &value}
				if !candidate.Infects(op, seen) {
					candidates = append(candidates, candidate)
				}
			}
		}
		if !domain.Possible(pivot) && pivot < domain.High {
			if up, ok := domain.NextPossible(pivot + 1); ok {
				candidate := Candidate{Kind: "threshold", Op: op.Boundary(), Pivot: up, Shift: 1, Witness: &up}
				if !candidate.Infects(op, seen) {
					candidates = append(candidates, candidate)
				}
			}
		}
	} else if op == GT || op == LE {
		if pivot < domain.High {
			if value, ok := domain.NextPossible(pivot + 1); ok {
				candidate := Candidate{Kind: "threshold", Op: op, Pivot: value, Shift: 1, Witness: &value}
				if !candidate.Infects(op, seen) {
					candidates = append(candidates, candidate)
				}
			}
		}
		if !domain.Possible(pivot) && domain.Low < pivot {
			if lo, ok := domain.PrevPossible(pivot - 1); ok {
				candidate := Candidate{Kind: "threshold", Op: op.Boundary(), Pivot: lo, Shift: -1, Witness: &lo}
				if !candidate.Infects(op, seen) {
					candidates = append(candidates, candidate)
				}
			}
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
