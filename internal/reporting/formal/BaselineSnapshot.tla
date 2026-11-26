--------------------------- MODULE BaselineSnapshot ---------------------------
EXTENDS FiniteSets

Writers == {"first", "second"}
VARIABLES phase, synced, published, visible, reads
vars == <<phase, synced, published, visible, reads>>

Init ==
    /\ phase = [w \in Writers |-> "idle"]
    /\ synced = {}
    /\ published = {}
    /\ visible = "old"
    /\ reads = {}

Prepare(w) ==
    /\ phase[w] = "idle"
    /\ phase' = [phase EXCEPT ![w] = "preparing"]
    /\ UNCHANGED <<synced, published, visible, reads>>

Sync(w) ==
    /\ phase[w] = "preparing"
    /\ phase' = [phase EXCEPT ![w] = "ready"]
    /\ synced' = synced \cup {w}
    /\ UNCHANGED <<published, visible, reads>>

FailBeforePublication(w) ==
    /\ phase[w] \in {"preparing", "ready"}
    /\ phase' = [phase EXCEPT ![w] = "failed"]
    /\ UNCHANGED <<synced, published, visible, reads>>

\* Each writer owns its temporary file. Rename replaces the path in one step.
Rename(w) ==
    /\ phase[w] = "ready"
    /\ phase' = [phase EXCEPT ![w] = "done"]
    /\ published' = published \cup {w}
    /\ visible' = w
    /\ UNCHANGED <<synced, reads>>

Read ==
    /\ reads' = reads \cup {visible}
    /\ UNCHANGED <<phase, synced, published, visible>>

Next ==
    \/ \E w \in Writers : Prepare(w) \/ Sync(w) \/ FailBeforePublication(w) \/ Rename(w)
    \/ Read

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ phase \in [Writers -> {"idle", "preparing", "ready", "failed", "done"}]
    /\ synced \subseteq Writers
    /\ published \subseteq Writers
    /\ visible \in Writers \cup {"old"}
    /\ reads \subseteq Writers \cup {"old"}

CompletePublication == published \subseteq synced
CompleteVisibility == {visible} \cup reads \subseteq published \cup {"old"}
FailedUnpublished == \A w \in Writers : phase[w] = "failed" => w \notin published
=============================================================================
