package campaign

import (
	"context"
	"fmt"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

func auditVerdict(status string) string {
	switch status {
	case "killed", "non-viable":
		return status
	case "input-gap", "assertion-gap", "source-survivor":
		return "survived"
	}
	return ""
}

func (s *session) auditProbes(ctx context.Context, report *Report, probes []probeJob, completed []sourceResult, sourceCompleted []sourceResult, baseline commandResult) error {
	snapshots := make(map[string][]byte)
	for _, pkg := range s.packages {
		for _, file := range pkg.Files {
			snapshots[file.Path] = file.Data
		}
	}
	recordAudit := func(f Finding, job sourceJob) error {
		result := s.runSource(ctx, job, baseline)
		if result.err != nil {
			return result.err
		}
		report.SourceRuns += result.runs
		report.Audited++
		got := result.finding.Status
		if got == "timeout" && result.deadlineExceeded && s.cfg.TimeoutRecheck == "serial" {
			got = "killed"
		}
		if auditVerdict(f.Status) == auditVerdict(got) {
			return nil
		}
		f.Rule, f.Evidence = "audit", "source-replay"
		f.Detail = fmt.Sprintf("probe verdict %s, source verdict %s", f.Status, got)
		f.Tests, f.Witness = result.finding.Tests, nil
		if auditVerdict(got) == "" {
			f.Status = "not-evaluated"
			if result.finding.Detail != "" {
				f.Detail += "\n" + result.finding.Detail
			}
		} else {
			f.Status = "audit-disagreement"
		}
		s.identify(&f)
		report.add(f, s.cfg.KilledFindings)
		return nil
	}
	for i, probe := range probes {
		if report.Audited == s.cfg.Audit {
			break
		}
		if probe.candidate.Kind == "margin" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f := completed[i].finding
		if f.Status == "" && probe.predicted && len(probe.consumers) == 0 {
			f = probe.finding
		}
		if auditVerdict(f.Status) == "" {
			continue
		}
		site := probe.site
		edit := target.Edit{File: site.File, Start: site.Start, End: site.End, Before: site.Expression, After: "(" + f.Mutation + ")"}
		job := sourceJob{source: snapshots[site.File], edit: &edit, finding: f}
		if err := recordAudit(f, job); err != nil {
			return err
		}
	}
	for i, srcJob := range s.sources {
		if report.Audited == s.cfg.Audit {
			break
		}
		if srcJob.edit == nil || srcJob.edit.Site == nil || !srcJob.edit.Site.IsSchemata() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f := sourceCompleted[i].finding
		if auditVerdict(f.Status) == "" || (f.Evidence != "replayed" && f.Evidence != "observed") {
			continue
		}
		if err := recordAudit(f, srcJob); err != nil {
			return err
		}
	}
	return nil
}
