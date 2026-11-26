---------------------------- MODULE Checkpoint ----------------------------
EXTENDS FiniteSets, Naturals

Clients == {"first", "second", "foreign"}
Matching == {"first", "second"}
Keys == 1..3
NoKey == 0

VARIABLES holders, tail, pending, durable, acknowledged, visible
vars == <<holders, tail, pending, durable, acknowledged, visible>>

Init ==
    /\ holders = {}
    /\ tail = "none"
    /\ pending = NoKey
    /\ durable = {}
    /\ acknowledged = {}
    /\ visible = {}

Open(c) ==
    /\ holders = {} /\ c \in Matching
    /\ holders' = {c}
    /\ durable' = IF tail = "complete" THEN durable \cup {pending} ELSE durable
    /\ visible' = durable'
    /\ tail' = "none"
    /\ pending' = NoKey
    /\ UNCHANGED acknowledged

Reject(c) ==
    /\ holders # {} \/ c \notin Matching
    /\ UNCHANGED vars

StartWrite(c, key) ==
    /\ c \in holders /\ tail = "none" /\ key \notin visible
    /\ tail' = "partial"
    /\ pending' = key
    /\ UNCHANGED <<holders, durable, acknowledged, visible>>

FinishWrite(c) ==
    /\ c \in holders /\ tail = "partial"
    /\ tail' = "complete"
    /\ UNCHANGED <<holders, pending, durable, acknowledged, visible>>

Sync(c) ==
    /\ c \in holders /\ tail = "complete"
    /\ durable' = durable \cup {pending}
    /\ tail' = "synced"
    /\ UNCHANGED <<holders, pending, acknowledged, visible>>

Acknowledge(c) ==
    /\ c \in holders /\ tail = "synced"
    /\ acknowledged' = acknowledged \cup {pending}
    /\ visible' = visible \cup {pending}
    /\ tail' = "none"
    /\ pending' = NoKey
    /\ UNCHANGED <<holders, durable>>

Crash(c, retained) ==
    /\ c \in holders
    /\ retained \in CASE tail = "partial" -> {"none", "torn"}
                      [] tail = "complete" -> {"none", "torn", "complete"}
                      [] OTHER -> {"none"}
    /\ holders' = {}
    /\ visible' = {}
    /\ pending' = IF retained = "none" THEN NoKey ELSE pending
    /\ tail' = retained
    /\ UNCHANGED <<durable, acknowledged>>

Close(c) ==
    /\ c \in holders /\ tail = "none"
    /\ holders' = {}
    /\ visible' = {}
    /\ UNCHANGED <<tail, pending, durable, acknowledged>>

Next ==
    \E c \in Clients:
        \/ Open(c)
        \/ Reject(c)
        \/ \E key \in Keys: StartWrite(c, key)
        \/ FinishWrite(c)
        \/ Sync(c)
        \/ Acknowledge(c)
        \/ \E retained \in {"none", "torn", "complete"}: Crash(c, retained)
        \/ Close(c)

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ holders \subseteq Clients
    /\ tail \in {"none", "partial", "torn", "complete", "synced"}
    /\ pending \in Keys \cup {NoKey}
    /\ (tail = "none") = (pending = NoKey)
    /\ durable \subseteq Keys /\ acknowledged \subseteq Keys /\ visible \subseteq Keys

ExclusiveLock == Cardinality(holders) <= 1
FingerprintIsolation == holders \subseteq Matching
NoIncompleteResult == visible \subseteq durable /\ (tail \in {"torn", "partial"} => pending \notin visible)
AcknowledgedResultsSurvive == acknowledged \subseteq durable /\ (holders # {} => acknowledged \subseteq visible)
=============================================================================
