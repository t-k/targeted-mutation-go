// Package campaign builds and runs isolated test campaigns with a stock Go toolchain.
package campaign

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/checkpoint"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

type Config struct {
	Dir               string
	Patterns          []string
	TestPatterns      []string
	Lines             []string
	TestRun           string
	ExcludeRules      []string
	List              bool
	Shard             string
	Checkpoint        string
	Audit             int
	Tags, Since       string
	Comparison        string
	Jobs              int
	Timeout           time.Duration
	TimeoutExplicit   bool
	TimeoutRecheck    string
	BuildTimeout      time.Duration
	Full              bool
	SourceOnly        bool
	IncludeGenerated  bool
	Conventional      bool
	VerifySurvivors   bool
	VerifyWitnesses   bool
	SuggestAssertions bool
	ShowUnreached     bool
	UndoChanges       bool
	KilledFindings    bool
	Warn              io.Writer `json:"-"`
}

type consumer struct {
	pkg, dir, binary string
	seen             map[int]analysis.Summary
	tests            []string
}

type probeJob struct {
	site      *target.Site
	candidate analysis.Candidate
	arithOp   uint64
	consumers []*consumer
	predicted bool
	finding   Finding
}

type jobResult struct {
	finding          Finding
	runs             int
	err              error
	activatedChanged bool
	deadlineExceeded bool
}

type session struct {
	cfg          Config
	env          goEnvironment
	scratch      string
	packages     []target.Package
	testPackages []target.Package
	sites        []*target.Site
	sources      []sourceJob
	consumers    []consumer
	overlay      string
	replacements []target.Replacement
	anchors      map[string][]sourceAnchor
	ignored      map[string]map[int]string
	shard        shardSelection
	journal      *checkpoint.Journal[Finding]
	baselineKey  string
	testOverlays []target.Replacement
	virtualDirs  map[string]string
	testedDeps   map[string]bool
	workerProcs  int
	coverProfile string
	coverPkg     string
	coverage     *coverageMap
}

func Run(ctx context.Context, cfg Config) (Report, error) {
	report := Report{Schema: 1, Findings: make([]Finding, 0)}
	if cfg.Jobs <= 0 {
		cfg.Jobs = min(4, runtime.GOMAXPROCS(0))
	}
	explicitTimeout := cfg.TimeoutExplicit || cfg.Timeout > 0
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.TimeoutRecheck == "" {
		cfg.TimeoutRecheck = "serial"
	}
	if cfg.TimeoutRecheck != "serial" && cfg.TimeoutRecheck != "none" {
		return report, fmt.Errorf("timeout-recheck must be serial or none")
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
	if cfg.Comparison != "" && cfg.Comparison != "all" && cfg.Comparison != "boundary" && cfg.Comparison != "negation" {
		return report, fmt.Errorf("comparison must be all, boundary, or negation")
	}
	if cfg.UndoChanges && cfg.Since == "" {
		return report, fmt.Errorf("undo-changes requires since")
	}
	if cfg.VerifyWitnesses && cfg.Conventional {
		return report, fmt.Errorf("verify-witnesses is incompatible with conventional analysis")
	}
	if cfg.Audit < 0 || cfg.Audit != 0 && (cfg.SourceOnly || cfg.List) {
		return report, fmt.Errorf("audit requires a nonnegative sample size and probe execution")
	}
	shard, err := parseShard(cfg.Shard)
	if err != nil {
		return report, err
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
	var selected target.Scope
	if len(cfg.Lines) != 0 {
		selected, err = explicitScope(cfg.Dir, cfg.Lines)
		if err != nil {
			return report, err
		}
		scope = intersectScope(scope, selected)
		for dir, entries := range reversions {
			reversions[dir] = slices.DeleteFunc(entries, func(entry target.Replacement) bool {
				return !selected.Contains(entry.Path, entry.Line, entry.EndLine)
			})
		}
	}
	productionScope := scope
	var previous []target.Replacement
	var testPackages []target.Package
	if cfg.Since != "" && !cfg.List {
		setup, cancel := context.WithTimeout(ctx, cfg.BuildTimeout)
		previous, testPackages, err = previousTestSelection(setup, cfg)
		cancel()
		if err != nil {
			return report, err
		}
		if len(previous) != 0 {
			scope = selected
		}
	}
	setup, cancel = context.WithTimeout(ctx, cfg.BuildTimeout)
	packages, err := target.Discover(setup, cfg.Dir, cfg.Patterns, cfg.Tags, scope,
		target.Discovery{Full: cfg.Full, SourceOnly: cfg.SourceOnly, IncludeGenerated: cfg.IncludeGenerated})
	cancel()
	if err != nil {
		return report, err
	}
	rawScratch, err := os.MkdirTemp("", "targeted-mutation-go-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(rawScratch)
	scratch, err := filepath.Abs(rawScratch)
	if err != nil {
		return report, err
	}
	if resolvedScratch, evalErr := filepath.EvalSymlinks(scratch); evalErr == nil {
		scratch = resolvedScratch
	}
	workerProcs := 0
	if cfg.Jobs > 1 {
		workerProcs = max(1, runtime.GOMAXPROCS(0)/cfg.Jobs)
	}
	s := session{cfg: cfg, env: env, scratch: scratch, packages: packages, testPackages: packages, shard: shard, workerProcs: workerProcs}
	if testPackages != nil {
		s.testPackages = testPackages
	} else if len(cfg.TestPatterns) != 0 && !cfg.List {
		setup, cancel := context.WithTimeout(ctx, cfg.BuildTimeout)
		s.testPackages, err = resolveTestPackages(setup, cfg)
		cancel()
		if err != nil {
			return report, err
		}
	}
	report.WorkspaceDirs = s.collectWorkspaceDirs()
	s.replacements = s.collect(reversions)
	for _, site := range s.sites {
		if !site.Arith {
			report.Sites++
		}
	}
	if err := s.validateSnapshots(); err != nil {
		return report, err
	}
	if cfg.List {
		if cfg.Checkpoint != "" {
			return report, fmt.Errorf("checkpoint applies to evaluated mutations")
		}
		s.preview(&report)
		return report, ctx.Err()
	}
	if cfg.Checkpoint != "" {
		setup, cancel := context.WithTimeout(ctx, cfg.BuildTimeout)
		err := s.openCheckpoint(setup)
		cancel()
		if err != nil {
			return report, err
		}
		defer s.journal.Close()
	}
	if len(cfg.TestPatterns) != 0 && (cfg.Full || cfg.SourceOnly || cfg.Audit > 0) {
		setup, cancel := context.WithTimeout(ctx, cfg.BuildTimeout)
		s.testedDeps = s.resolveTestedDependencies(setup)
		cancel()
	}
	baselineConfig := s
	baselineConfig.workerProcs = 0
	if !explicitTimeout {
		baselineConfig.cfg.Timeout = min(s.cfg.BuildTimeout, 2*time.Minute)
	}
	if s.cfg.Full && !s.cfg.SourceOnly && !s.cfg.Conventional && !s.cfg.VerifySurvivors && len(s.virtualDirs) == 0 && len(s.sources) > 0 {
		var coverPkgs []string
		seenPkg := make(map[string]bool, len(s.packages))
		for _, pkg := range s.packages {
			if pkg.Path != "" && !seenPkg[pkg.Path] && len(pkg.Files) > 0 {
				seenPkg[pkg.Path] = true
				coverPkgs = append(coverPkgs, pkg.Path)
			}
		}
		if len(coverPkgs) > 0 {
			baselineConfig.coverProfile = filepath.Join(s.scratch, "baseline.cover")
			baselineConfig.coverPkg = strings.Join(coverPkgs, ",")
		}
	}
	base := baselineConfig.goTest(ctx, "", s.testPatterns()...)
	if base.kind != "passed" {
		return report, fmt.Errorf("ordinary Go test baseline %s:\n%s", base.kind, base.detail)
	}
	if baselineConfig.coverProfile != "" {
		for _, pkg := range s.testPackages {
			if base.packages[pkg.Path] == "pass" && !dirHasTestFiles(pkg.Dir) {
				base.packages[pkg.Path] = "skip"
			}
		}
		s.coverage = loadCoverageProfile(baselineConfig.coverProfile, s.packages)
	}
	if s.cfg.TestRun != "" && countRunEvents(base.inventory) == 0 {
		return report, fmt.Errorf("no tests matched --test %q", s.cfg.TestRun)
	}
	baselineTestElapsed := base.maxTestElapsed
	if !base.hasTestElapsed {
		baselineTestElapsed = base.elapsed
	}
	if !explicitTimeout {
		s.cfg.Timeout = 5*time.Second + 5*baselineTestElapsed
	} else if baselineTestElapsed >= s.cfg.Timeout && s.cfg.Warn != nil {
		fmt.Fprintf(s.cfg.Warn, "warning: baseline took %s, meeting or exceeding --timeout %s\n",
			baselineTestElapsed.Round(time.Millisecond), s.cfg.Timeout)
	}
	if s.journal != nil {
		s.baselineKey = canonicalBaselineKey(base.packages, base.inventory)
	}
	if err := s.validateSnapshots(); err != nil {
		return report, err
	}
	if len(s.sites)+len(s.sources) == 0 {
		report.Evaluation = "no-sites"
		report.Reconcile()
		return report, nil
	}
	if len(s.sites) > 0 {
		for _, replacement := range s.replacements {
			if replacement.Synthetic {
				if _, err := os.Lstat(replacement.Path); !os.IsNotExist(err) {
					return report, fmt.Errorf("synthetic probe path is occupied: %s", replacement.Path)
				}
			}
		}
		s.overlay, err = makeOverlay(scratch, s.replacements)
		if err != nil {
			return report, err
		}
		if err := s.prepare(ctx, base, &report); err != nil {
			return report, err
		}
	}
	reached := s.reachedConsumers()
	var allObserved []Finding
	probeJobs := s.plan(&report, reached, &allObserved)
	replayTotal := 0
	for _, job := range probeJobs {
		if len(job.consumers) > 0 {
			replayTotal++
		}
	}
	probeProgress := startProgress(s.cfg.Warn, "probe replays", replayTotal)
	results := parallel(ctx, cfg.Jobs, len(probeJobs), func(i int) sourceResult {
		job := probeJobs[i]
		if len(job.consumers) == 0 {
			return sourceResult{}
		}
		res := s.cachedEdit(job.finding, job.site, job.consumers, func() sourceResult {
			result := s.replay(ctx, job)
			return sourceResult{jobResult: result, replays: result.runs}
		})
		probeProgress.record(res.finding.Status)
		return res
	})
	probeProgress.stop()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	probeRechecks := 0
	if s.cfg.TimeoutRecheck == "serial" && s.cfg.Jobs > 1 {
		for i := range results {
			if results[i].finding.Status == "timeout" && !results[i].resumed {
				probeRechecks++
			}
		}
	}
	probeRecheckProgress := startProgress(s.cfg.Warn, "serial timeout rechecks", probeRechecks)
	for i := range results {
		item := &results[i]
		if item.err != nil {
			probeRecheckProgress.stop()
			return report, item.err
		}
		if item.finding.Status == "timeout" && !item.resumed && s.cfg.TimeoutRecheck == "serial" {
			if s.cfg.Jobs > 1 {
				serialSession := s
				serialSession.cfg.Jobs = 1
				serialSession.workerProcs = 0
				rechecked := serialSession.replay(ctx, probeJobs[i])
				if rechecked.err != nil {
					probeRecheckProgress.stop()
					return report, rechecked.err
				}
				item.replays += rechecked.runs
				item.jobResult = rechecked
			}
			if item.finding.Status == "timeout" && item.activatedChanged && item.deadlineExceeded {
				item.finding.Status, item.finding.Detail = "killed", "killed by timeout"
				item.timedOutKilled = true
			}
			probeRecheckProgress.record(item.finding.Status)
		}
		report.Replays += item.replays
		if item.resumed {
			report.Resumed++
			item.finding.StartLine, item.finding.EndLine = probeJobs[i].finding.StartLine, probeJobs[i].finding.EndLine
		}
		if item.finding.Status != "" {
			allObserved = append(allObserved, item.finding)
		}
		s.recordFinding(&report, item.finding, item.timedOutKilled)
	}
	probeRecheckProgress.stop()
	sourceProgress := startProgress(s.cfg.Warn, "source mutations", len(s.sources))
	sourceResults := parallel(ctx, cfg.Jobs, len(s.sources), func(i int) sourceResult {
		job := s.sources[i]
		var res sourceResult
		if job.edit != nil && job.edit.Site != nil {
			site := job.edit.Site
			res = s.cachedEdit(job.finding, site, reached[site.ID], func() sourceResult {
				return s.runSourceComparison(ctx, job, reached[site.ID], base)
			})
		} else if job.edit != nil && isCoveragePrunableEdit(job.edit, job.source) && s.coverage.isUncovered(job.edit.File, job.source, job.edit) {
			f := job.finding
			f.Status, f.Evidence = "source-survivor", "observed"
			res = sourceResult{jobResult: jobResult{finding: f}, skipped: 1}
		} else {
			res = s.cachedEdit(job.finding, nil, nil, func() sourceResult {
				return sourceResult{jobResult: s.runSource(ctx, job, base)}
			})
		}
		sourceProgress.record(res.finding.Status)
		return res
	})
	sourceProgress.stop()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	sourceRechecks := 0
	if s.cfg.TimeoutRecheck == "serial" && s.cfg.Jobs > 1 {
		for i := range sourceResults {
			if sourceResults[i].finding.Status == "timeout" && !sourceResults[i].resumed {
				sourceRechecks++
			}
		}
	}
	sourceRecheckProgress := startProgress(s.cfg.Warn, "serial timeout rechecks", sourceRechecks)
	for i := range sourceResults {
		item := &sourceResults[i]
		if item.err != nil {
			sourceRecheckProgress.stop()
			return report, item.err
		}
		job := s.sources[i]
		if item.finding.Status == "timeout" && !item.resumed && s.cfg.TimeoutRecheck == "serial" {
			if s.cfg.Jobs > 1 {
				serialSession := s
				serialSession.cfg.Jobs = 1
				serialSession.workerProcs = 0
				if job.edit != nil && job.edit.Site != nil {
					rechecked := serialSession.runSourceComparison(ctx, job, reached[job.edit.Site.ID], base)
					if rechecked.err != nil {
						sourceRecheckProgress.stop()
						return report, rechecked.err
					}
					item.replays += rechecked.replays
					item.runs += rechecked.runs
					item.finding = rechecked.finding
					item.deadlineExceeded = rechecked.deadlineExceeded
					item.timedOutKilled = rechecked.timedOutKilled
				} else {
					rechecked := serialSession.runSource(ctx, job, base)
					if rechecked.err != nil {
						sourceRecheckProgress.stop()
						return report, rechecked.err
					}
					item.runs += rechecked.runs
					item.finding = rechecked.finding
					item.deadlineExceeded = rechecked.deadlineExceeded
				}
			}
			if item.finding.Status == "timeout" && item.deadlineExceeded {
				item.finding.Status, item.finding.Detail = "killed", "killed by timeout"
				item.timedOutKilled = true
			}
			sourceRecheckProgress.record(item.finding.Status)
			if s.journal != nil && checkpointVerdict(item.finding.Status) && !item.timedOutKilled && item.finding.Detail != "killed by timeout" {
				var site *target.Site
				var siteReached []*consumer
				if job.edit != nil && job.edit.Site != nil {
					site = job.edit.Site
					siteReached = reached[site.ID]
				}
				_ = s.storeCachedFinding(job.finding, site, siteReached, item.finding)
			}
		}
		report.SourceRuns += item.runs
		report.Replays += item.replays
		report.Skipped += item.skipped
		if item.resumed {
			report.Resumed++
			item.finding.StartLine, item.finding.EndLine = job.finding.StartLine, job.finding.EndLine
		}
		if item.finding.Status != "" {
			allObserved = append(allObserved, item.finding)
		}
		s.recordFinding(&report, item.finding, item.timedOutKilled)
	}
	sourceRecheckProgress.stop()
	if cfg.Audit != 0 {
		beforeAuditLen := len(report.Findings)
		if err := s.auditProbes(ctx, &report, probeJobs, results, base); err != nil {
			return report, err
		}
		for _, added := range report.Findings[beforeAuditLen:] {
			allObserved = append(allObserved, added)
		}
	}
	if len(previous) != 0 {
		if err := s.comparePrevious(ctx, &report, productionScope, previous, reached, allObserved); err != nil {
			return report, err
		}
	}
	if cfg.SuggestAssertions {
		s.suggestAssertions(ctx, &report, probeJobs)
	}
	if cfg.VerifyWitnesses || cfg.SuggestAssertions {
		s.verifyWitnesses(ctx, &report, probeJobs)
		s.verifySourceWitnesses(ctx, &report)
	}
	if err := s.validateSnapshots(); err != nil {
		return report, err
	}
	slices.SortStableFunc(report.Findings, compareFindings)
	report.Reconcile()
	return report, ctx.Err()
}

func countRunEvents(inventory map[string][]string) int {
	count := 0
	for _, events := range inventory {
		for _, event := range events {
			if strings.HasPrefix(event, "run:") {
				count++
			}
		}
	}
	return count
}

func (s *session) collectWorkspaceDirs() []string {
	var dirs []string
	for _, pkg := range slices.Concat(s.packages, s.testPackages) {
		if pkg.Dir != "" {
			dir := pkg.Dir
			if resolved, err := filepath.EvalSymlinks(dir); err == nil {
				dir = resolved
			}
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}

func (s *session) resolveTestedDependencies(ctx context.Context) map[string]bool {
	args := append([]string{"list", "-deps", "-test", "-json"}, s.buildFlags()...)
	args = append(args, s.testPatterns()...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Env, cmd.Dir = cleanEnv(), s.cfg.Dir
	configureProcess(cmd)
	out, err := cmd.Output()
	cleanupProcess(cmd)
	if err != nil {
		return nil
	}
	deps := make(map[string]bool)
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg struct{ ImportPath string }
		if err := decoder.Decode(&pkg); err != nil {
			break
		}
		if pkg.ImportPath != "" {
			deps[pkg.ImportPath] = true
		}
	}
	return deps
}

func (s *session) storeCachedFinding(f Finding, site *target.Site, reached []*consumer, result Finding) error {
	if s.journal == nil {
		return nil
	}
	type observation struct {
		Package string
		Summary analysis.Summary
	}
	var trace []observation
	if site != nil {
		for _, bin := range reached {
			trace = append(trace, observation{Package: bin.pkg, Summary: bin.seen[site.ID]})
		}
		slices.SortFunc(trace, func(a, b observation) int { return strings.Compare(a.Package, b.Package) })
	}
	key := stableHash(f.ID, s.baselineKey, trace)
	return s.journal.Put(key, result)
}

func (s *session) validateSnapshots() error {
	for _, pkg := range s.packages {
		for _, file := range pkg.Files {
			current, err := os.ReadFile(file.Path)
			if err != nil {
				return fmt.Errorf("read source snapshot %s: %w", file.Path, err)
			}
			if !bytes.Equal(file.Data, current) {
				return fmt.Errorf("target source changed during campaign: %s", file.Path)
			}
		}
	}
	return nil
}

func (s *session) isIgnored(f Finding) bool {
	lines := s.ignored[f.File]
	if len(lines) == 0 {
		return false
	}
	_, ok := lines[f.Line]
	return ok
}

func (s *session) collect(reverts map[string][]target.Replacement) []target.Replacement {
	var replacements []target.Replacement
	s.anchors = make(map[string][]sourceAnchor)
	s.ignored = make(map[string]map[int]string)
	for _, pkg := range s.packages {
		hasSites := false
		for _, file := range pkg.Files {
			rel := s.rel(file.Path)
			s.anchors[rel] = sourceAnchors(file.Data, file.Owners)
			if len(file.Ignored) != 0 {
				s.ignored[rel] = file.Ignored
			}
			for i := range file.Edits {
				edit := &file.Edits[i]
				finding := Finding{File: rel, Line: edit.Line, Column: edit.Column,
					StartLine: edit.StartLine, EndLine: edit.EndLine,
					Expression: edit.Before, Mutation: edit.After, Rule: edit.Kind}
				s.identify(&finding)
				if !s.enabled(finding) {
					continue
				}
				s.sources = append(s.sources, sourceJob{
					source: file.Data, edit: edit, finding: finding, pkgPath: pkg.Path,
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
			rel := s.rel(entry.Path)
			if _, loaded := s.ignored[rel]; !loaded {
				if data, err := os.ReadFile(entry.Path); err == nil {
					fset := token.NewFileSet()
					if syntax, err := parser.ParseFile(fset, entry.Path, data, parser.ParseComments|parser.SkipObjectResolution); err == nil {
						if ig := target.ParseIgnoreComments(fset, syntax, data); len(ig) != 0 {
							s.ignored[rel] = ig
						}
					}
				}
			}
			mutation := entry.Description
			if mutation == "" {
				mutation = "restore previous Git hunk"
			}
			finding := Finding{File: rel, Line: entry.Line, Column: max(1, entry.Column),
				StartLine: entry.Line, EndLine: max(entry.Line, entry.EndLine),
				Rule: "revert", Mutation: mutation}
			s.identify(&finding)
			if s.enabled(finding) {
				s.sources = append(s.sources, sourceJob{replacement: entry, finding: finding, pkgPath: pkg.Path})
			}
		}
	}
	return replacements
}

func (s *session) prepare(ctx context.Context, base commandResult, report *Report) error {
	known := make(map[string]bool, len(s.testPackages))
	for i, pkg := range s.testPackages {
		known[pkg.Path] = true
		if base.packages[pkg.Path] == "skip" {
			continue
		}
		consumer := consumer{
			pkg: pkg.Path, dir: s.testDirectory(pkg),
			binary: filepath.Join(s.scratch, fmt.Sprintf("package-%d.test", i)),
		}
		s.consumers = append(s.consumers, consumer)
	}
	for pkg, disposition := range base.packages {
		if disposition == "pass" && !known[pkg] {
			return fmt.Errorf("ordinary baseline ran an undiscovered test package: %s", pkg)
		}
	}
	results := parallel(ctx, s.cfg.Jobs, len(s.consumers), func(i int) error {
		bin := &s.consumers[i]
		if err := s.buildTest(ctx, s.overlay, bin, 1, s.virtualDirs[bin.pkg] != ""); err != nil {
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
		if !sameInventory(base.inventory[bin.pkg], run.events) {
			return fmt.Errorf("instrumented baseline ran different test cases in %s", bin.pkg)
		}
		data, err := readTrace(trace, len(s.sites))
		if err != nil {
			return err
		}
		bin.tests = run.events
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
	return s.buildTest(ctx, overlay, bin, jobs, false)
}

func (s *session) buildTest(ctx context.Context, overlay string, bin *consumer, jobs int, skipVet bool) error {
	args := []string{"test", "-c", "-o", bin.binary, "-overlay=" + overlay, "-p=" + strconv.Itoa(jobs)}
	if skipVet {
		args = append(args, "-vet=off")
	}
	args = append(args, s.buildFlags()...)
	args = append(args, bin.pkg)
	build := runCommand(ctx, s.cfg.BuildTimeout, s.cfg.Dir, s.workerEnv(), "go", args...)
	if build.kind != "passed" {
		return fmt.Errorf("test build %s for %s:\n%s", build.kind, bin.pkg, build.detail)
	}
	if _, err := os.Stat(bin.binary); err != nil {
		return fmt.Errorf("missing test binary for %s: %w", bin.pkg, err)
	}
	return nil
}

func (s *session) runBinary(ctx context.Context, bin *consumer, trace, mutation, test string) commandResult {
	pattern := s.cfg.TestRun
	if test != "" {
		pattern = exactTestPattern(test)
	}
	return s.runBinaryPattern(ctx, bin, trace, mutation, pattern)
}

func (s *session) runBinaryPattern(ctx context.Context, bin *consumer, trace, mutation, pattern string) commandResult {
	return s.runBinaryOptions(ctx, bin, trace, mutation, pattern, mutation != "")
}

func (s *session) runBinaryOptions(ctx context.Context, bin *consumer, trace, mutation, pattern string, failfast bool) commandResult {
	args := []string{"-test.v=test2json", "-test.timeout=" + s.cfg.Timeout.String()}
	if failfast {
		args = append(args, "-test.failfast")
	}
	if pattern != "" {
		args = append(args, "-test.run="+pattern)
	}
	return runCommand(ctx, s.cfg.Timeout+2*time.Second, bin.dir, s.binaryWorkerEnv(trace, mutation), bin.binary, args...)
}

func (s *session) buildFlags() []string {
	var flags []string
	if s.cfg.Tags != "" {
		flags = append(flags, "-tags="+s.cfg.Tags)
	}
	return flags
}

func (s *session) goTest(ctx context.Context, overlay string, patterns ...string) commandResult {
	return s.goTestMode(ctx, overlay, false, patterns...)
}

func (s *session) goTestMutant(ctx context.Context, overlay string, patterns ...string) commandResult {
	return s.goTestMode(ctx, overlay, true, patterns...)
}

func (s *session) goTestMode(ctx context.Context, overlay string, failfast bool, patterns ...string) commandResult {
	if len(s.virtualDirs) != 0 {
		return s.goTestWithVirtualPackages(ctx, overlay, failfast)
	}
	return s.goTestCommand(ctx, overlay, failfast, patterns...)
}

func (s *session) goTestCommand(ctx context.Context, overlay string, failfast bool, patterns ...string) commandResult {
	args := []string{"test", "-json", "-count=1", "-timeout=" + s.cfg.Timeout.String(), "-p=" + strconv.Itoa(s.cfg.Jobs)}
	if failfast {
		args = append(args, "-failfast")
	}
	if s.cfg.TestRun != "" {
		args = append(args, "-run=" + s.cfg.TestRun)
	}
	if s.coverProfile != "" && s.coverPkg != "" {
		args = append(args, "-coverprofile="+s.coverProfile, "-coverpkg="+s.coverPkg)
	}
	if overlay != "" {
		// The ordinary baseline has already passed vet. A source mutation can
		// be executable Go even when a static quality check rejects it.
		args = append(args, "-vet=off", "-overlay="+overlay)
	}
	args = append(args, s.buildFlags()...)
	args = append(args, patterns...)
	return runCommand(ctx, s.cfg.BuildTimeout, s.cfg.Dir, s.workerEnv(), "go", args...)
}

func (s *session) reachedConsumers() [][]*consumer {
	consumers := make([][]*consumer, len(s.sites))
	for i := range s.consumers {
		bin := &s.consumers[i]
		for site := range bin.seen {
			consumers[site] = append(consumers[site], bin)
		}
	}
	return consumers
}

func (s *session) plan(report *Report, consumers [][]*consumer, allObserved *[]Finding) []probeJob {
	var jobs []probeJob
	for _, site := range s.sites {
		if site.Arith {
			continue
		}
		var seen analysis.Summary
		reached := consumers[site.ID]
		for _, bin := range reached {
			seen.Merge(bin.seen[site.ID])
		}
		if seen.Flags == 0 {
			f := Finding{Status: "unreached", File: s.rel(site.File), Line: site.Line, Column: site.Column,
				StartLine: site.StartLine, EndLine: site.EndLine, Expression: site.Expression}
			s.identify(&f)
			if s.enabled(f) && !s.isIgnored(f) {
				report.Unreached++
				if s.cfg.ShowUnreached {
					report.Findings = append(report.Findings, f)
				}
			}
			continue
		}
		if site.String {
			continue
		}
		for _, candidate := range analysis.PlanSite(site.Op, site.Domain, site.Margin, site.Float, seen, !s.cfg.Conventional) {
			if !s.comparisonEnabled(candidate.Kind) {
				continue
			}
			job := probeJob{site: site, candidate: candidate, predicted: !candidate.Infects(site.Op, seen)}
			job.finding = s.describe(job, seen)
			if !s.enabled(job.finding) {
				continue
			}
			if job.predicted && !s.cfg.VerifySurvivors && !s.cfg.Conventional {
				report.Skipped += len(reached)
				if allObserved != nil {
					*allObserved = append(*allObserved, job.finding)
				}
				s.recordFinding(report, job.finding, false)
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
	f := Finding{File: s.rel(site.File), Line: site.Line, Column: site.Column,
		StartLine: site.StartLine, EndLine: site.EndLine, Expression: site.Expression, Rule: c.Kind}
	switch {
	case c.Kind == "margin" && c.Shift < 0:
		f.Mutation = fmt.Sprintf("%s %s %s - 1 (without overflow)", site.Left, c.Op, site.Right)
	case c.Kind == "margin" && c.Shift > 0:
		f.Mutation = fmt.Sprintf("%s %s %s + 1 (without overflow)", site.Left, c.Op, site.Right)
	case c.Shift != 0 && site.Domain != nil:
		f.Mutation = fmt.Sprintf("%s %s %s", site.Left, c.Op, rankString(*site.Domain, c.Pivot))
	default:
		f.Mutation = fmt.Sprintf("%s %s %s", site.Left, c.Op, site.Right)
	}
	s.identify(&f)
	if job.predicted {
		f.Status, f.Evidence = "input-gap", "observed"
		switch {
		case c.Kind == "margin":
			sign := "- 1"
			if c.Shift > 0 {
				sign = "+ 1"
			}
			f.Detail = fmt.Sprintf("distinguishing margin: %s = %s %s", site.Left, site.Right, sign)
		case site.Float && c.Kind == "boundary":
			f.Detail = fmt.Sprintf("operands %s and %s were never equal", site.Left, site.Right)
		case site.Domain != nil && c.Witness != nil:
			w := *c.Witness
			input := rankString(*site.Domain, w)
			f.Witness = &Witness{Input: input, Operand: site.Left,
				Original: analysis.Evaluate(site.Op, w, site.Domain.Pivot), Mutant: analysis.Evaluate(c.Op, w, c.Pivot)}
			f.Witness.Call = witnessCall(site, input)
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
	if job.site.Arith {
		mode = fmt.Sprintf("%d:%d:0:0", job.site.ID, job.arithOp)
	}
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
			result.activatedChanged = flags&24 == 24
			result.deadlineExceeded = run.deadlineExceeded || (run.kind == "timeout" && run.elapsed+s.cfg.Timeout/10 >= s.cfg.Timeout)
			f.Status, f.Detail = run.kind, run.detail
			return result
		}
		if job.site.Arith {
			if flags&8 == 0 || (run.kind == "failed" && flags&16 == 0) {
				f.Status, f.Detail = "unstable", "replay disagreed with baseline observations"
				return result
			}
		} else if flags&8 == 0 || (flags&16 != 0) == job.predicted ||
			run.kind == "failed" && job.predicted {
			f.Status, f.Detail = "unstable", "replay disagreed with baseline observations"
			return result
		}
		if run.kind == "passed" && !sameInventory(bin.tests, run.events) {
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
	mask := candidate.Op.Mask()
	if site.Float {
		mask = candidate.Op.FloatMask()
	}
	mode := "false"
	switch {
	case candidate.Kind == "margin" && candidate.Shift < 0:
		mode = "margin-down"
	case candidate.Kind == "margin" && candidate.Shift > 0:
		mode = "margin-up"
	case candidate.Shift != 0:
		mode = "true"
	}
	return fmt.Sprintf("%d:%d:%d:%s", site.ID, mask, candidate.Pivot, mode)
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

func (s *session) recordFinding(r *Report, f Finding, timedOutKilled bool) {
	if f.Status == "" {
		return
	}
	if isGapStatus(f.Status) && s.isIgnored(f) {
		r.Suppressed++
		return
	}
	if (timedOutKilled || f.Detail == "killed by timeout") && f.Status == "killed" {
		r.TimedOut++
	}
	r.add(f, s.cfg.KilledFindings)
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
	case "input-gap", "assertion-gap", "source-survivor", "untested-change", "weakened-test":
	case "non-viable":
		r.Rejected++
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
