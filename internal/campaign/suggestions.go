package campaign

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

func exactTestPattern(name string) string {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = "^" + regexp.QuoteMeta(part) + "$"
	}
	return strings.Join(parts, "/")
}

func witnessCall(site *target.Site, input string) string {
	if site.Callable == "" {
		return ""
	}
	args := slices.Clone(site.Arguments)
	args[site.InputArgument] = input
	return site.Callable + "(" + strings.Join(args, ", ") + ")"
}

func (s *session) suggestAssertions(ctx context.Context, report *Report, jobs []probeJob) {
	probes := make(map[string]probeJob, len(jobs))
	for _, job := range jobs {
		probes[job.finding.ID] = job
	}
	for i := range report.Findings {
		f := &report.Findings[i]
		job, ok := probes[f.ID]
		if !ok || f.Status != "assertion-gap" || ctx.Err() != nil {
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
		s.suggestProbeAssertion(bounded, f, job)
		cancel()
	}
}

func (s *session) suggestProbeAssertion(ctx context.Context, f *Finding, job probeJob) {
	const maxTrials = 32
	trials := 0
	for _, bin := range job.consumers {
		// TestMain or initialization can touch a site without any selected test.
		base, outside, ok := s.observeTest(ctx, bin, job.site.ID, "^$", "")
		if !ok || base.kind != "passed" || outside.Flags != 0 || len(base.tests) != 0 {
			continue
		}
		for _, name := range leafTests(bin.tests) {
			if trials == maxTrials || ctx.Err() != nil {
				return
			}
			trials++
			pattern := exactTestPattern(name)
			original, seen, ok := s.observeTest(ctx, bin, job.site.ID, pattern, "")
			if !ok || original.kind != "passed" || !slices.Contains(original.events, "pass:"+name) ||
				!job.candidate.Infects(job.site.Op, seen) {
				continue
			}
			mutated, activated, ok := s.observeTest(ctx, bin, job.site.ID, pattern, selector(job.site, job.candidate))
			if !ok || mutated.kind != "passed" || activated.Flags&24 != 24 || !sameInventory(original.events, mutated.events) {
				continue
			}
			if !slices.Contains(f.Tests, name) {
				f.Tests = append(f.Tests, name)
			}
			if f.Witness == nil && job.site.Callable != "" && job.site.Domain != nil {
				f.Witness = observedWitness(job, seen)
			}
		}
	}
}

func leafTests(events []string) []string {
	var names []string
	for _, event := range events {
		if name, ok := strings.CutPrefix(event, "pass:"); ok && strings.HasPrefix(name, "Test") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	return slices.DeleteFunc(slices.Clone(names), func(name string) bool {
		index, _ := slices.BinarySearch(names, name+"/")
		return index < len(names) && strings.HasPrefix(names[index], name+"/")
	})
}

func (s *session) observeTest(ctx context.Context, bin *consumer, site int, pattern, mutation string) (commandResult, analysis.Summary, bool) {
	trace, err := newTrace(s.scratch, len(s.sites))
	if err != nil {
		return commandResult{}, analysis.Summary{}, false
	}
	defer os.Remove(trace)
	run := s.runBinaryPattern(ctx, bin, trace, mutation, pattern)
	data, err := readTrace(trace, len(s.sites))
	if err != nil {
		return run, analysis.Summary{}, false
	}
	return run, summary(data, site), true
}

func observedWitness(job probeJob, seen analysis.Summary) *Witness {
	site, candidate := job.site, job.candidate
	for _, point := range []struct{ flag, value uint64 }{{analysis.Equal, site.Domain.Pivot}, {analysis.Less, seen.Below}, {analysis.Greater, seen.Above}} {
		if seen.Flags&point.flag == 0 {
			continue
		}
		original := analysis.Evaluate(site.Op, point.value, site.Domain.Pivot)
		mutant := analysis.Evaluate(candidate.Op, point.value, candidate.Pivot)
		if original != mutant && site.Domain.Possible(point.value) {
			input := rankString(*site.Domain, point.value)
			return &Witness{Input: input, Operand: site.Left, Original: original, Mutant: mutant, Call: witnessCall(site, input)}
		}
	}
	return nil
}
