package campaign

import (
	"context"
	"maps"
	"os"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

type sourceJob struct {
	replacement target.Replacement
	finding     Finding
}

func (s *session) runSource(ctx context.Context, job sourceJob, baseline commandResult) jobResult {
	result := jobResult{finding: job.finding}
	dir, err := os.MkdirTemp(s.scratch, "source-")
	if err != nil {
		result.err = err
		return result
	}
	defer os.RemoveAll(dir)
	overlay, err := makeOverlay(dir, []target.Replacement{job.replacement})
	if err != nil {
		result.err = err
		return result
	}
	serial := *s
	serial.cfg.Jobs = 1
	run := serial.goTest(ctx, overlay, s.cfg.Patterns...)
	result.runs = 1
	f := &result.finding
	f.Evidence = "source-replay"
	switch run.kind {
	case "passed":
		if !maps.Equal(baseline.packages, run.packages) || !maps.EqualFunc(baseline.inventory, run.inventory, sameInventory) {
			f.Status, f.Detail = "unstable", "source replay ran a different test inventory"
		} else if f.Rule == "revert" {
			f.Status = "untested-change"
		} else {
			f.Status = "source-survivor"
		}
	case "failed":
		f.Status, f.Tests = "killed", run.failed
	default:
		f.Status, f.Detail = run.kind, run.detail
	}
	return result
}
