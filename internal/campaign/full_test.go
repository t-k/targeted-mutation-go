package campaign

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestEmptyBodyUsesStaticRejectionOnlyWhenGuaranteed(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/empty-body.tsv")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")[1:]
	dir := t.TempDir()
	fixture(t, dir, "go.mod", "module example.org/empty\n\ngo 1.26.0\n")
	var source strings.Builder
	source.WriteString("package empty\ntype receiver struct{}\n")
	type expectation struct {
		line, column int
		invalid      bool
	}
	var cases []expectation
	for i, row := range rows {
		fields := strings.Fields(row)
		form, result := fields[0], fields[1]
		signature, statement := "", "_ = x"
		switch result {
		case "named":
			signature, statement = " (r bool)", "return"
		case "unnamed":
			signature, statement = " bool", "return x"
		}
		declaration := fmt.Sprintf("func Case%d", i)
		switch form {
		case "method":
			declaration = fmt.Sprintf("func (receiver) Case%d", i)
		case "closure":
			declaration = "var _ = func"
		case "inner":
			statement = "if x { _ = x }; " + statement
		}
		line := fmt.Sprintf("%s(x bool)%s { %s }", declaration, signature, statement)
		fmt.Fprintln(&source, line)
		cases = append(cases, expectation{i + 3, strings.LastIndex(line, "{") + 1, form != "inner" && result != "none"})
	}
	fixture(t, dir, "empty.go", source.String())
	full, err := Run(context.Background(), Config{Dir: dir, Full: true, Jobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	oracle, err := Run(context.Background(), Config{Dir: dir, SourceOnly: true, Jobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(report Report, expected expectation) Finding {
		for _, f := range report.Findings {
			if f.Rule == "empty-block" && f.Line == expected.line && f.Column == expected.column {
				return f
			}
		}
		t.Fatalf("missing empty-block finding on %d:%d", expected.line, expected.column)
		return Finding{}
	}
	for i, expected := range cases {
		f, native := lookup(full, expected), lookup(oracle, expected)
		if f.Status != native.Status || f.Expression != native.Expression || f.Mutation != native.Mutation || native.Evidence != "source-replay" {
			t.Fatalf("%s: full %+v, source-only %+v", rows[i], f, native)
		}
		want := "source-replay"
		if expected.invalid {
			want = "static"
			if f.Status != "non-viable" || f.Detail != "missing return" {
				t.Fatalf("%s: expected missing return, got %+v", rows[i], f)
			}
		}
		if f.Evidence != want {
			t.Errorf("%s: got evidence %q, want %q", rows[i], f.Evidence, want)
		}
	}
	if full.Rejected != oracle.Rejected || full.SourceRuns >= oracle.SourceRuns {
		t.Fatalf("full rejection and replay counts = %d/%d, source-only = %d/%d", full.Rejected, full.SourceRuns, oracle.Rejected, oracle.SourceRuns)
	}
}

func TestFullComparisonRoutesMatchSourceSemantics(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/full-comparison.tsv")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")[1:]
	for _, mode := range []string{"full", "verified", "source-only"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			fixture(t, dir, "go.mod", "module example.org/compare\n\ngo 1.26.0\n")
			var code, tests strings.Builder
			code.WriteString("package compare\ntype Word string\n")
			tests.WriteString("package compare\nimport \"testing\"\nfunc TestComparisons(t *testing.T) {\n")
			type expectation struct {
				row, original, mutant, status, evidence string
				line                                    int
				unreached                               bool
			}
			var cases []expectation
			for _, row := range rows {
				fields := strings.Fields(row)
				if fields[2] != mode {
					continue
				}
				order := map[string]int{"less": -1, "equal": 0, "greater": 1}[fields[1]]
				expected := expectation{row: row, original: ">=", mutant: "==", evidence: "source-replay", line: len(cases) + 3}
				signature, expr, args := "(x, y int)", "x >= y", fmt.Sprintf("%d, 2", 2+order)
				reusable := true
				switch fields[0] {
				case "constant-left":
					signature, expr, args = "(x int)", "2 <= x", strconv.Itoa(2-order)
					expected.original, expected.mutant = "<=", ">="
				case "strings":
					signature, expr, args = "(x Word)", `"b" == x`, strconv.Quote(string(rune('b'-order)))
					expected.original, expected.mutant = "==", "<"
				case "float":
					signature, reusable = "(x, y float64)", false
				case "nested-bool":
					signature, expr, args = "(x, y int, ok bool)", "x < y == ok", "1, 2, true"
					expected.original, expected.mutant = "==", "!="
					order, reusable = 0, false
				case "joined":
					expr, args = "x>-y", fmt.Sprintf("%d, -2", 2+order)
					expected.original = ">"
				}
				name := fmt.Sprintf("Compare%d", len(cases))
				fmt.Fprintf(&code, "func %s%s bool { return %s }\n", name, signature, expr)
				before, after := comparisonResult(expected.original, order), comparisonResult(expected.mutant, order)
				expected.status = "source-survivor"
				if fields[1] != "unreached" {
					fmt.Fprintf(&tests, "if %s(%s) != %t { t.Error(%q) }\n", name, args, before, name)
					if before != after {
						expected.status = "killed"
					}
				}
				if reusable && mode != "source-only" {
					expected.unreached = fields[1] == "unreached"
					switch {
					case fields[1] == "unreached" && mode == "verified":
					case expected.status == "killed" || mode == "verified":
						expected.evidence = "replayed"
					default:
						expected.evidence = "observed"
					}
				}
				cases = append(cases, expected)
			}
			tests.WriteString("}\n")
			fixture(t, dir, "compare.go", code.String())
			fixture(t, dir, "compare_test.go", tests.String())
			report, err := Run(context.Background(), Config{Dir: dir, Jobs: 4, Full: mode != "source-only",
				SourceOnly: mode == "source-only", VerifySurvivors: mode == "verified", KilledFindings: true, ShowUnreached: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range cases {
				var got Finding
				unreached := false
				for _, f := range report.Findings {
					unreached = unreached || f.Line == want.line && f.Status == "unreached"
					if f.Line == want.line && f.Rule == "source-operator" && f.Expression == want.original && f.Mutation == want.mutant {
						got = f
					}
				}
				if unreached != want.unreached {
					t.Errorf("%s: unreachable finding is %t, want %t", want.row, unreached, want.unreached)
				}
				if got.Status != want.status || got.Evidence != want.evidence {
					t.Errorf("%s: got %+v, want %s with %s", want.row, got, want.status, want.evidence)
				}
				if strings.HasPrefix(want.row, "joined\t") {
					var raw, grouped bool
					for _, f := range report.Findings {
						if f.Line == want.line && f.Rule == "source-operator" && f.Expression == ">" && f.Mutation == "<" {
							raw = f.Status == "non-viable" && f.Evidence == "source-replay"
						}
						if f.Line == want.line && f.Rule == "operator-grouping" && strings.Contains(f.Mutation, " < ") {
							grouped = f.Evidence == "source-replay"
						}
					}
					if !raw || !grouped {
						t.Errorf("%s: missing joined raw or grouped candidate (%t, %t)", want.row, raw, grouped)
					}
				}
			}
		})
	}
}

func comparisonResult(op string, order int) bool {
	switch op {
	case "<":
		return order < 0
	case "<=":
		return order <= 0
	case ">":
		return order > 0
	case ">=":
		return order >= 0
	case "==":
		return order == 0
	case "!=":
		return order != 0
	}
	panic("invalid comparison " + op)
}

func TestUnconfirmedComparisonProbeFallsBackToSource(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "go.mod", "module example.org/fallback\n\ngo 1.26.0\n")
	fixture(t, dir, "label.go", "package fallback\nfunc Label(s string) bool { return s == \"a\" }\n")
	fixture(t, dir, "label_test.go", `package fallback
import ("os"; "testing")
func TestLabel(t *testing.T) {
	if os.Getenv("TMT_GO_MUTATION") != "" { t.Skip("missing activation evidence") }
	if !Label("a") { t.Fatal("expected a match") }
}
`)
	report, err := Run(context.Background(), Config{Dir: dir, Full: true, Jobs: 2, KilledFindings: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range report.Findings {
		if f.Rule == "source-operator" && f.Expression == "==" && f.Mutation == "!=" {
			if f.Status != "killed" || f.Evidence != "source-replay" || report.Replays == 0 || report.Incomplete != 0 {
				t.Fatalf("missing successful source fallback: %+v, counts %+v", f, report)
			}
			return
		}
	}
	t.Fatalf("missing comparison mutation: %+v", report)
}
