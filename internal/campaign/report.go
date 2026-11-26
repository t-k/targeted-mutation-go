package campaign

type Report struct {
	Schema     int       `json:"schema"`
	Sites      int       `json:"sites"`
	Builds     int       `json:"instrumented_builds"`
	Replays    int       `json:"mutation_replays"`
	SourceRuns int       `json:"source_runs"`
	Skipped    int       `json:"replays_avoided"`
	Killed     int       `json:"killed"`
	Unreached  int       `json:"unreached"`
	Rejected   int       `json:"non_viable,omitempty"`
	Incomplete int       `json:"incomplete"`
	Findings   []Finding `json:"findings"`
}

type Finding struct {
	Status     string   `json:"status"`
	Evidence   string   `json:"evidence,omitempty"`
	File       string   `json:"file"`
	Line       int      `json:"line"`
	Column     int      `json:"column"`
	Expression string   `json:"expression,omitempty"`
	Mutation   string   `json:"mutation,omitempty"`
	Rule       string   `json:"rule,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	Packages   []string `json:"packages,omitempty"`
	Tests      []string `json:"tests,omitempty"`
	Interval   []string `json:"threshold_interval,omitempty"`
	Witness    *Witness `json:"witness,omitempty"`
}

type Witness struct {
	Input    string `json:"input"`
	Operand  string `json:"operand"`
	Original bool   `json:"original"`
	Mutant   bool   `json:"mutant"`
	Call     string `json:"call,omitempty"`
	Verified bool   `json:"verified"`
}
