---------------------------- MODULE SourceReplay ----------------------------
EXTENDS Naturals

VARIABLES phase, builds, probes, cleanBaseline, compiled, evidence, outcome
vars == <<phase, builds, probes, cleanBaseline, compiled, evidence, outcome>>

Init ==
    /\ phase = "baseline"
    /\ builds = 0
    /\ probes = 0
    /\ cleanBaseline = FALSE
    /\ compiled = FALSE
    /\ evidence = "none"
    /\ outcome = "unset"

Baseline(passed) ==
    /\ phase = "baseline"
    /\ cleanBaseline' = passed
    /\ phase' = IF passed THEN "plan" ELSE "done"
    /\ outcome' = IF passed THEN "unset" ELSE "error"
    /\ UNCHANGED <<builds, probes, compiled, evidence>>

PlanSource ==
    /\ phase = "plan"
    /\ phase' = "building"
    /\ builds' = 1
    /\ UNCHANGED <<cleanBaseline, compiled, probes, evidence, outcome>>

Reject ==
    /\ phase = "plan"
    /\ phase' = "done"
    /\ outcome' = "non-viable"
    /\ UNCHANGED <<builds, probes, cleanBaseline, compiled, evidence>>

Observe(agrees, verify) ==
    /\ phase = "plan"
    /\ agrees /\ ~verify
    /\ phase' = "done"
    /\ evidence' = "observed-equivalent"
    /\ outcome' = "survived"
    /\ UNCHANGED <<builds, probes, cleanBaseline, compiled>>

PlanProbe(agrees, verify, reached) ==
    /\ phase = "plan"
    /\ reached /\ (~agrees \/ verify)
    /\ phase' = "probe"
    /\ probes' = 1
    /\ UNCHANGED <<builds, cleanBaseline, compiled, evidence, outcome>>

ProbeFinished(confirmed, passed) ==
    /\ phase = "probe"
    /\ phase' = IF confirmed THEN "done" ELSE "building"
    /\ builds' = IF confirmed THEN 0 ELSE 1
    /\ evidence' = IF confirmed THEN "probe-applied" ELSE "none"
    /\ outcome' = IF ~confirmed THEN "unset" ELSE IF passed THEN "survived" ELSE "killed"
    /\ UNCHANGED <<probes, cleanBaseline, compiled>>

Build(passed, repairable) ==
    /\ phase = "building"
    /\ compiled' = passed
    /\ phase' = IF passed THEN "testing" ELSE
                    IF repairable /\ builds = 1 THEN "repair" ELSE "done"
    /\ outcome' = IF phase' = "done" THEN "non-viable" ELSE "unset"
    /\ UNCHANGED <<builds, probes, cleanBaseline, evidence>>

Repair ==
    /\ phase = "repair"
    /\ builds' = builds + 1
    /\ phase' = "building"
    /\ UNCHANGED <<cleanBaseline, compiled, probes, evidence, outcome>>

TestFinished(passed, confirmed) ==
    /\ phase = "testing"
    /\ phase' = "done"
    /\ outcome' = IF ~confirmed THEN "error" ELSE IF passed THEN "survived" ELSE "killed"
    /\ UNCHANGED <<builds, probes, cleanBaseline, compiled, evidence>>

Interrupt ==
    /\ phase # "done"
    /\ phase' = "done"
    /\ outcome' = "error"
    /\ UNCHANGED <<builds, probes, cleanBaseline, compiled, evidence>>

Next ==
    \/ \E p \in BOOLEAN : Baseline(p)
    \/ PlanSource
    \/ Reject
    \/ \E a, v \in BOOLEAN : Observe(a, v)
    \/ \E a, v, r \in BOOLEAN : PlanProbe(a, v, r)
    \/ \E c, p \in BOOLEAN : ProbeFinished(c, p)
    \/ \E p, r \in BOOLEAN : Build(p, r)
    \/ Repair
    \/ \E p, e \in BOOLEAN : TestFinished(p, e)
    \/ Interrupt
    \/ phase = "done" /\ UNCHANGED vars

Spec == Init /\ [][Next]_vars

BoundedAttempts == builds \in 0..2 /\ probes \in 0..1
NoFalseVerdict == outcome \in {"survived", "killed"} =>
    /\ cleanBaseline /\ phase = "done"
    /\ compiled \/ evidence = "probe-applied" \/ (outcome = "survived" /\ evidence = "observed-equivalent")
RejectedDoesNotRun == outcome = "non-viable" => ~compiled /\ phase = "done"
=============================================================================
