package campaign

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

type witnessJob struct {
	index int
	test  string
	code  string
	probe probeJob
}

func (s *session) verifySourceWitnesses(ctx context.Context, report *Report) {
	if len(s.sources) == 0 {
		return
	}
	jobs := make(map[string]sourceJob, len(s.sources))
	for _, job := range s.sources {
		if job.edit != nil {
			jobs[job.finding.ID] = job
		}
	}
	var candidates []int
	for i, f := range report.Findings {
		if _, present := jobs[f.ID]; present && f.Status == "source-survivor" {
			candidates = append(candidates, i)
		}
	}
	slices.SortStableFunc(candidates, func(a, b int) int {
		aRule, bRule := report.Findings[a].Rule, report.Findings[b].Rule
		if order := sourceWitnessPriority(aRule) - sourceWitnessPriority(bRule); order != 0 {
			return order
		}
		return compareFindings(report.Findings[a], report.Findings[b])
	})
	candidates = candidates[:min(8, len(candidates))]
	type result struct {
		witness *SourceWitness
		err     error
	}
	results := parallel(ctx, min(2, s.cfg.Jobs), len(candidates), func(i int) result {
		witness, err := s.sourceWitness(ctx, jobs[report.Findings[candidates[i]].ID])
		return result{witness, err}
	})
	for i, tested := range results {
		f := &report.Findings[candidates[i]]
		if tested.err != nil {
			f.Detail = "source witness verification: " + tested.err.Error()
			report.Incomplete++
		} else {
			f.SourceWitness = tested.witness
		}
	}
}

func sourceWitnessPriority(rule string) int {
	switch rule {
	case "min-max", "rounding-mode", "affix-contains", "affix-end", "replacement-count", "search-direction", "slice-element-remove", "integer-rounding-order":
		return 0
	}
	return 1
}

func (s *session) verifyWitnesses(ctx context.Context, report *Report, probes []probeJob) {
	byID := make(map[string]probeJob)
	for _, probe := range probes {
		f := probe.finding
		byID[f.ID] = probe
	}
	groups := make(map[string][]witnessJob)
	for i, finding := range report.Findings {
		want := finding.Status == "input-gap" && s.cfg.VerifyWitnesses || finding.Status == "assertion-gap" && s.cfg.SuggestAssertions
		if !want || finding.Witness == nil || finding.Witness.Call == "" {
			continue
		}
		probe, ok := byID[finding.ID]
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

func witnessCode(w *Witness) string {
	return witnessAssertionCode(w, "t", "got", false)
}

func witnessAssertionCode(w *Witness, param, gotVar string, shadowedBool bool) string {
	expected := fmt.Sprintf("%t", w.Original)
	if shadowedBool {
		expected = "(0 != 0)"
		if w.Original {
			expected = "(0 == 0)"
		}
	}
	return fmt.Sprintf("if %s := %s; %s != %s {\n\t%s.Fatalf(\"got %%v, want %t\", %s)\n}",
		gotVar, w.Call, gotVar, expected, param, w.Original, gotVar)
}

func witnessUsesIdent(expr, ident string) bool {
	parsed, err := parser.ParseExpr(expr)
	if err != nil {
		return false
	}
	found := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && id.Name == ident {
			found = true
			return false
		}
		return true
	})
	return found
}

func witnessParamAndCode(job witnessJob, w *Witness, prefix string, idents shadowedIdents) (string, string) {
	param := "t"
	if idents.hasT || job.probe.site.Callable == "t" || witnessUsesIdent(w.Call, "t") {
		param = "tmtT"
		if job.probe.site.Callable == param || witnessUsesIdent(w.Call, param) {
			param = "tmt" + prefix + "T"
		}
	}
	gotVar := "got"
	if idents.hasGot || job.probe.site.Callable == "got" || witnessUsesIdent(w.Call, "got") {
		gotVar = "tmtGot"
		if job.probe.site.Callable == gotVar || witnessUsesIdent(w.Call, gotVar) {
			gotVar = "tmt" + prefix + "Got"
		}
	}
	return param, witnessAssertionCode(w, param, gotVar, idents.hasBool)
}

func (s *session) verifyWitnessGroup(ctx context.Context, report *Report, pkg target.Package, jobs []witnessJob) error {
	dir, err := os.MkdirTemp(s.scratch, "witness-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(pkg.Dir, strings.ToLower(pkg.Prefix)+"_witness_test.go")
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return fmt.Errorf("synthetic witness path is occupied: %s", path)
	}
	var extra [][]byte
	for _, file := range pkg.Files {
		extra = append(extra, file.Data)
	}
	idents := packageShadowedIdents(pkg.Dir, extra...)
	var source strings.Builder
	fmt.Fprintf(&source, "package %s\nimport tmtWitnessTesting \"testing\"\n", pkg.Name)
	for i := range jobs {
		job := &jobs[i]
		job.test = fmt.Sprintf("TestTmt%sWitness%d", pkg.Prefix, i)
		w := report.Findings[job.index].Witness
		param, code := witnessParamAndCode(*job, w, pkg.Prefix, idents)
		job.code = code
		fmt.Fprintf(&source, "func %s(%s *tmtWitnessTesting.T) {\n%s\n}\n", job.test, param, job.code)
	}
	files := append(slices.Clone(s.replacements), target.Replacement{Path: path, Data: []byte(source.String())})
	overlay, err := makeOverlay(dir, files)
	if err != nil {
		return err
	}
	bin := consumer{pkg: pkg.Path, dir: pkg.Dir, binary: filepath.Join(dir, "witness.test")}
	if err := s.build(ctx, overlay, &bin, s.cfg.Jobs); err != nil {
		if len(jobs) == 1 {
			return err
		}
		for _, job := range jobs {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if singleErr := s.verifyWitnessSingle(ctx, report, pkg, path, dir, idents, job); singleErr != nil {
				report.Findings[job.index].Detail = "witness verification: " + singleErr.Error()
				report.Incomplete++
			}
		}
		return nil
	}
	for _, job := range jobs {
		if err := s.runWitnessVerification(ctx, report, dir, &bin, job); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) verifyWitnessSingle(ctx context.Context, report *Report, pkg target.Package, path, rootDir string, idents shadowedIdents, job witnessJob) error {
	dir, err := os.MkdirTemp(rootDir, "single-")
	if err != nil {
		return err
	}
	w := report.Findings[job.index].Witness
	param, code := witnessParamAndCode(job, w, pkg.Prefix, idents)
	job.code = code
	source := fmt.Sprintf("package %s\nimport tmtWitnessTesting \"testing\"\nfunc %s(%s *tmtWitnessTesting.T) {\n%s\n}\n",
		pkg.Name, job.test, param, job.code)
	files := append(slices.Clone(s.replacements), target.Replacement{Path: path, Data: []byte(source)})
	overlay, err := makeOverlay(dir, files)
	if err != nil {
		return err
	}
	bin := consumer{pkg: pkg.Path, dir: pkg.Dir, binary: filepath.Join(dir, "witness.test")}
	if err := s.build(ctx, overlay, &bin, 1); err != nil {
		return err
	}
	return s.runWitnessVerification(ctx, report, dir, &bin, job)
}

func (s *session) runWitnessVerification(ctx context.Context, report *Report, dir string, bin *consumer, job witnessJob) error {
	// An original failure or absent witness stays explicitly unverified.
	base := s.runBinary(ctx, bin, "", "", job.test)
	if base.kind != "passed" || !slices.Contains(base.tests, job.test) {
		report.Findings[job.index].Detail = "generated witness did not pass on the original"
		report.Incomplete++
		return nil
	}
	trace, err := newTrace(dir, len(s.sites))
	if err != nil {
		return err
	}
	mutated := s.runBinary(ctx, bin, trace, selector(job.probe.site, job.probe.candidate), job.test)
	flags, err := readFlags(trace, job.probe.site.ID)
	if err != nil {
		return err
	}
	f := &report.Findings[job.index]
	if mutated.kind == "failed" && flags&24 == 24 && slices.Contains(mutated.failed, job.test) {
		f.Witness.Verified = true
		f.Witness.Code = job.code
		if targetPkg, ok := s.externalWitnessTarget(job.probe.site.File); ok && ast.IsExported(job.probe.site.Callable) {
			if srcPkg, found := s.packageForFile(job.probe.site.File); found && srcPkg.Path != targetPkg.Path {
				qualifiedCall := srcPkg.Name + "." + f.Witness.Call
				f.Witness.Call = qualifiedCall
				f.Witness.Code = witnessAssertionCode(f.Witness, "t", "got", false)
			}
		}
	} else {
		f.Detail = "generated witness did not fail on the applied mutant"
		report.Incomplete++
	}
	return nil
}

func (s *session) packageForFile(file string) (target.Package, bool) {
	for _, pkg := range s.packages {
		if slices.Contains(pkg.Paths, file) || filepath.Dir(file) == pkg.Dir {
			return pkg, true
		}
	}
	return target.Package{}, false
}

// externalWitnessTarget returns the external test package to target for witness
// code formatting, either explicitly via --witness-package or automatically
// when the source package has no tests of its own and all running test binaries
// belong to external packages.
func (s *session) externalWitnessTarget(sourceFile string) (target.Package, bool) {
	srcPkg, ok := s.packageForFile(sourceFile)
	if !ok {
		return target.Package{}, false
	}
	if s.cfg.WitnessPackage != "" {
		for _, pkg := range s.testPackages {
			if pkg.Path == s.cfg.WitnessPackage || pkg.Name == s.cfg.WitnessPackage || s.rel(pkg.Dir) == filepath.ToSlash(strings.TrimPrefix(s.cfg.WitnessPackage, "./")) {
				return pkg, pkg.Path != srcPkg.Path
			}
		}
		return target.Package{}, false
	}
	if len(s.consumers) == 0 {
		return target.Package{}, false
	}
	for _, bin := range s.consumers {
		if bin.pkg == srcPkg.Path {
			return target.Package{}, false
		}
	}
	firstExternal := s.consumers[0].pkg
	for _, pkg := range s.testPackages {
		if pkg.Path == firstExternal {
			return pkg, pkg.Path != srcPkg.Path
		}
	}
	return target.Package{}, false
}
