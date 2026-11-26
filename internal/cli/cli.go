// Package cli implements the user-facing command and report formats.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/t-k/targeted-mutation-go/internal/campaign"
	"github.com/t-k/targeted-mutation-go/internal/reporting"
)

type sanitizedWriter struct {
	dst io.Writer
}

func (w sanitizedWriter) Write(p []byte) (int, error) {
	_, err := io.WriteString(w.dst, SanitizeBlock(string(p)))
	return len(p), err
}

func Execute(ctx context.Context, args []string, out, errOut io.Writer) int {
	safeErr := sanitizedWriter{dst: errOut}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		usage(out)
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintln(out, SanitizeInline(version()))
		return 0
	}
	if args[0] != "analyze" {
		fmt.Fprintf(safeErr, "unknown command %q\n", args[0])
		usage(safeErr)
		return 1
	}
	options := flag.NewFlagSet("analyze", flag.ContinueOnError)
	options.SetOutput(safeErr)
	options.Usage = func() {
		usage(safeErr)
		options.PrintDefaults()
	}
	var cfg campaign.Config
	cfg.Warn = safeErr
	options.StringVar(&cfg.Dir, "dir", ".", "project or workspace directory")
	options.StringVar(&cfg.Since, "since", "", "restrict sites to working-tree lines changed from this Git revision")
	options.Func("lines", "restrict mutations to FILE, FILE:LINE, or FILE:FROM-TO (repeatable)", func(value string) error {
		cfg.Lines = append(cfg.Lines, value)
		return nil
	})
	options.Func("test-package", "select independent test package patterns (repeatable)", func(value string) error {
		cfg.TestPatterns = append(cfg.TestPatterns, value)
		return nil
	})
	options.StringVar(&cfg.TestRun, "test", "", "select tests with a Go -run expression")
	options.StringVar(&cfg.Comparison, "comparison", "all", "comparison rules (all, boundary, or negation)")
	options.StringVar(&cfg.Tags, "tags", "", "comma-separated Go build tags")
	options.IntVar(&cfg.Jobs, "jobs", min(4, runtime.GOMAXPROCS(0)), "maximum concurrent build or mutation workers")
	options.DurationVar(&cfg.Timeout, "timeout", 0, "timeout per test package process (default: adaptive 5s + 5x test runtime)")
	options.StringVar(&cfg.TimeoutRecheck, "timeout-recheck", "serial", "recheck parallel timeouts serially (serial or none)")
	options.DurationVar(&cfg.BuildTimeout, "build-timeout", 5*time.Minute, "timeout per Go build or Go test command")
	options.BoolVar(&cfg.Full, "full", false, "also run the source mutation catalogue")
	options.BoolVar(&cfg.SourceOnly, "source-only", false, "run the full catalogue as source mutations")
	options.BoolVar(&cfg.IncludeGenerated, "include-generated", false, "also discover mutations in generated Go sources")
	options.BoolVar(&cfg.Conventional, "conventional", false, "run boundary and negation mutations without threshold synthesis")
	options.BoolVar(&cfg.VerifySurvivors, "verify-survivors", false, "replay comparison mutants whose observations predict survival")
	options.BoolVar(&cfg.VerifyWitnesses, "verify-witnesses", false, "compile and run synthesized witness tests for direct calls")
	options.BoolVar(&cfg.SuggestAssertions, "suggest-assertions", false, "find affected tests and verify concrete assertion suggestions")
	options.BoolVar(&cfg.ShowUnreached, "show-unreached", false, "include comparison sites the selected tests did not reach")
	options.BoolVar(&cfg.UndoChanges, "undo-changes", false, "run tests with each edited Go hunk restored to --since")
	options.BoolVar(&cfg.KilledFindings, "show-killed", false, "include killed mutants in the report")
	showNonViable := options.Bool("show-non-viable", false, "include non-viable candidates in text output")
	options.BoolVar(&cfg.List, "list", false, "preview mutation candidates without running tests")
	options.StringVar(&cfg.Shard, "shard", "", "select the zero-based shard K/N")
	options.StringVar(&cfg.Checkpoint, "checkpoint", "", "resume completed mutations from a campaign journal")
	options.IntVar(&cfg.Audit, "audit", 0, "compare up to N completed probe mutations with the actual source edits")
	options.Func("exclude-rule", "skip a mutation rule (repeatable)", func(value string) error {
		cfg.ExcludeRules = append(cfg.ExcludeRules, value)
		return nil
	})
	format := options.String("format", "text", "report format (text, json, jsonl, or sarif)")
	baselinePath := options.String("baseline", "", "suppress known findings from this baseline file")
	writeBaselinePath := options.String("write-baseline", "", "save findings from a complete campaign as a baseline")
	reordered, reorderErr := reorderAnalyzeArgs(options, args[1:])
	if reorderErr != nil {
		fmt.Fprintln(safeErr, reorderErr)
		options.Usage()
		return 1
	}
	if err := options.Parse(reordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	timeoutSet := false
	options.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" {
			timeoutSet = true
		}
	})
	cfg.TimeoutExplicit = timeoutSet
	if *format != "text" && *format != "json" && *format != "jsonl" && *format != "sarif" {
		fmt.Fprintf(safeErr, "invalid report format %q\n", *format)
		return 1
	}
	if cfg.Jobs < 1 || (timeoutSet && cfg.Timeout <= 0) || cfg.BuildTimeout <= 0 {
		fmt.Fprintln(safeErr, "--jobs, --timeout, and --build-timeout must be positive")
		return 1
	}
	if cfg.TimeoutRecheck != "serial" && cfg.TimeoutRecheck != "none" {
		fmt.Fprintf(safeErr, "invalid --timeout-recheck %q: expected serial or none\n", cfg.TimeoutRecheck)
		return 1
	}
	if cfg.Since == "" && cfg.UndoChanges {
		fmt.Fprintln(safeErr, "--undo-changes requires --since")
		return 1
	}
	if cfg.Conventional && cfg.VerifyWitnesses {
		fmt.Fprintln(safeErr, "--verify-witnesses is incompatible with --conventional")
		return 1
	}
	if cfg.List && (*baselinePath != "" || *writeBaselinePath != "") {
		fmt.Fprintln(safeErr, "--baseline and --write-baseline require an evaluated campaign")
		return 1
	}
	cfg.Patterns = options.Args()
	for _, pattern := range append(append([]string(nil), cfg.Patterns...), cfg.TestPatterns...) {
		if strings.HasSuffix(pattern, ".go") {
			fmt.Fprintf(safeErr, "expected a Go package pattern, got a file: %q\n", pattern)
			return 1
		}
		if strings.HasPrefix(pattern, "-") {
			fmt.Fprintf(safeErr, "flags must precede package patterns: %q\n", pattern)
			return 1
		}
	}
	var baseline reporting.Baseline
	var err error
	if *baselinePath != "" {
		baseline, err = reporting.ReadBaseline(*baselinePath)
		if err != nil {
			fmt.Fprintln(safeErr, err)
			return 1
		}
	}
	report, err := campaign.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(safeErr, err)
		return 1
	}
	if *writeBaselinePath != "" {
		if report.Incomplete != 0 {
			fmt.Fprintln(safeErr, "cannot write a baseline from an incomplete campaign")
		} else if err := reporting.WriteBaseline(*writeBaselinePath, report, findingID); err != nil {
			fmt.Fprintln(safeErr, err)
			return 1
		}
	}
	if *baselinePath != "" {
		filtered := report.Findings[:0]
		for _, finding := range report.Findings {
			if baseline.Suppresses(finding, findingID) {
				report.Suppressed++
			} else {
				filtered = append(filtered, finding)
			}
		}
		report.Findings = filtered
		report.Reconcile()
	}
	switch *format {
	case "json":
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err = encoder.Encode(report); err == nil {
			_, err = out.Write(buf.Bytes())
		}
	case "jsonl":
		err = reporting.WriteJSONL(out, report, findingID)
	case "sarif":
		projectDir := cfg.Dir
		if absDir, absErr := filepath.Abs(projectDir); absErr == nil {
			projectDir = absDir
		}
		if evalDir, evalErr := filepath.EvalSymlinks(projectDir); evalErr == nil {
			projectDir = evalDir
		}
		root, _ := exec.CommandContext(ctx, "git", "-C", projectDir, "rev-parse", "--show-toplevel").Output()
		repoDir := strings.TrimSpace(string(root))
		if repoDir != "" {
			if evalRepo, evalErr := filepath.EvalSymlinks(repoDir); evalErr == nil {
				repoDir = evalRepo
			}
		}
		err = reporting.WriteSARIF(out, report, findingID, reporting.SARIFPaths{
			ProjectDir:    projectDir,
			RepositoryDir: repoDir,
			WorkspaceDirs: report.WorkspaceDirs,
		})
	default:
		err = writeText(out, report, *showNonViable)
	}
	if err != nil {
		fmt.Fprintln(safeErr, err)
		return 1
	}
	if report.Incomplete != 0 {
		return 3
	}
	for _, finding := range report.Findings {
		if reporting.IsGap(finding.Status) {
			return 2
		}
	}
	return 0
}

func findingID(f campaign.Finding) string { return f.ID }

func usage(out io.Writer) {
	fmt.Fprintln(out, "Usage: targeted-mutation-go analyze [flags] [packages...]")
	fmt.Fprintln(out, "       targeted-mutation-go version")
	fmt.Fprintln(out, "Package patterns default to ./...; pass analyze --help for all options.")
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return "targeted-mutation-go " + info.Main.Version
	}
	return "targeted-mutation-go (development build)"
}

func writeText(out io.Writer, report campaign.Report, showNonViable bool) error {
	var text strings.Builder
	for _, finding := range report.Findings {
		if finding.Status == "non-viable" && !showNonViable {
			continue
		}
		label := strings.ToUpper(strings.ReplaceAll(SanitizeInline(finding.Status), "-", " "))
		fmt.Fprintf(&text, "%s %s:%d:%d\n", label, SanitizeInline(finding.File), finding.Line, finding.Column)
		if finding.Expression != "" {
			fmt.Fprintf(&text, "  expression: %s\n", SanitizeInline(finding.Expression))
		}
		if finding.Mutation != "" {
			if finding.Evidence != "" {
				fmt.Fprintf(&text, "  mutation: %s [%s]\n", SanitizeInline(finding.Mutation), SanitizeInline(finding.Evidence))
			} else {
				fmt.Fprintf(&text, "  mutation: %s\n", SanitizeInline(finding.Mutation))
			}
		}
		if len(finding.Interval) == 2 {
			fmt.Fprintf(&text, "  observed-equivalent thresholds: %s..%s\n", SanitizeInline(finding.Interval[0]), SanitizeInline(finding.Interval[1]))
		}
		if witness := finding.Witness; witness != nil {
			if witness.Call != "" {
				fmt.Fprintf(&text, "  add a check for %s == %t (mutant: %t)", SanitizeInline(witness.Call), witness.Original, witness.Mutant)
			} else {
				fmt.Fprintf(&text, "  distinguishing operand: %s = %s (original: %t, mutant: %t)", SanitizeInline(witness.Operand), SanitizeInline(witness.Input), witness.Original, witness.Mutant)
			}
			if witness.Verified {
				text.WriteString(" [verified]")
			}
			text.WriteByte('\n')
			writeSuggestion(&text, witness.Code)
		}
		if witness := finding.SourceWitness; witness != nil {
			fmt.Fprintf(&text, "  verified with %s (original %s, mutant %s)\n",
				SanitizeInline(witness.Call), SanitizeInline(witness.Original), SanitizeInline(witness.Mutated))
			writeSuggestion(&text, witness.Code)
		}
		if len(finding.Packages) != 0 {
			pkgs := make([]string, len(finding.Packages))
			for i, p := range finding.Packages {
				pkgs[i] = SanitizeInline(p)
			}
			fmt.Fprintf(&text, "  test packages: %s\n", strings.Join(pkgs, ", "))
		}
		if len(finding.Tests) != 0 {
			label := "failing tests"
			if finding.Status == "assertion-gap" {
				label = "tests that observed the change"
			}
			tests := make([]string, len(finding.Tests))
			for i, name := range finding.Tests {
				tests[i] = SanitizeInline(name)
			}
			fmt.Fprintf(&text, "  %s: %s\n", label, strings.Join(tests, ", "))
		}
		if finding.Detail != "" {
			for line := range strings.SplitSeq(SanitizeBlock(finding.Detail), "\n") {
				fmt.Fprintf(&text, "  %s\n", line)
			}
		}
		text.WriteByte('\n')
	}
	if report.Evaluation == "listed" {
		fmt.Fprintf(&text, "%d sites, %d candidates", report.Sites, len(report.Findings))
		if report.Suppressed > 0 {
			fmt.Fprintf(&text, ", %d suppressed", report.Suppressed)
		}
		text.WriteByte('\n')
		_, err := io.WriteString(out, text.String())
		return err
	}
	fmt.Fprintf(&text, "%d sites, %d test binaries, %d probe replays (%d avoided), %d source runs, %d killed",
		report.Sites, report.Builds, report.Replays, report.Skipped, report.SourceRuns, report.Killed)
	if report.TimedOut > 0 {
		fmt.Fprintf(&text, " (%d timed out)", report.TimedOut)
	}
	fmt.Fprintf(&text, ", %d survived, %d unreached, %d non-viable, %d incomplete",
		report.Survived, report.Unreached, report.Rejected, report.Incomplete)
	if report.Suppressed > 0 {
		fmt.Fprintf(&text, ", %d suppressed", report.Suppressed)
	}
	if report.Resumed > 0 {
		fmt.Fprintf(&text, ", %d resumed", report.Resumed)
	}
	if report.Audited > 0 {
		fmt.Fprintf(&text, ", %d audited", report.Audited)
	}
	if report.PreviousRuns > 0 {
		fmt.Fprintf(&text, ", %d previous test runs", report.PreviousRuns)
	}
	text.WriteByte('\n')
	_, err := io.WriteString(out, text.String())
	return err
}

func writeSuggestion(out *strings.Builder, code string) {
	if code != "" {
		for line := range strings.SplitSeq(strings.TrimSpace(SanitizeBlock(code)), "\n") {
			fmt.Fprintf(out, "    %s\n", line)
		}
	}
}

type boolFlag interface {
	flag.Value
	IsBoolFlag() bool
}

func reorderAnalyzeArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			flags = append(flags, "--")
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		trimmed := strings.TrimLeft(arg, "-")
		if strings.IndexByte(trimmed, '=') >= 0 {
			continue
		}
		f := fs.Lookup(trimmed)
		if f == nil {
			continue
		}
		if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag needs an argument: -%s", trimmed)
		}
		i++
		flags = append(flags, args[i])
	}
	return append(flags, positional...), nil
}
