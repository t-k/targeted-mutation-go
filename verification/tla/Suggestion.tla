---------------------------- MODULE Suggestion -----------------------------
EXTENDS Naturals

VARIABLES phase, ambient, sameTest, changed, original, mutant
vars == <<phase, ambient, sameTest, changed, original, mutant>>

Init ==
    /\ phase = "outside"
    /\ ambient = FALSE
    /\ sameTest = FALSE
    /\ changed = FALSE
    /\ original = FALSE
    /\ mutant = FALSE

ObserveOutside(reached, passed) ==
    /\ phase = "outside"
    /\ ambient' = reached
    /\ phase' = IF passed /\ ~reached THEN "test" ELSE "declined"
    /\ UNCHANGED <<sameTest, changed, original, mutant>>

ConfirmTest(inventoryMatches, infected) ==
    /\ phase = "test"
    /\ sameTest' = inventoryMatches
    /\ changed' = infected
    /\ phase' = IF inventoryMatches /\ infected THEN "original" ELSE "declined"
    /\ UNCHANGED <<ambient, original, mutant>>

VerifyOriginal(passed) ==
    /\ phase = "original"
    /\ original' = passed
    /\ phase' = IF passed THEN "mutant" ELSE "declined"
    /\ UNCHANGED <<ambient, sameTest, changed, mutant>>

VerifyMutant(failedOnEdit) ==
    /\ phase = "mutant"
    /\ mutant' = failedOnEdit
    /\ phase' = IF failedOnEdit THEN "verified" ELSE "declined"
    /\ UNCHANGED <<ambient, sameTest, changed, original>>

Next ==
    \/ \E a, p \in BOOLEAN : ObserveOutside(a, p)
    \/ \E i, c \in BOOLEAN : ConfirmTest(i, c)
    \/ \E p \in BOOLEAN : VerifyOriginal(p)
    \/ \E f \in BOOLEAN : VerifyMutant(f)
    \/ phase \in {"declined", "verified"} /\ UNCHANGED vars

Spec == Init /\ [][Next]_vars
TestAttributed == sameTest /\ changed => ~ambient
Verified == phase = "verified" => ~ambient /\ sameTest /\ changed /\ original /\ mutant
=============================================================================
