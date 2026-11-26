package campaign

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

type witnessJob struct {
	index int
	test  string
	probe probeJob
}

func (s *session) verifySourceWitnesses(ctx context.Context, report *Report) {
	if len(s.sources) == 0 {
		return
	}
	jobs := make(map[string]sourceJob, len(s.sources))
	for _, job := range s.sources {
		if job.edit != nil {
			jobs[job.finding.ID] = job
		}
	}
	var candidates []int
	for i, f := range report.Findings {
		if _, present := jobs[f.ID]; present && f.Status == "source-survivor" {
			candidates = append(candidates, i)
		}
	}
	slices.SortStableFunc(candidates, func(a, b int) int {
		aRule, bRule := report.Findings[a].Rule, report.Findings[b].Rule
		if order := sourceWitnessPriority(aRule) - sourceWitnessPriority(bRule); order != 0 {
			return order
		}
		return compareFindings(report.Findings[a], report.Findings[b])
	})
	candidates = candidates[:min(8, len(candidates))]
	type result struct {
		witness *SourceWitness
		err     error
	}
	results := parallel(ctx, min(2, s.cfg.Jobs), len(candidates), func(i int) result {
		witness, err := s.sourceWitness(ctx, jobs[report.Findings[candidates[i]].ID])
		return result{witness, err}
	})
	for i, tested := range results {
		f := &report.Findings[candidates[i]]
		if tested.err != nil {
			f.Detail = "source witness verification: " + tested.err.Error()
			report.Incomplete++
		} else {
			f.SourceWitness = tested.witness
		}
	}
}

func sourceWitnessPriority(rule string) int {
	switch rule {
	case "min-max", "rounding-mode", "affix-contains", "affix-end", "replacement-count", "search-direction", "slice-element-remove", "integer-rounding-order":
		return 0
	}
	return 1
}

func (s *session) verifyWitnesses(ctx context.Context, report *Report, probes []probeJob) {
	byLocation := make(map[string]probeJob)
	for _, probe := range probes {
		f := probe.finding
		byLocation[f.ID] = probe
	}
	groups := make(map[string][]witnessJob)
	for i, finding := range report.Findings {
		want := finding.Status == "input-gap" && s.cfg.VerifyWitnesses || finding.Status == "assertion-gap" && s.cfg.SuggestAssertions
		if !want || finding.Witness == nil || finding.Witness.Call == "" {
			continue
		}
		probe, ok := byLocation[finding.ID]
		if !ok {
			continue
		}
		dir := filepath.Dir(probe.site.File)
		groups[dir] = append(groups[dir], witnessJob{index: i, probe: probe})
	}
	for _, pkg := range s.packages {
		jobs := groups[pkg.Dir]
		if len(jobs) == 0 || ctx.Err() != nil {
			continue
		}
		if err := s.verifyWitnessGroup(ctx, report, pkg, jobs); err != nil {
			for _, job := range jobs {
				report.Findings[job.index].Detail = "witness verification: " + err.Error()
			}
			report.Incomplete++
		}
	}
}

func witnessCode(w *Witness) string {
	return fmt.Sprintf("if got := %s; got != %t {\n\tt.Fatalf(\"got %%v, want %t\", got)\n}", w.Call, w.Original, w.Original)
}

func (s *session) verifyWitnessGroup(ctx context.Context, report *Report, pkg target.Package, jobs []witnessJob) error {
	dir, err := os.MkdirTemp(s.scratch, "witness-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	var source strings.Builder
	fmt.Fprintf(&source, "package %s\nimport \"testing\"\n", pkg.Name)
	for i := range jobs {
		job := &jobs[i]
		job.test = fmt.Sprintf("TestTmt%sWitness%d", pkg.Prefix, i)
		w := report.Findings[job.index].Witness
		fmt.Fprintf(&source, "func %s(t *testing.T) {\n%s\n}\n", job.test, witnessCode(w))
	}
	path := filepath.Join(pkg.Dir, strings.ToLower(pkg.Prefix)+"_witness_test.go")
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return fmt.Errorf("synthetic witness path is occupied: %s", path)
	}
	files := append(slices.Clone(s.replacements), target.Replacement{Path: path, Data: []byte(source.String())})
	overlay, err := makeOverlay(dir, files)
	if err != nil {
		return err
	}
	bin := consumer{pkg: pkg.Path, dir: pkg.Dir, binary: filepath.Join(dir, "witness.test")}
	if err := s.build(ctx, overlay, &bin, s.cfg.Jobs); err != nil {
		return err
	}
	for _, job := range jobs {
		// An original failure or absent witness stays explicitly unverified.
		base := s.runBinary(ctx, &bin, "", "", job.test)
		if base.kind != "passed" || !slices.Contains(base.tests, job.test) {
			report.Findings[job.index].Detail = "generated witness did not pass on the original"
			report.Incomplete++
			continue
		}
		trace, err := newTrace(dir, len(s.sites))
		if err != nil {
			return err
		}
		mutated := s.runBinary(ctx, &bin, trace, selector(job.probe.site, job.probe.candidate), job.test)
		flags, err := readFlags(trace, job.probe.site.ID)
		if err != nil {
			return err
		}
		f := &report.Findings[job.index]
		if mutated.kind == "failed" && flags&24 == 24 && slices.Contains(mutated.failed, job.test) {
			f.Witness.Verified = true
			f.Witness.Code = witnessCode(f.Witness)
		} else {
			f.Detail = "generated witness did not fail on the applied mutant"
			report.Incomplete++
		}
	}
	return nil
}
