------------------------------ MODULE Campaign ------------------------------
EXTENDS FiniteSets, Naturals

CONSTANTS Binaries, Jobs, Workers, Consumer
ASSUME /\ IsFiniteSet(Binaries) /\ IsFiniteSet(Jobs)
       /\ Binaries # {} /\ Workers \in Nat \ {0}
       /\ Consumer \in [Jobs -> Binaries]

VARIABLES baseline, state, active, outcome, cancelled, closed

vars == <<baseline, state, active, outcome, cancelled, closed>>

Init ==
    /\ baseline = [b \in Binaries |-> "pending"]
    /\ state = [j \in Jobs |-> "pending"]
    /\ active = {}
    /\ outcome = [j \in Jobs |-> "unset"]
    /\ cancelled = FALSE
    /\ closed = FALSE

Baseline(b, result) ==
    /\ ~cancelled /\ ~closed /\ baseline[b] = "pending"
    /\ baseline' = [baseline EXCEPT ![b] = result]
    /\ UNCHANGED <<state, active, outcome, cancelled, closed>>

Ready == \A b \in Binaries : baseline[b] = "passed"

Start(j) ==
    /\ ~cancelled /\ ~closed /\ Ready
    /\ state[j] = "pending" /\ Cardinality(active) < Workers
    /\ state' = [state EXCEPT ![j] = "running"]
    /\ active' = active \cup {j}
    /\ UNCHANGED <<baseline, outcome, cancelled, closed>>

Finish(j, evidence, result) ==
    /\ j \in active
    /\ result \in {"error", "timeout"} \/ (evidence /\ ~cancelled)
    /\ state' = [state EXCEPT ![j] = IF evidence THEN "applied" ELSE "unconfirmed"]
    /\ outcome' = [outcome EXCEPT ![j] = result]
    /\ active' = active \ {j}
    /\ UNCHANGED <<baseline, cancelled, closed>>

Cancel ==
    /\ ~cancelled /\ ~closed
    /\ cancelled' = TRUE
    /\ UNCHANGED <<baseline, state, active, outcome, closed>>

Close ==
    /\ ~closed /\ active = {}
    /\ cancelled \/ (\E b \in Binaries : baseline[b] = "failed")
       \/ (Ready /\ \A j \in Jobs : state[j] # "pending")
    /\ closed' = TRUE
    /\ UNCHANGED <<baseline, state, active, outcome, cancelled>>

Next ==
    \/ \E b \in Binaries, r \in {"passed", "failed"} : Baseline(b, r)
    \/ \E j \in Jobs : Start(j)
    \/ \E j \in Jobs, e \in BOOLEAN, r \in {"killed", "survived", "error", "timeout"} : Finish(j, e, r)
    \/ Cancel
    \/ Close
    \/ closed /\ UNCHANGED vars

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ baseline \in [Binaries -> {"pending", "passed", "failed"}]
    /\ state \in [Jobs -> {"pending", "running", "applied", "unconfirmed"}]
    /\ active \subseteq Jobs
    /\ outcome \in [Jobs -> {"unset", "killed", "survived", "error", "timeout"}]
    /\ cancelled \in BOOLEAN /\ closed \in BOOLEAN

BoundedWorkers ==
    /\ Cardinality(active) <= Workers
    /\ active = {j \in Jobs : state[j] = "running"}

SoundVerdict ==
    \A j \in Jobs : outcome[j] \in {"killed", "survived"} =>
        /\ state[j] = "applied"
        /\ baseline[Consumer[j]] = "passed"
        /\ Ready

CleanClose == closed => active = {}
ModelBinaries == 1..2
ModelJobs == 1..3
ModelConsumer == [j \in Jobs |-> IF j = 2 THEN 2 ELSE 1]
=============================================================================
