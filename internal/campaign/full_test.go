package campaign

import (
	"context"
	"fmt"
	"os"
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
