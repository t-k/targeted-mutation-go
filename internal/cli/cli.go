// Package cli implements the user-facing command and report formats.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/t-k/targeted-mutation-go/internal/campaign"
)

func Execute(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		usage(out)
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintln(out, version())
		return 0
	}
	if args[0] != "analyze" {
		fmt.Fprintf(errOut, "unknown command %q\n", args[0])
		usage(errOut)
		return 1
	}
	options := flag.NewFlagSet("analyze", flag.ContinueOnError)
	options.SetOutput(errOut)
	options.Usage = func() {
		usage(errOut)
		options.PrintDefaults()
	}
	var cfg campaign.Config
	options.StringVar(&cfg.Dir, "dir", ".", "project or workspace directory")
	options.StringVar(&cfg.Since, "since", "", "restrict sites to working-tree lines changed from this Git revision")
	options.StringVar(&cfg.Comparison, "comparison", "all", "comparison rules (all, boundary, or negation)")
	options.StringVar(&cfg.Tags, "tags", "", "comma-separated Go build tags")
	options.IntVar(&cfg.Jobs, "jobs", min(4, runtime.GOMAXPROCS(0)), "maximum concurrent build or mutation workers")
	options.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "timeout per test package process")
	options.DurationVar(&cfg.BuildTimeout, "build-timeout", 5*time.Minute, "timeout per Go build or Go test command")
	options.BoolVar(&cfg.Full, "full", false, "also run supported source-level operator mutations")
	options.BoolVar(&cfg.Conventional, "conventional", false, "run boundary and negation mutations without threshold synthesis")
	options.BoolVar(&cfg.VerifySurvivors, "verify-survivors", false, "replay comparison mutants whose observations predict survival")
	options.BoolVar(&cfg.VerifyWitnesses, "verify-witnesses", false, "compile and run synthesized witness tests for direct calls")
	options.BoolVar(&cfg.ShowUnreached, "show-unreached", false, "include comparison sites the selected tests did not reach")
	options.BoolVar(&cfg.UndoChanges, "undo-changes", false, "run tests with each edited Go hunk restored to --since")
	options.BoolVar(&cfg.KilledFindings, "show-killed", false, "include killed mutants in the report")
	format := options.String("format", "text", "report format (text or json)")
	if err := options.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(errOut, "invalid report format %q\n", *format)
		return 1
	}
	if cfg.Jobs < 1 || cfg.Timeout <= 0 || cfg.BuildTimeout <= 0 {
		fmt.Fprintln(errOut, "--jobs, --timeout, and --build-timeout must be positive")
		return 1
	}
	if cfg.Since == "" && cfg.UndoChanges {
		fmt.Fprintln(errOut, "--undo-changes requires --since")
		return 1
	}
	if cfg.Conventional && cfg.VerifyWitnesses {
		fmt.Fprintln(errOut, "--verify-witnesses is incompatible with --conventional")
		return 1
	}
	cfg.Patterns = options.Args()
	for _, pattern := range cfg.Patterns {
		if strings.HasSuffix(pattern, ".go") {
			fmt.Fprintf(errOut, "expected a Go package pattern, got a file: %q\n", pattern)
			return 1
		}
		if strings.HasPrefix(pattern, "-") {
			fmt.Fprintf(errOut, "flags must precede package patterns: %q\n", pattern)
			return 1
		}
	}
	report, err := campaign.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	if *format == "json" {
		encoder := json.NewEncoder(out)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(report)
	} else {
		err = writeText(out, report)
	}
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	if report.Incomplete != 0 {
		return 3
	}
	for _, finding := range report.Findings {
		switch finding.Status {
		case "input-gap", "assertion-gap", "source-survivor", "untested-change":
			return 2
		}
	}
	return 0
}

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

func writeText(out io.Writer, report campaign.Report) error {
	var text strings.Builder
	for _, finding := range report.Findings {
		label := strings.ToUpper(strings.ReplaceAll(finding.Status, "-", " "))
		fmt.Fprintf(&text, "%s %s:%d:%d\n", label, finding.File, finding.Line, finding.Column)
		if finding.Expression != "" {
			fmt.Fprintf(&text, "  expression: %s\n", finding.Expression)
		}
		if finding.Mutation != "" {
			fmt.Fprintf(&text, "  mutation: %s [%s]\n", finding.Mutation, finding.Evidence)
		}
		if len(finding.Interval) == 2 {
			fmt.Fprintf(&text, "  observed-equivalent thresholds: %s..%s\n", finding.Interval[0], finding.Interval[1])
		}
		if witness := finding.Witness; witness != nil {
			if witness.Call != "" {
				fmt.Fprintf(&text, "  add a check for %s == %t (mutant: %t)", witness.Call, witness.Original, witness.Mutant)
			} else {
				fmt.Fprintf(&text, "  distinguishing operand: %s = %s (original: %t, mutant: %t)", witness.Operand, witness.Input, witness.Original, witness.Mutant)
			}
			if witness.Verified {
				text.WriteString(" [verified]")
			}
			text.WriteByte('\n')
		}
		if len(finding.Packages) != 0 {
			fmt.Fprintf(&text, "  test packages: %s\n", strings.Join(finding.Packages, ", "))
		}
		if len(finding.Tests) != 0 {
			fmt.Fprintf(&text, "  failing tests: %s\n", strings.Join(finding.Tests, ", "))
		}
		if finding.Detail != "" {
			fmt.Fprintf(&text, "  %s\n", finding.Detail)
		}
		text.WriteByte('\n')
	}
	fmt.Fprintf(&text, "%d sites, %d test binaries, %d probe replays (%d avoided), %d source runs, %d killed, %d unreached, %d incomplete\n",
		report.Sites, report.Builds, report.Replays, report.Skipped, report.SourceRuns, report.Killed, report.Unreached, report.Incomplete)
	_, err := io.WriteString(out, text.String())
	return err
}
