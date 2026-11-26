// Package campaign builds and runs isolated test campaigns with a stock Go toolchain.
package campaign

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

type Config struct {
	Dir             string
	Patterns        []string
	Tags, Since     string
	Jobs            int
	Timeout         time.Duration
	BuildTimeout    time.Duration
	Full            bool
	Conventional    bool
	VerifySurvivors bool
	VerifyWitnesses bool
	ShowUnreached   bool
	UndoChanges     bool
	KilledFindings  bool
}

type consumer struct {
	pkg, dir, binary string
	seen             map[int]analysis.Summary
	tests            []string
}

type probeJob struct {
	site      *target.Site
	candidate analysis.Candidate
	consumers []*consumer
	predicted bool
	finding   Finding
}

type jobResult struct {
	finding Finding
	runs    int
	err     error
}

type session struct {
	cfg          Config
	env          goEnvironment
	scratch      string
	packages     []target.Package
	sites        []*target.Site
	sources      []sourceJob
	consumers    []consumer
	overlay      string
	replacements []target.Replacement
}

func Run(ctx context.Context, cfg Config) (Report, error) {
	report := Report{Schema: 1, Findings: make([]Finding, 0)}
	if cfg.Jobs <= 0 {
		cfg.Jobs = min(4, runtime.GOMAXPROCS(0))
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.BuildTimeout <= 0 {
		cfg.BuildTimeout = 5 * time.Minute
	}
	if len(cfg.Patterns) == 0 {
		cfg.Patterns = []string{"./..."}
	}
	var err error
	cfg.Dir, err = filepath.Abs(cfg.Dir)
	if err != nil {
		return report, err
	}
	cfg.Dir, err = filepath.EvalSymlinks(cfg.Dir)
	if err != nil {
		return report, err
	}
	if cfg.UndoChanges && cfg.Since == "" {
		return report, fmt.Errorf("undo-changes requires since")
	}
	if cfg.VerifyWitnesses && cfg.Conventional {
		return report, fmt.Errorf("verify-witnesses requires targeted analysis")
	}
	setup, cancel := context.WithTimeout(ctx, cfg.BuildTimeout)
	env, err := environment(setup, cfg.Dir)
	cancel()
	if err != nil {
		return report, err
	}
	scope, reversions, err := gitScope(ctx, cfg.Dir, cfg.Since, cfg.UndoChanges)
	if err != nil {
		return report, err
	}
	setup, cancel = context.WithTimeout(ctx, cfg.BuildTimeout)
	packages, err := target.Discover(setup, cfg.Dir, cfg.Patterns, cfg.Tags, scope, cfg.Full)
	cancel()
	if err != nil {
		return report, err
	}
	scratch, err := os.MkdirTemp("", "targeted-mutation-go-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(scratch)
	s := session{cfg: cfg, env: env, scratch: scratch, packages: packages}
	s.replacements = s.collect(reversions)
	report.Sites = len(s.sites)
	if len(s.sites)+len(s.sources) == 0 {
		return report, nil
	}

	base := s.goTest(ctx, "", cfg.Patterns...)
	if base.kind != "passed" {
		return report, fmt.Errorf("ordinary Go test baseline %s:\n%s", base.kind, base.detail)
	}
	if len(s.sites) > 0 {
		s.overlay, err = makeOverlay(scratch, s.replacements)
		if err != nil {
			return report, err
		}
		if err := s.prepare(ctx, base, &report); err != nil {
			return report, err
		}
	}
	probeJobs := s.plan(&report)
	results := parallel(ctx, cfg.Jobs, len(probeJobs), func(i int) jobResult {
		return s.replay(ctx, probeJobs[i])
	})
	if err := ctx.Err(); err != nil {
		return report, err
	}
	for _, item := range results {
		if item.err != nil {
			return report, item.err
		}
		report.Replays += item.runs
		report.add(item.finding, cfg.KilledFindings)
	}
	results = parallel(ctx, cfg.Jobs, len(s.sources), func(i int) jobResult {
		return s.runSource(ctx, s.sources[i])
	})
	if err := ctx.Err(); err != nil {
		return report, err
	}
	for _, item := range results {
		if item.err != nil {
			return report, item.err
		}
		report.SourceRuns += item.runs
		report.add(item.finding, cfg.KilledFindings)
	}
	if cfg.VerifyWitnesses {
		s.verifyWitnesses(ctx, &report, probeJobs)
	}
	slices.SortStableFunc(report.Findings, compareFindings)
	return report, ctx.Err()
}

func (s *session) collect(reverts map[string][]target.Replacement) []target.Replacement {
	var replacements []target.Replacement
	for _, pkg := range s.packages {
		hasSites := false
		for _, file := range pkg.Files {
			for _, edit := range file.Edits {
				mutated := make([]byte, 0, len(file.Data)+len(edit.After)-len(edit.Before))
				mutated = append(mutated, file.Data[:edit.Start]...)
				mutated = append(mutated, edit.After...)
				mutated = append(mutated, file.Data[edit.End:]...)
				s.sources = append(s.sources, sourceJob{
					replacement: target.Replacement{Path: file.Path, Data: mutated},
					finding: Finding{File: s.rel(edit.File), Line: edit.Line, Column: edit.Column,
						Expression: edit.Before, Mutation: edit.After, Rule: edit.Kind},
				})
			}
			if len(file.Sites) == 0 {
				continue
			}
			hasSites = true
			for _, site := range file.Sites {
				site.ID = len(s.sites)
				s.sites = append(s.sites, site)
			}
			replacements = append(replacements, target.Replacement{Path: file.Path, Data: target.Rewrite(file, pkg.Prefix)})
		}
		if hasSites {
			replacements = append(replacements, target.Runtime(pkg)...)
		}
		for _, entry := range reverts[pkg.Dir] {
			if !slices.Contains(pkg.Paths, entry.Path) {
				continue
			}
			s.sources = append(s.sources, sourceJob{
				replacement: entry,
				finding: Finding{File: s.rel(entry.Path), Line: entry.Line, Column: 1,
					Rule: "revert", Mutation: "restore previous Git hunk"},
			})
		}
	}
	return replacements
}

func (s *session) prepare(ctx context.Context, base commandResult, report *Report) error {
	for i, pkg := range s.packages {
		if base.packages[pkg.Path] == "skip" {
			continue
		}
		consumer := consumer{
			pkg: pkg.Path, dir: pkg.Dir,
			binary: filepath.Join(s.scratch, fmt.Sprintf("package-%d.test", i)),
		}
		s.consumers = append(s.consumers, consumer)
	}
	results := parallel(ctx, s.cfg.Jobs, len(s.consumers), func(i int) error {
		bin := &s.consumers[i]
		if err := s.build(ctx, s.overlay, bin, 1); err != nil {
			return err
		}
		trace, err := newTrace(s.scratch, len(s.sites))
		if err != nil {
			return err
		}
		defer os.Remove(trace)
		run := s.runBinary(ctx, bin, trace, "", "")
		if run.kind != "passed" {
			return fmt.Errorf("instrumented baseline %s for %s:\n%s", run.kind, bin.pkg, run.detail)
		}
		if !sameInventory(base.inventory[bin.pkg], run.tests) {
			return fmt.Errorf("instrumented baseline ran different test cases in %s", bin.pkg)
		}
		data, err := readTrace(trace, len(s.sites))
		if err != nil {
			return err
		}
		bin.tests = run.tests
		bin.seen = make(map[int]analysis.Summary)
		for i := range s.sites {
			obs := summary(data, i)
			if obs.Flags != 0 {
				bin.seen[i] = obs
			}
		}
		return nil
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, err := range results {
		if err != nil {
			return err
		}
	}
	report.Builds = len(s.consumers)
	return nil
}

func sameInventory(first, second []string) bool {
	a, b := slices.Clone(first), slices.Clone(second)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func (s *session) build(ctx context.Context, overlay string, bin *consumer, jobs int) error {
	args := []string{"test", "-c", "-o", bin.binary, "-overlay=" + overlay, "-p=" + strconv.Itoa(jobs)}
	args = append(args, s.buildFlags()...)
	args = append(args, bin.pkg)
	build := runCommand(ctx, s.cfg.BuildTimeout, s.cfg.Dir, cleanEnv(), "go", args...)
	if build.kind != "passed" {
		return fmt.Errorf("test build %s for %s:\n%s", build.kind, bin.pkg, build.detail)
	}
	if _, err := os.Stat(bin.binary); err != nil {
		return fmt.Errorf("missing test binary for %s: %w", bin.pkg, err)
	}
	return nil
}

func (s *session) runBinary(ctx context.Context, bin *consumer, trace, mutation, test string) commandResult {
	args := []string{"-test.v=test2json", "-test.timeout=" + s.cfg.Timeout.String()}
	if test != "" {
		args = append(args, "-test.run=^"+test+"$")
	}
	return runCommand(ctx, s.cfg.Timeout+2*time.Second, bin.dir, binaryEnv(s.env, trace, mutation), bin.binary, args...)
}

func (s *session) buildFlags() []string {
	var flags []string
	if s.cfg.Tags != "" {
		flags = append(flags, "-tags="+s.cfg.Tags)
	}
	return flags
}

func (s *session) goTest(ctx context.Context, overlay string, patterns ...string) commandResult {
	args := []string{"test", "-json", "-count=1", "-timeout=" + s.cfg.Timeout.String(), "-p=" + strconv.Itoa(s.cfg.Jobs)}
	if overlay != "" {
		args = append(args, "-overlay="+overlay)
	}
	args = append(args, s.buildFlags()...)
	args = append(args, patterns...)
	return runCommand(ctx, s.cfg.BuildTimeout, s.cfg.Dir, cleanEnv(), "go", args...)
}

func (s *session) plan(report *Report) []probeJob {
	var jobs []probeJob
	for _, site := range s.sites {
		var seen analysis.Summary
		var reached []*consumer
		for i := range s.consumers {
			bin := &s.consumers[i]
			if obs, ok := bin.seen[site.ID]; ok {
				seen.Merge(obs)
				reached = append(reached, bin)
			}
		}
		if seen.Flags == 0 {
			report.Unreached++
			if s.cfg.ShowUnreached {
				report.Findings = append(report.Findings, Finding{Status: "unreached",
					File: s.rel(site.File), Line: site.Line, Column: site.Column, Expression: site.Expression})
			}
			continue
		}
		for _, candidate := range analysis.Plan(site.Op, site.Domain, seen, !s.cfg.Conventional) {
			job := probeJob{site: site, candidate: candidate, predicted: !candidate.Infects(site.Op, seen)}
			job.finding = s.describe(job, seen)
			if job.predicted && !s.cfg.VerifySurvivors && !s.cfg.Conventional {
				report.Skipped += len(reached)
				report.add(job.finding, s.cfg.KilledFindings)
				jobs = append(jobs, job)
				continue
			}
			for _, bin := range reached {
				if job.predicted || candidate.Infects(site.Op, bin.seen[site.ID]) {
					job.consumers = append(job.consumers, bin)
				} else {
					report.Skipped++
				}
			}
			jobs = append(jobs, job)
		}
	}
	return jobs
}

func (s *session) describe(job probeJob, seen analysis.Summary) Finding {
	site, c := job.site, job.candidate
	f := Finding{File: s.rel(site.File), Line: site.Line, Column: site.Column, Expression: site.Expression, Rule: c.Kind}
	f.Mutation = fmt.Sprintf("%s %s %s", site.Left, c.Op, site.Right)
	if c.Shift != 0 {
		f.Mutation = fmt.Sprintf("%s %s %s", site.Left, c.Op, rankString(*site.Domain, c.Pivot))
	}
	if job.predicted {
		f.Status, f.Evidence = "input-gap", "observed"
		if site.Domain != nil && c.Witness != nil {
			w := *c.Witness
			input := rankString(*site.Domain, w)
			f.Witness = &Witness{Input: input, Operand: site.Left,
				Original: analysis.Evaluate(site.Op, w, site.Domain.Pivot), Mutant: analysis.Evaluate(c.Op, w, c.Pivot)}
			if site.Callable != "" {
				f.Witness.Call = fmt.Sprintf("%s(%s)", site.Callable, input)
			}
			if lo, hi, ok := analysis.ThresholdInterval(site.Op, *site.Domain, seen); ok {
				f.Interval = []string{rankString(*site.Domain, lo), rankString(*site.Domain, hi)}
			}
		}
	}
	return f
}

func rankString(d analysis.Domain, rank uint64) string {
	if d.Signed {
		return strconv.FormatInt(analysis.UnrankSigned(rank), 10)
	}
	return strconv.FormatUint(rank, 10)
}

func (s *session) replay(ctx context.Context, job probeJob) jobResult {
	if len(job.consumers) == 0 {
		return jobResult{}
	}
	result := jobResult{finding: job.finding}
	f := &result.finding
	mode := selector(job.site, job.candidate)
	f.Evidence = "replayed"
	for _, bin := range job.consumers {
		trace, err := newTrace(s.scratch, len(s.sites))
		if err != nil {
			result.err = err
			return result
		}
		run := s.runBinary(ctx, bin, trace, mode, "")
		flags, err := readFlags(trace, job.site.ID)
		_ = os.Remove(trace)
		if err != nil {
			result.err = err
			return result
		}
		result.runs++
		f.Packages = append(f.Packages, bin.pkg)
		if run.kind != "passed" && run.kind != "failed" {
			f.Status, f.Detail = run.kind, run.detail
			return result
		}
		if flags&8 == 0 || (flags&16 != 0) == job.predicted ||
			run.kind == "failed" && job.predicted {
			f.Status, f.Detail = "unstable", "replay disagreed with baseline observations"
			return result
		}
		if run.kind == "passed" && !sameInventory(bin.tests, run.tests) {
			f.Status, f.Detail = "unstable", "mutation replay ran a different test inventory"
			return result
		}
		if run.kind == "failed" {
			f.Status, f.Tests = "killed", run.failed
			return result
		}
	}
	if !job.predicted {
		f.Status = "assertion-gap"
	}
	return result
}

func selector(site *target.Site, candidate analysis.Candidate) string {
	return fmt.Sprintf("%d:%d:%d:%t", site.ID, candidate.Op.Mask(), candidate.Pivot, candidate.Shift != 0)
}

func (s *session) rel(path string) string {
	if relative, err := filepath.Rel(s.cfg.Dir, path); err == nil {
		return filepath.ToSlash(relative)
	}
	return path
}

func compareFindings(a, b Finding) int {
	if order := strings.Compare(a.File, b.File); order != 0 {
		return order
	}
	if a.Line != b.Line {
		return a.Line - b.Line
	}
	if a.Column != b.Column {
		return a.Column - b.Column
	}
	if order := strings.Compare(a.Rule, b.Rule); order != 0 {
		return order
	}
	return strings.Compare(a.Mutation, b.Mutation)
}

func (r *Report) add(f Finding, killedFindings bool) {
	switch f.Status {
	case "":
		return
	case "killed":
		r.Killed++
		if !killedFindings {
			return
		}
	case "input-gap", "assertion-gap", "source-survivor", "untested-change":
	default:
		r.Incomplete++
	}
	r.Findings = append(r.Findings, f)
}

func parallel[T any](ctx context.Context, jobs, count int, run func(int) T) []T {
	results := make([]T, count)
	var index atomic.Int64
	var group sync.WaitGroup
	for range min(jobs, count) {
		group.Go(func() {
			for ctx.Err() == nil {
				i := int(index.Add(1) - 1)
				if i >= count {
					return
				}
				results[i] = run(i)
			}
		})
	}
	group.Wait()
	return results
}
