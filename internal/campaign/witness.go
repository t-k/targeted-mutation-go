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

func (s *session) verifyWitnesses(ctx context.Context, report *Report, probes []probeJob) {
	byLocation := make(map[string]probeJob)
	for _, probe := range probes {
		f := probe.finding
		if f.Witness != nil && f.Witness.Call != "" {
			byLocation[witnessKey(f)] = probe
		}
	}
	groups := make(map[string][]witnessJob)
	for i, finding := range report.Findings {
		if finding.Status != "input-gap" || finding.Witness == nil || finding.Witness.Call == "" {
			continue
		}
		probe, ok := byLocation[witnessKey(finding)]
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

func witnessKey(f Finding) string {
	return fmt.Sprintf("%s:%d:%d:%s", f.File, f.Line, f.Column, f.Rule)
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
		job.test = fmt.Sprintf("Test%sWitness%d", pkg.Prefix, i)
		w := report.Findings[job.index].Witness
		fmt.Fprintf(&source, "func %s(t *testing.T) {\nif got := %s; got != %t { t.Fatalf(\"witness: got %%v\", got) }\n}\n",
			job.test, w.Call, w.Original)
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
		} else {
			f.Detail = "generated witness did not fail on the applied mutant"
			report.Incomplete++
		}
	}
	return nil
}
