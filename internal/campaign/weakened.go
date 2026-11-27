package campaign

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

func previousTestSelection(ctx context.Context, cfg Config) ([]target.Replacement, []target.Package, error) {
	testFiles, err := previousTestFiles(ctx, cfg.Dir, cfg.Since)
	if err != nil {
		return nil, nil, err
	}
	testdataFiles, err := previousTestdataFiles(ctx, cfg.Dir, cfg.Since)
	if err != nil {
		return nil, nil, err
	}
	if len(testFiles) == 0 && len(testdataFiles) == 0 {
		return nil, nil, nil
	}
	cfg = selectedTestConfig(cfg)
	packages, err := resolveTestPackages(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	dirs := make(map[string]bool, len(packages))
	for _, pkg := range packages {
		dirs[pkg.Dir] = true
	}
	if slices.ContainsFunc(testFiles, func(f target.Replacement) bool { return f.Data != nil && !dirs[filepath.Dir(f.Path)] }) {
		dir, err := os.MkdirTemp("", "tmt-previous-test-selection-")
		if err != nil {
			return nil, nil, err
		}
		defer os.RemoveAll(dir)
		overlay, err := makeOverlay(dir, slices.Concat(testFiles, testdataFiles))
		if err != nil {
			return nil, nil, err
		}
		prior, err := resolveTestPackages(ctx, cfg, overlay)
		if err != nil {
			// The old overlay will receive an explicit unconfirmed baseline verdict.
			return slices.Concat(testFiles, testdataFiles), packages, nil
		}
		for _, pkg := range prior {
			dirs[pkg.Dir] = true
		}
	}
	testFiles = slices.DeleteFunc(testFiles, func(f target.Replacement) bool { return !dirs[filepath.Dir(f.Path)] })
	testdataFiles = slices.DeleteFunc(testdataFiles, func(f target.Replacement) bool {
		owner, ok := testdataOwnerDir(f.Path)
		return !ok || !dirs[owner]
	})
	return slices.Concat(testFiles, testdataFiles), packages, nil
}

func selectedTestConfig(cfg Config) Config {
	if len(cfg.TestPatterns) == 0 {
		cfg.TestPatterns = cfg.Patterns
	}
	return cfg
}

type previousCandidate struct {
	finding Finding
	probe   *probeJob
	source  *sourceJob
}

func (s *session) findingInScope(changed target.Scope, f Finding) bool {
	if f.Rule == "revert" {
		return true
	}
	start, end := f.spanLines()
	return changed.Contains(filepath.Join(s.cfg.Dir, filepath.FromSlash(f.File)), start, end)
}

func (s *session) comparePrevious(ctx context.Context, report *Report, changed target.Scope, files []target.Replacement, reached [][]*consumer, allObserved []Finding) error {
	candidates := s.previousCandidates(allObserved, changed, reached)
	s.resetReportToScope(report, changed, reached, allObserved)
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
	setup, cancel := context.WithTimeout(ctx, s.cfg.BuildTimeout)
	prior.testPackages, err = resolveTestPackages(setup, selectedTestConfig(s.cfg), baselineOverlay)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.uncheckedPrevious(report, files[0].Path, "could not load previous test packages: "+err.Error())
		return nil
	}
	if err := prior.setVirtualDirs(ctx, files); err != nil {
		return err
	}
	base := prior.goTest(ctx, baselineOverlay, s.testPatterns()...)
	report.PreviousRuns++
	if base.kind != "passed" {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.uncheckedPrevious(report, files[0].Path, summarizePreviousTestFailure(base.kind, base.detail))
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
			s.recordFinding(report, f, false)
		} else if previousUnconfirmed(result.finding.Status) {
			f := candidates[i].finding
			f.Status, f.Evidence = "not-evaluated", "previous-tests"
			f.Detail = fmt.Sprintf("previous test comparison %s: %s", result.finding.Status, result.finding.Detail)
			s.recordFinding(report, f, false)
		}
	}
	return nil
}

func (s *session) resetReportToScope(report *Report, changed target.Scope, reached [][]*consumer, allObserved []Finding) {
	report.Sites = 0
	report.Killed = 0
	report.TimedOut = 0
	report.Unreached = 0
	report.Rejected = 0
	report.Incomplete = 0
	report.Suppressed = 0
	report.Findings = report.Findings[:0]
	for _, site := range s.sites {
		if site.IsSchemata() {
			continue
		}
		if !changed.Contains(site.File, site.StartLine, site.EndLine) {
			continue
		}
		report.Sites++
		if len(reached[site.ID]) == 0 {
			f := Finding{Status: "unreached", File: s.rel(site.File), Line: site.Line, Column: site.Column,
				StartLine: site.StartLine, EndLine: site.EndLine, Expression: site.Expression}
			s.identify(&f)
			if s.enabled(f) && !s.isIgnored(f) {
				report.Unreached++
				if s.cfg.ShowUnreached {
					report.Findings = append(report.Findings, f)
				}
			}
		}
	}
	for _, f := range allObserved {
		if isIncompleteVerdict(f.Status) || s.findingInScope(changed, f) {
			s.recordFinding(report, f, f.Detail == "killed by timeout")
		}
	}
}

func isIncompleteVerdict(status string) bool {
	switch status {
	case "", "killed", "non-viable", "unreached":
		return false
	}
	return !isGapStatus(status)
}

// Go overlays can expose a package whose directory was deleted, and tests may
// open files under testdata/ relative to the package directory. Give deleted
// packages and packages with changed testdata/ a private physical execution
// directory populated with the base revision's Git-tracked testdata/.
func (s *session) setVirtualDirs(ctx context.Context, files []target.Replacement) error {
	changedTestdata := make(map[string]bool)
	for _, f := range files {
		if owner, ok := testdataOwnerDir(f.Path); ok {
			changedTestdata[owner] = true
		}
	}
	for _, pkg := range s.testPackages {
		missingDir := false
		if _, err := os.Stat(pkg.Dir); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			missingDir = true
		}
		if !missingDir && !changedTestdata[pkg.Dir] {
			continue
		}
		dir, err := os.MkdirTemp(s.scratch, "virtual-test-package-")
		if err != nil {
			return err
		}
		if s.cfg.Since != "" {
			if _, err := restorePreviousTestdata(ctx, s.cfg.Dir, s.cfg.Since, pkg.Dir, dir); err != nil {
				return err
			}
		}
		if s.virtualDirs == nil {
			s.virtualDirs = make(map[string]string)
		}
		s.virtualDirs[pkg.Path] = dir
	}
	return nil
}

func summarizePreviousTestFailure(kind, detail string) string {
	header := fmt.Sprintf("previous tests did not pass on the current source (%s)", kind)
	if strings.TrimSpace(detail) == "" {
		return header
	}
	type testEvent struct {
		Action  string `json:"Action"`
		Package string `json:"Package"`
		Test    string `json:"Test"`
		Output  string `json:"Output"`
	}
	var failedTests []string
	outputsByTest := make(map[string][]string)
	var pkgOutputs []string
	parsedAny := false
	for line := range strings.SplitSeq(detail, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev testEvent
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Action == "" {
			continue
		}
		parsedAny = true
		switch ev.Action {
		case "fail":
			if ev.Test != "" && !slices.Contains(failedTests, ev.Test) {
				failedTests = append(failedTests, ev.Test)
			}
		case "output":
			msg := strings.TrimSpace(ev.Output)
			if msg == "" || strings.HasPrefix(msg, "=== RUN") || strings.HasPrefix(msg, "=== PAUSE") ||
				strings.HasPrefix(msg, "=== CONT") || strings.HasPrefix(msg, "--- PASS") ||
				strings.HasPrefix(msg, "--- FAIL") || strings.HasPrefix(msg, "--- SKIP") ||
				msg == "FAIL" || msg == "PASS" || strings.HasPrefix(msg, "FAIL\t") ||
				strings.HasPrefix(msg, "ok\t") || strings.HasPrefix(msg, "?\t") {
				continue
			}
			if ev.Test != "" {
				outputsByTest[ev.Test] = append(outputsByTest[ev.Test], msg)
			} else {
				pkgOutputs = append(pkgOutputs, msg)
			}
		}
	}
	if !parsedAny {
		return header + "\n" + strings.TrimSpace(detail)
	}
	var b strings.Builder
	b.WriteString(header)
	missingFixture := false
	if len(failedTests) > 0 {
		fmt.Fprintf(&b, "\nfailing tests: %s", strings.Join(failedTests, ", "))
		shown := 0
		for _, name := range failedTests {
			for _, msg := range outputsByTest[name] {
				if shown < 8 {
					fmt.Fprintf(&b, "\n%s: %s", name, msg)
					shown++
				}
				if looksLikeMissingFile(msg) {
					missingFixture = true
				}
			}
		}
	} else if len(pkgOutputs) > 0 {
		for _, msg := range pkgOutputs[:min(8, len(pkgOutputs))] {
			fmt.Fprintf(&b, "\n%s", msg)
			if looksLikeMissingFile(msg) {
				missingFixture = true
			}
		}
	}
	if missingFixture {
		b.WriteString("\nhint: previous tests could not open a required file; Git-tracked files under package testdata/ are restored automatically, while untracked or outside-package fixtures must exist on disk")
	}
	return b.String()
}

func looksLikeMissingFile(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "no such file or directory") ||
		strings.Contains(lower, "cannot find the file") ||
		strings.Contains(lower, "cannot find the path")
}

func (s *session) testDirectory(pkg target.Package) string {
	if dir := s.virtualDirs[pkg.Path]; dir != "" {
		return dir
	}
	return pkg.Dir
}

func (s *session) goTestWithVirtualPackages(ctx context.Context, overlay string, failfast bool) commandResult {
	var physical, virtual []target.Package
	for _, pkg := range s.testPackages {
		if s.virtualDirs[pkg.Path] == "" {
			physical = append(physical, pkg)
		} else {
			virtual = append(virtual, pkg)
		}
	}
	result := commandResult{kind: "passed", inventory: make(map[string][]string), packages: make(map[string]string)}
	if len(physical) != 0 {
		patterns := make([]string, len(physical))
		for i, pkg := range physical {
			patterns[i] = pkg.Path
		}
		result = s.goTestCommand(ctx, overlay, failfast, patterns...)
		if result.kind != "passed" && result.kind != "failed" {
			return result
		}
		if failfast && result.kind == "failed" {
			return result
		}
		if result.inventory == nil {
			result.inventory = make(map[string][]string)
		}
		if result.packages == nil {
			result.packages = make(map[string]string)
		}
	}
	dir, err := os.MkdirTemp(s.scratch, "virtual-test-build-")
	if err != nil {
		return commandResult{kind: "error", detail: err.Error()}
	}
	defer os.RemoveAll(dir)
	for i, pkg := range virtual {
		bin := consumer{pkg: pkg.Path, dir: s.testDirectory(pkg), binary: filepath.Join(dir, fmt.Sprintf("test-%d%s", i, exeSuffix()))}
		if err := s.buildTest(ctx, overlay, &bin, 1, true); err != nil {
			return commandResult{kind: "error", detail: err.Error()}
		}
		run := s.runBinaryOptions(ctx, &bin, "", "", s.cfg.TestRun, failfast)
		switch run.kind {
		case "passed":
			result.packages[pkg.Path] = "pass"
			result.hasTestElapsed = true
			if run.elapsed > result.maxTestElapsed {
				result.maxTestElapsed = run.elapsed
			}
		case "failed":
			result.kind, result.packages[pkg.Path] = "failed", "fail"
			result.failed = append(result.failed, run.failed...)
			if failfast {
				return result
			}
		default:
			return run
		}
		if len(run.events) != 0 {
			result.inventory[pkg.Path] = run.events
		}
	}
	return result
}

func previousUnconfirmed(status string) bool {
	switch status {
	case "", "source-survivor", "non-viable", "assertion-gap", "input-gap":
		return false
	}
	return true
}

func (s *session) uncheckedPrevious(report *Report, file, detail string) {
	f := Finding{Status: "not-evaluated", Evidence: "previous-tests", File: s.rel(file), Line: 1, Column: 1,
		StartLine: 1, EndLine: 1, Rule: "weakened-test", Detail: detail}
	s.identify(&f)
	s.recordFinding(report, f, false)
}

func (s *session) previousCandidates(allObserved []Finding, changed target.Scope, reached [][]*consumer) []previousCandidate {
	survivors := make(map[string]Finding)
	for _, f := range allObserved {
		switch f.Status {
		case "input-gap", "assertion-gap", "source-survivor":
			if !s.findingInScope(changed, f) && !s.isIgnored(f) {
				survivors[f.ID] = f
			}
		}
	}
	var result []previousCandidate
	for _, site := range s.sites {
		if site.String || site.IsSchemata() || changed.Contains(site.File, site.StartLine, site.EndLine) {
			continue
		}
		for _, candidate := range analysis.PreviewSite(site.Op, site.Domain, site.Margin, site.Float, !s.cfg.Conventional) {
			if !s.comparisonEnabled(candidate.Kind) {
				continue
			}
			job := probeJob{site: site, candidate: candidate}
			job.finding = s.describe(job, analysis.Summary{})
			if s.isIgnored(job.finding) {
				continue
			}
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
