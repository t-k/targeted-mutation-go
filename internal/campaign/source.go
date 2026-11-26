package campaign

import (
	"context"
	"maps"
	"os"
	"path/filepath"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

type sourceJob struct {
	replacement target.Replacement
	source      []byte
	edit        *target.Edit
	finding     Finding
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
	dir, err := os.MkdirTemp(s.scratch, "source-")
	if err != nil {
		result.err = err
		return result
	}
	defer os.RemoveAll(dir)
	file := job.materialize()
	overlay, err := makeOverlay(dir, []target.Replacement{file})
	if err != nil {
		result.err = err
		return result
	}
	serial := *s
	serial.cfg.Jobs = 1
	run := serial.goTest(ctx, overlay, s.cfg.Patterns...)
	result.runs = 1
	var diagnostics []compilerDiagnostic
	if run.kind == "build-error" && job.edit != nil {
		diagnostics = compilerDiagnostics(run.detail)
		overlayFile := filepath.Join(dir, "source-0.go")
		if repaired := repairUnused(file.Data, overlayFile, diagnostics); repaired != nil {
			if err := os.WriteFile(overlayFile, repaired, 0600); err != nil {
				result.err = err
				return result
			}
			run = serial.goTest(ctx, overlay, s.cfg.Patterns...)
			result.runs++
			diagnostics = compilerDiagnostics(run.detail)
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
