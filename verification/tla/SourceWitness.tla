--------------------------- MODULE SourceWitness ----------------------------
EXTENDS Naturals

VARIABLES phase, runs, originalObserved, mutantObserved, different,
          originalPassed, mutantFailed, assertionMarked, verified
vars == <<phase, runs, originalObserved, mutantObserved, different,
          originalPassed, mutantFailed, assertionMarked, verified>>

Init ==
    /\ phase = "observe-original"
    /\ runs = 0
    /\ originalObserved = FALSE /\ mutantObserved = FALSE /\ different = FALSE
    /\ originalPassed = FALSE /\ mutantFailed = FALSE /\ assertionMarked = FALSE
    /\ verified = FALSE

ObserveOriginal(valid) ==
    /\ phase = "observe-original"
    /\ runs' = runs + 1
    /\ originalObserved' = valid
    /\ phase' = IF valid THEN "observe-mutant" ELSE "done"
    /\ UNCHANGED <<mutantObserved, different, originalPassed, mutantFailed, assertionMarked, verified>>

ObserveMutant(valid, separates) ==
    /\ phase = "observe-mutant"
    /\ runs' = runs + 1
    /\ mutantObserved' = valid
    /\ different' = valid /\ separates
    /\ phase' = IF different' THEN "assert-original" ELSE "done"
    /\ UNCHANGED <<originalObserved, originalPassed, mutantFailed, assertionMarked, verified>>

AssertOriginal(passed, noMarker) ==
    /\ phase = "assert-original"
    /\ runs' = runs + 1
    /\ originalPassed' = passed /\ noMarker
    /\ phase' = IF originalPassed' THEN "assert-mutant" ELSE "done"
    /\ UNCHANGED <<originalObserved, mutantObserved, different, mutantFailed, assertionMarked, verified>>

AssertMutant(failed, correctMarker) ==
    /\ phase = "assert-mutant"
    /\ runs' = runs + 1
    /\ mutantFailed' = failed
    /\ assertionMarked' = correctMarker
    /\ verified' = failed /\ correctMarker
    /\ phase' = "done"
    /\ UNCHANGED <<originalObserved, mutantObserved, different, originalPassed>>

Interrupt ==
    /\ phase # "done"
    /\ phase' = "done"
    /\ UNCHANGED <<runs, originalObserved, mutantObserved, different,
                   originalPassed, mutantFailed, assertionMarked, verified>>

Next ==
    \/ \E valid \in BOOLEAN : ObserveOriginal(valid)
    \/ \E valid, separates \in BOOLEAN : ObserveMutant(valid, separates)
    \/ \E passed, noMarker \in BOOLEAN : AssertOriginal(passed, noMarker)
    \/ \E failed, correctMarker \in BOOLEAN : AssertMutant(failed, correctMarker)
    \/ Interrupt
    \/ phase = "done" /\ UNCHANGED vars

Spec == Init /\ [][Next]_vars /\ WF_vars(Next)

Bounded == runs \in 0..4
Sound == verified =>
    /\ phase = "done" /\ runs = 4
    /\ originalObserved /\ mutantObserved /\ different
    /\ originalPassed /\ mutantFailed /\ assertionMarked
Terminates == <>(phase = "done")
=============================================================================
