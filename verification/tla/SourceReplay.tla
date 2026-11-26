---------------------------- MODULE SourceReplay ----------------------------
EXTENDS Naturals

VARIABLES phase, builds, cleanBaseline, compiled, outcome
vars == <<phase, builds, cleanBaseline, compiled, outcome>>

Init ==
    /\ phase = "baseline"
    /\ builds = 0
    /\ cleanBaseline = FALSE
    /\ compiled = FALSE
    /\ outcome = "unset"

Baseline(passed) ==
    /\ phase = "baseline"
    /\ cleanBaseline' = passed
    /\ phase' = IF passed THEN "building" ELSE "done"
    /\ builds' = IF passed THEN 1 ELSE 0
    /\ outcome' = IF passed THEN "unset" ELSE "error"
    /\ UNCHANGED compiled

Build(passed, repairable) ==
    /\ phase = "building"
    /\ compiled' = passed
    /\ phase' = IF passed THEN "testing" ELSE
                    IF repairable /\ builds = 1 THEN "repair" ELSE "done"
    /\ outcome' = IF phase' = "done" THEN "non-viable" ELSE "unset"
    /\ UNCHANGED <<builds, cleanBaseline>>

Repair ==
    /\ phase = "repair"
    /\ builds' = builds + 1
    /\ phase' = "building"
    /\ UNCHANGED <<cleanBaseline, compiled, outcome>>

TestFinished(passed, evidence) ==
    /\ phase = "testing"
    /\ phase' = "done"
    /\ outcome' = IF ~evidence THEN "error" ELSE IF passed THEN "survived" ELSE "killed"
    /\ UNCHANGED <<builds, cleanBaseline, compiled>>

Interrupt ==
    /\ phase # "done"
    /\ phase' = "done"
    /\ outcome' = "error"
    /\ UNCHANGED <<builds, cleanBaseline, compiled>>

Next ==
    \/ \E p \in BOOLEAN : Baseline(p)
    \/ \E p, r \in BOOLEAN : Build(p, r)
    \/ Repair
    \/ \E p, e \in BOOLEAN : TestFinished(p, e)
    \/ Interrupt
    \/ phase = "done" /\ UNCHANGED vars

Spec == Init /\ [][Next]_vars

BoundedAttempts == builds \in 0..2
NoFalseVerdict == outcome \in {"survived", "killed"} => cleanBaseline /\ compiled /\ phase = "done"
RejectedDoesNotRun == outcome = "non-viable" => ~compiled /\ phase = "done"
=============================================================================
