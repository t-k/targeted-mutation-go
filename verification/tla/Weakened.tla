---------------------------- MODULE Weakened -------------------------------
VARIABLES phase, currentSurvives, previousPasses, sameEdit, previousKills
vars == <<phase, currentSurvives, previousPasses, sameEdit, previousKills>>

Init ==
    /\ phase = "current"
    /\ currentSurvives = FALSE
    /\ previousPasses = FALSE
    /\ sameEdit = FALSE
    /\ previousKills = FALSE

Current(survives) ==
    /\ phase = "current"
    /\ currentSurvives' = survives
    /\ phase' = IF survives THEN "previous-baseline" ELSE "done"
    /\ UNCHANGED <<previousPasses, sameEdit, previousKills>>

PreviousBaseline(passes) ==
    /\ phase = "previous-baseline"
    /\ previousPasses' = passes
    /\ phase' = IF passes THEN "previous-mutant" ELSE "unconfirmed"
    /\ UNCHANGED <<currentSurvives, sameEdit, previousKills>>

PreviousMutant(same, killed) ==
    /\ phase = "previous-mutant"
    /\ sameEdit' = same
    /\ previousKills' = killed
    /\ phase' = IF ~same THEN "unconfirmed" ELSE IF killed THEN "weakened" ELSE "done"
    /\ UNCHANGED <<currentSurvives, previousPasses>>

Interrupt ==
    /\ phase \in {"current", "previous-baseline", "previous-mutant"}
    /\ phase' = "unconfirmed"
    /\ UNCHANGED <<currentSurvives, previousPasses, sameEdit, previousKills>>

Next ==
    \/ \E s \in BOOLEAN : Current(s)
    \/ \E p \in BOOLEAN : PreviousBaseline(p)
    \/ \E s, k \in BOOLEAN : PreviousMutant(s, k)
    \/ Interrupt
    \/ phase \in {"done", "unconfirmed", "weakened"} /\ UNCHANGED vars

Spec == Init /\ [][Next]_vars
Evidence == phase = "weakened" => currentSurvives /\ previousPasses /\ sameEdit /\ previousKills
=============================================================================
