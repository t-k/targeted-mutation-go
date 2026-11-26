package campaign

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

func previousTestSelection(ctx context.Context, cfg Config) ([]target.Replacement, []target.Package, error) {
	files, err := previousTestFiles(ctx, cfg.Dir, cfg.Since)
	if len(files) == 0 || err != nil {
		return nil, nil, err
	}
	if len(cfg.TestPatterns) == 0 {
		cfg.TestPatterns = cfg.Patterns
	}
	packages, err := resolveTestPackages(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	dirs := make(map[string]bool, len(packages))
	for _, pkg := range packages {
		dirs[pkg.Dir] = true
	}
	files = slices.DeleteFunc(files, func(f target.Replacement) bool { return !dirs[filepath.Dir(f.Path)] })
	return files, packages, nil
}

type previousCandidate struct {
	finding Finding
	probe   *probeJob
	source  *sourceJob
}

func (s *session) comparePrevious(ctx context.Context, report *Report, changed target.Scope, files []target.Replacement, reached [][]*consumer) error {
	candidates := s.previousCandidates(report, changed, reached)
	report.Findings = slices.DeleteFunc(report.Findings, func(f Finding) bool {
		return f.Rule != "revert" && !changed.Contains(filepath.Join(s.cfg.Dir, filepath.FromSlash(f.File)), f.Line, f.Line)
	})
	if len(candidates) == 0 {
		return nil
	}
	dir, err := os.MkdirTemp(s.scratch, "previous-tests-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	prior := *s
	prior.scratch, prior.consumers, prior.testOverlays, prior.journal = dir, nil, files, nil
	baselineOverlay, err := makeOverlay(dir, files)
	if err != nil {
		return err
	}
	base := prior.goTest(ctx, baselineOverlay, s.testPatterns()...)
	report.PreviousRuns++
	if base.kind != "passed" {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.uncheckedPrevious(report, files[0].Path, fmt.Sprintf("previous tests did not pass on the current source (%s)\n%s", base.kind, base.detail))
		return nil
	}
	var priorReached [][]*consumer
	if slices.ContainsFunc(candidates, func(c previousCandidate) bool { return c.probe != nil }) {
		probeDir, err := os.MkdirTemp(dir, "probes-")
		if err != nil {
			return err
		}
		prior.overlay, err = makeOverlay(probeDir, slices.Concat(s.replacements, files))
		if err != nil {
			return err
		}
		var scratchReport Report
		if err := prior.prepare(ctx, base, &scratchReport); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.uncheckedPrevious(report, files[0].Path, "could not instrument previous tests: "+err.Error())
			return nil
		}
		priorReached = prior.reachedConsumers()
	}
	results := parallel(ctx, s.cfg.Jobs, len(candidates), func(i int) jobResult {
		candidate := candidates[i]
		if candidate.source != nil {
			return prior.runSource(ctx, *candidate.source, base)
		}
		job := *candidate.probe
		for _, bin := range priorReached[job.site.ID] {
			if job.candidate.Infects(job.site.Op, bin.seen[job.site.ID]) {
				job.consumers = append(job.consumers, bin)
			}
		}
		return prior.replay(ctx, job)
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, result := range results {
		if result.err != nil {
			return result.err
		}
		report.PreviousRuns += result.runs
		if result.finding.Status == "killed" {
			f := candidates[i].finding
			f.Status, f.Evidence, f.Tests = "weakened-test", "previous-tests", result.finding.Tests
			f.Detail = "previous tests caught this mutation on the current source"
			report.add(f, s.cfg.KilledFindings)
		} else if previousUnconfirmed(result.finding.Status) {
			f := candidates[i].finding
			f.Status, f.Evidence = "not-evaluated", "previous-tests"
			f.Detail = fmt.Sprintf("previous test comparison %s: %s", result.finding.Status, result.finding.Detail)
			report.add(f, s.cfg.KilledFindings)
		}
	}
	return nil
}

func previousUnconfirmed(status string) bool {
	switch status {
	case "", "source-survivor", "non-viable", "assertion-gap", "input-gap":
		return false
	}
	return true
}

func (s *session) uncheckedPrevious(report *Report, file, detail string) {
	f := Finding{Status: "not-evaluated", Evidence: "previous-tests", File: s.rel(file), Line: 1, Column: 1, Rule: "weakened-test", Detail: detail}
	s.identify(&f)
	report.add(f, s.cfg.KilledFindings)
}

func (s *session) previousCandidates(report *Report, changed target.Scope, reached [][]*consumer) []previousCandidate {
	survivors := make(map[string]Finding)
	for _, f := range report.Findings {
		switch f.Status {
		case "input-gap", "assertion-gap", "source-survivor":
			if !changed.Contains(filepath.Join(s.cfg.Dir, filepath.FromSlash(f.File)), f.Line, f.Line) {
				survivors[f.ID] = f
			}
		}
	}
	var result []previousCandidate
	for _, site := range s.sites {
		if site.String || changed.Contains(site.File, site.Line, site.Line) {
			continue
		}
		for _, candidate := range analysis.Preview(site.Op, site.Domain, !s.cfg.Conventional) {
			if !s.comparisonEnabled(candidate.Kind) {
				continue
			}
			job := probeJob{site: site, candidate: candidate}
			job.finding = s.describe(job, analysis.Summary{})
			f, recorded := survivors[job.finding.ID]
			if !recorded {
				if len(reached[site.ID]) != 0 || !s.enabled(job.finding) {
					continue
				}
				f = job.finding
			}
			result = append(result, previousCandidate{finding: f, probe: &job})
		}
	}
	for i := range s.sources {
		job := &s.sources[i]
		if f, ok := survivors[job.finding.ID]; ok && job.edit != nil {
			result = append(result, previousCandidate{finding: f, source: job})
		}
	}
	return result
}
