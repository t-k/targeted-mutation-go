package campaign

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

type sourceJob struct {
	replacement target.Replacement
	source      []byte
	edit        *target.Edit
	finding     Finding
	pkgPath     string
}

type sourceResult struct {
	jobResult
	replays, skipped int
	resumed          bool
	timedOutKilled   bool
}

func (s *session) runSourceComparison(ctx context.Context, job sourceJob, reached []*consumer, baseline commandResult) sourceResult {
	if len(reached) == 0 {
		return sourceResult{jobResult: s.runSource(ctx, job, baseline)}
	}
	site := job.edit.Site
	candidate := analysis.Candidate{Op: job.edit.Operator, Pivot: job.edit.Pivot, Shift: job.edit.Shift, Float: site.Float}
	probe := probeJob{site: site, candidate: candidate, finding: job.finding}
	for _, bin := range reached {
		if candidate.Infects(site.Op, bin.seen[site.ID]) {
			probe.consumers = append(probe.consumers, bin)
		}
	}
	probe.predicted = len(probe.consumers) == 0
	result := sourceResult{skipped: len(reached) - len(probe.consumers)}
	if probe.predicted && (s.cfg.VerifySurvivors || s.cfg.Conventional) {
		probe.consumers = reached
		result.skipped = 0
	}
	probe.finding.Status, probe.finding.Evidence = "source-survivor", "observed"
	if len(probe.consumers) == 0 {
		result.finding = probe.finding
		return result
	}
	replayed := s.replay(ctx, probe)
	result.replays = replayed.runs
	if replayed.err != nil {
		result.err = replayed.err
		return result
	}
	result.finding = replayed.finding
	switch result.finding.Status {
	case "assertion-gap":
		result.finding.Status = "source-survivor"
	case "source-survivor", "killed":
	case "timeout":
		if replayed.activatedChanged && replayed.deadlineExceeded && s.cfg.Jobs == 1 && s.cfg.TimeoutRecheck != "none" {
			result.finding.Status, result.finding.Detail = "killed", "killed by timeout"
			result.timedOutKilled = true
			return result
		}
		if s.cfg.Jobs > 1 && s.cfg.TimeoutRecheck != "none" {
			return result
		}
		result.jobResult = s.runSource(ctx, job, baseline)
	default:
		// An unconfirmed probe verdict must be resolved using the actual edit.
		result.jobResult = s.runSource(ctx, job, baseline)
	}
	return result
}

func (job sourceJob) materialize() target.Replacement {
	if job.edit == nil {
		return job.replacement
	}
	e := job.edit
	data := make([]byte, 0, len(job.source)+len(e.After)-(e.End-e.Start))
	data = append(data, job.source[:e.Start]...)
	data = append(data, e.After...)
	data = append(data, job.source[e.End:]...)
	return target.Replacement{Path: e.File, Data: data}
}

func (s *session) runSource(ctx context.Context, job sourceJob, baseline commandResult) jobResult {
	result := jobResult{finding: job.finding}
	if !s.cfg.SourceOnly && job.edit != nil && job.edit.MissingReturn {
		result.finding.Status, result.finding.Evidence, result.finding.Detail = "non-viable", "static", "missing return"
		return result
	}
	dir, err := os.MkdirTemp(s.scratch, "source-")
	if err != nil {
		result.err = err
		return result
	}
	defer os.RemoveAll(dir)
	file := job.materialize()
	files := slices.Concat(s.testOverlays, []target.Replacement{file})
	overlay, err := makeOverlay(dir, files)
	if err != nil {
		result.err = err
		return result
	}
	overlayFile := filepath.Join(dir, fmt.Sprintf("source-%d.go", len(s.testOverlays)))
	serial := *s
	serial.cfg.Jobs = 1
	needsCompileCheck := len(s.cfg.TestPatterns) != 0 && job.pkgPath != "" && !s.testedDeps[job.pkgPath]
	if needsCompileCheck {
		check := serial.compilePackageOverlay(ctx, dir, overlay, job.pkgPath)
		result.runs++
		var checkDiags []compilerDiagnostic
		if check.kind == "build-error" && job.edit != nil {
			checkDiags = candidateDiagnostics(check, s.cfg.Dir, overlayFile)
			if repaired := repairUnused(file.Data, overlayFile, checkDiags); repaired != nil {
				file.Data = repaired
				if err := os.WriteFile(overlayFile, repaired, 0600); err != nil {
					result.err = err
					return result
				}
				check = serial.compilePackageOverlay(ctx, dir, overlay, job.pkgPath)
				result.runs++
				checkDiags = candidateDiagnostics(check, s.cfg.Dir, overlayFile)
			}
		}
		if check.kind != "passed" {
			result.finding.Evidence = "source-replay"
			if check.kind == "build-error" && len(checkDiags) != 0 {
				result.finding.Status, result.finding.Detail = "non-viable", checkDiags[0].message
			} else {
				result.finding.Status, result.finding.Detail = check.kind, check.detail
			}
			return result
		}
	}
	run := serial.goTest(ctx, overlay, s.testPatterns()...)
	result.runs++
	var diagnostics []compilerDiagnostic
	if run.kind == "build-error" && job.edit != nil {
		diagnostics = candidateDiagnostics(run, s.cfg.Dir, overlayFile)
		if repaired := repairUnused(file.Data, overlayFile, diagnostics); repaired != nil {
			if err := os.WriteFile(overlayFile, repaired, 0600); err != nil {
				result.err = err
				return result
			}
			run = serial.goTest(ctx, overlay, s.testPatterns()...)
			result.runs++
			diagnostics = candidateDiagnostics(run, s.cfg.Dir, overlayFile)
		}
	}
	f := &result.finding
	f.Evidence = "source-replay"
	switch {
	case run.kind == "passed":
		if !maps.Equal(baseline.packages, run.packages) || !maps.EqualFunc(baseline.inventory, run.inventory, sameInventory) {
			f.Status, f.Detail = "unstable", "source replay ran a different test inventory"
		} else if f.Rule == "revert" {
			f.Status = "untested-change"
		} else {
			f.Status = "source-survivor"
		}
	case run.kind == "failed":
		f.Status, f.Tests = "killed", run.failed
	case run.kind == "build-error" && len(diagnostics) != 0:
		f.Status, f.Detail = "non-viable", diagnostics[0].message
	default:
		f.Status, f.Detail = run.kind, run.detail
	}
	return result
}

func (s *session) compilePackageOverlay(ctx context.Context, dir, overlay, pkgPath string) commandResult {
	outBin := filepath.Join(dir, "compile-check.test")
	defer os.Remove(outBin)
	args := []string{"test", "-json", "-c", "-o", outBin, "-p=1", "-vet=off", "-overlay=" + overlay}
	args = append(args, s.buildFlags()...)
	args = append(args, pkgPath)
	return runCommand(ctx, s.cfg.BuildTimeout, s.cfg.Dir, cleanEnv(), "go", args...)
}
