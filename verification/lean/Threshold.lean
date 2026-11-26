import Std.Tactic

/-! The Go planner stores each operand in its integer order as a rank. The finite integer domain
is checked at the call site. The following four lemmas identify the only rank that can
distinguish the original from an adjacent threshold. The final lemmas justify the extrema
used in place of the whole observation set. -/
namespace TargetedMutation

theorem ge_lower (x t : Int) : (t ≤ x ↔ ¬ (t-1 ≤ x)) ↔ x = t-1 := by omega
theorem ge_raise (x t : Int) : (t ≤ x ↔ ¬ (t+1 ≤ x)) ↔ x = t := by omega
theorem gt_lower (x t : Int) : (t < x ↔ ¬ (t-1 < x)) ↔ x = t := by omega
theorem gt_raise (x t : Int) : (t < x ↔ ¬ (t+1 < x)) ↔ x = t+1 := by omega

theorem le_lower (x t : Int) : (x ≤ t ↔ ¬ (x ≤ t-1)) ↔ x = t := by omega
theorem le_raise (x t : Int) : (x ≤ t ↔ ¬ (x ≤ t+1)) ↔ x = t+1 := by omega
theorem lt_lower (x t : Int) : (x < t ↔ ¬ (x < t-1)) ↔ x = t-1 := by omega
theorem lt_raise (x t : Int) : (x < t ↔ ¬ (x < t+1)) ↔ x = t := by omega

theorem eq_lower (x t : Int) : (x = t ↔ ¬ (x = t-1)) ↔ x = t ∨ x = t-1 := by omega
theorem eq_raise (x t : Int) : (x = t ↔ ¬ (x = t+1)) ↔ x = t ∨ x = t+1 := by omega
theorem ne_lower (x t : Int) : (x ≠ t ↔ ¬ (x ≠ t-1)) ↔ x = t ∨ x = t-1 := by omega
theorem ne_raise (x t : Int) : (x ≠ t ↔ ¬ (x ≠ t+1)) ↔ x = t ∨ x = t+1 := by omega

theorem below_extremum (observed : Int → Prop) (boundary nearest : Int)
    (found : observed nearest) (lower : nearest < boundary)
    (maximal : ∀ x, observed x → x < boundary → x ≤ nearest) :
    observed (boundary-1) ↔ nearest = boundary-1 := by
  constructor
  · intro h
    have := maximal _ h (by omega)
    omega
  · intro h
    rw [← h]
    exact found

theorem above_extremum (observed : Int → Prop) (boundary nearest : Int)
    (found : observed nearest) (higher : boundary < nearest)
    (minimal : ∀ x, observed x → boundary < x → nearest ≤ x) :
    observed (boundary+1) ↔ nearest = boundary+1 := by
  constructor
  · intro h
    have := minimal _ h (by omega)
    omega
  · intro h
    rw [← h]
    exact found

theorem shifts_preserve_domain (lo hi t : Int) (bounds : lo ≤ t ∧ t ≤ hi) :
    (lo < t → lo ≤ t-1 ∧ t-1 ≤ hi) ∧ (t < hi → lo ≤ t+1 ∧ t+1 ≤ hi) := by omega

theorem rank_preserves_signed_order (x y : Int) : x < y ↔ x + 2^63 < y + 2^63 := by omega

theorem up_interval (observed : Int → Prop) (original lower upper candidate : Int)
    (hlo : ∀ x, observed x → x < original → x ≤ lower)
    (hhi : ∀ x, observed x → original ≤ x → upper ≤ x)
    (bounds : lower < candidate ∧ candidate ≤ upper) :
    ∀ x, observed x → (original ≤ x ↔ candidate ≤ x) := by
  intro x hx
  by_cases before : x < original
  · have := hlo x hx before
    omega
  · have := hhi x hx (by omega)
    omega

theorem rank_integer_offset (x : Int) (lower : -(2^63) ≤ x) (upper : x < 2^63) :
    0 ≤ x + 2^63 ∧ x + 2^63 < 2^64 := by omega

-- Bits 0, 1, and 2 represent the less, equal, and greater cases of a total order.
theorem comparison_mask_exact (seen original mutant : BitVec 3) :
    (seen &&& (original ^^^ mutant) = 0#3) ↔
      ((seen.getLsbD 0 = true → original.getLsbD 0 = mutant.getLsbD 0) ∧
       (seen.getLsbD 1 = true → original.getLsbD 1 = mutant.getLsbD 1) ∧
       (seen.getLsbD 2 = true → original.getLsbD 2 = mutant.getLsbD 2)) := by
  bv_decide

def swapOperands (mask : BitVec 3) : BitVec 3 :=
  ((mask &&& 1#3) <<< 2) ||| (mask &&& 2#3) ||| ((mask &&& 4#3) >>> 2)

theorem swap_operands_preserves_agreement (seen original mutant : BitVec 3) :
    seen &&& (original ^^^ mutant) = 0#3 ↔
      swapOperands seen &&& (swapOperands original ^^^ swapOperands mutant) = 0#3 := by
  unfold swapOperands
  bv_decide

theorem compares_as_false (seen original : BitVec 3) :
    seen &&& (original ^^^ 0#3) = 0#3 ↔ seen &&& original = 0#3 := by bv_decide

theorem compares_as_true (seen original : BitVec 3) :
    seen &&& (original ^^^ 7#3) = 0#3 ↔ seen &&& original = seen := by bv_decide

theorem masked_value_bits (x mask : BitVec 64) : (x &&& mask) &&& (~~~ mask) = 0#64 := by
  bv_decide

theorem signed_remainder_bounds (x d : Int) (positive : 0 < d) :
    -(d - 1) ≤ x.tmod d ∧ x.tmod d ≤ d - 1 := by
  have lo := Int.lt_tmod_of_pos x positive
  have hi := Int.tmod_lt_of_pos x positive
  omega

theorem negative_divisor_bounds (x d : Int) (positive : 0 < d) :
    -(d - 1) ≤ x.tmod (-d) ∧ x.tmod (-d) ≤ d - 1 := by
  simpa using signed_remainder_bounds x d positive

theorem unsigned_remainder_bounds (x d : Nat) (positive : 0 < d) : x % d ≤ d - 1 := by
  have bound := Nat.mod_lt x positive
  omega

theorem rank_difference_exact (x y : BitVec 64) :
    (x ^^^ (1#64 <<< 63)) - (y ^^^ (1#64 <<< 63)) = x - y := by
  bv_decide

def prevMaskedStep (x forbidden bit : BitVec 64) : BitVec 64 :=
  let lowMask := bit - 1#64
  (x &&& (~~~(lowMask ||| bit))) ||| ((~~~forbidden) &&& lowMask)

theorem prev_masked_step_exact (x forbidden bit w : BitVec 64)
    (hpow : bit ≠ 0#64 ∧ bit &&& (bit - 1#64) = 0#64)
    (hbit : (x &&& forbidden) &&& bit ≠ 0#64)
    (hmsb : (x &&& forbidden) &&& (~~~((bit - 1#64) ||| bit)) = 0#64) :
    let v := prevMaskedStep x forbidden bit
    v &&& forbidden = 0#64 ∧ BitVec.ult v x ∧
      (w &&& forbidden = 0#64 → BitVec.ule w x → BitVec.ule w v) := by
  unfold prevMaskedStep
  bv_decide

def nextMaskedStep (x lowBit : BitVec 64) : BitVec 64 :=
  (x &&& (~~~(lowBit - 1#64))) ||| lowBit

theorem next_masked_step_exact (x forbidden bit lowBit w : BitVec 64)
    (hpowBit : bit ≠ 0#64 ∧ bit &&& (bit - 1#64) = 0#64)
    (hbit : (x &&& forbidden) &&& bit ≠ 0#64)
    (hmsb : (x &&& forbidden) &&& (~~~((bit - 1#64) ||| bit)) = 0#64)
    (hlow : lowBit ≠ 0#64 ∧ lowBit &&& (lowBit - 1#64) = 0#64)
    (hallowed : ((~~~forbidden &&& ~~~x) &&& (~~~((bit - 1#64) ||| bit))) &&& lowBit ≠ 0#64)
    (hle : ((~~~forbidden &&& ~~~x) &&& (~~~((bit - 1#64) ||| bit))) &&& (lowBit - 1#64) = 0#64) :
    let v := nextMaskedStep x lowBit
    v &&& forbidden = 0#64 ∧ BitVec.ult x v ∧
      (w &&& forbidden = 0#64 → BitVec.ule x w → BitVec.ule v w) := by
  unfold nextMaskedStep
  bv_decide

theorem next_masked_none (x forbidden bit w : BitVec 64)
    (hpowBit : bit ≠ 0#64 ∧ bit &&& (bit - 1#64) = 0#64)
    (hbit : (x &&& forbidden) &&& bit ≠ 0#64)
    (hmsb : (x &&& forbidden) &&& (~~~((bit - 1#64) ||| bit)) = 0#64)
    (hnone : (~~~forbidden &&& ~~~x) &&& (~~~((bit - 1#64) ||| bit)) = 0#64) :
    w &&& forbidden = 0#64 → ¬ BitVec.ule x w := by
  bv_decide

theorem min_bound_eq_le (x lo : Int) (h : lo ≤ x) : (x = lo ↔ x ≤ lo) := by omega
theorem min_bound_ne_gt (x lo : Int) (h : lo ≤ x) : (x ≠ lo ↔ lo < x) := by omega
theorem max_bound_eq_ge (x hi : Int) (h : x ≤ hi) : (x = hi ↔ hi ≤ x) := by omega
theorem max_bound_ne_lt (x hi : Int) (h : x ≤ hi) : (x ≠ hi ↔ x < hi) := by omega

end TargetedMutation
