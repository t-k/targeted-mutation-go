package campaign

type SurvivorBreakdown struct {
	Reported   int `json:"reported"`
	Suppressed int `json:"suppressed"`
}

type Report struct {
	Schema            int                `json:"schema"`
	Evaluation        string             `json:"evaluation,omitempty"`
	MutantsRun        int                `json:"mutants_run"`
	Survived          int                `json:"survived"`
	TimedOut          int                `json:"timed_out,omitempty"`
	SurvivorBreakdown *SurvivorBreakdown `json:"survivor_breakdown,omitempty"`
	Suppressed        int                `json:"suppressed,omitempty"`
	Resumed           int                `json:"resumed,omitempty"`
	Audited           int                `json:"audited,omitempty"`
	PreviousRuns      int                `json:"previous_test_runs,omitempty"`
	Sites             int                `json:"sites"`
	Builds            int                `json:"instrumented_builds"`
	Replays           int                `json:"mutation_replays"`
	SourceRuns        int                `json:"source_runs"`
	Skipped           int                `json:"replays_avoided"`
	Killed            int                `json:"killed"`
	Unreached         int                `json:"unreached"`
	Rejected          int                `json:"non_viable,omitempty"`
	Incomplete        int                `json:"incomplete"`
	Findings          []Finding          `json:"findings"`
	WorkspaceDirs     []string           `json:"-"`
}

func (r *Report) Reconcile() {
	reported := 0
	for _, f := range r.Findings {
		if isGapStatus(f.Status) {
			reported++
		}
	}
	r.Survived = reported + r.Suppressed
	r.MutantsRun = r.Killed + r.Survived
	r.SurvivorBreakdown = &SurvivorBreakdown{
		Reported:   reported,
		Suppressed: r.Suppressed,
	}
}

func isGapStatus(status string) bool {
	switch status {
	case "input-gap", "assertion-gap", "source-survivor", "untested-change", "weakened-test":
		return true
	}
	return false
}

type Finding struct {
	ID            string         `json:"id,omitempty"`
	Status        string         `json:"status"`
	Evidence      string         `json:"evidence,omitempty"`
	File          string         `json:"file"`
	Line          int            `json:"line"`
	Column        int            `json:"column"`
	StartLine     int            `json:"-"`
	EndLine       int            `json:"-"`
	Expression    string         `json:"expression,omitempty"`
	Mutation      string         `json:"mutation,omitempty"`
	Rule          string         `json:"rule,omitempty"`
	Detail        string         `json:"detail,omitempty"`
	Packages      []string       `json:"packages,omitempty"`
	Tests         []string       `json:"tests,omitempty"`
	Interval      []string       `json:"threshold_interval,omitempty"`
	Witness       *Witness       `json:"witness,omitempty"`
	SourceWitness *SourceWitness `json:"source_witness,omitempty"`
}

func (f Finding) spanLines() (int, int) {
	start, end := f.StartLine, f.EndLine
	if start <= 0 {
		start = f.Line
	}
	if end < start {
		end = max(start, f.Line)
	}
	return start, end
}

type Witness struct {
	Input    string `json:"input"`
	Operand  string `json:"operand"`
	Original bool   `json:"original"`
	Mutant   bool   `json:"mutant"`
	Call     string `json:"call,omitempty"`
	Code     string `json:"code,omitempty"`
	Verified bool   `json:"verified"`
}
