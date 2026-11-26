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

end TargetedMutation
