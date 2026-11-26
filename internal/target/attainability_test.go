package target

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
)

func TestAttainabilityPICT(t *testing.T) {
	table, err := os.ReadFile("../../verification/pict/attainability.tsv")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(table)), "\n")[1:]
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/attainable\n\ngo 1.26.0\n")
	var source strings.Builder
	source.WriteString("package attainable\n")
	for i, row := range rows {
		fields := strings.Fields(row)
		typ, lhs := "int", "x"
		if strings.HasPrefix(fields[0], "unsigned-") {
			typ = "uint"
		}
		switch {
		case strings.HasSuffix(fields[0], "-mask"):
			lhs = "x & 5"
		case strings.HasSuffix(fields[0], "-remainder"):
			lhs = "x % 3"
		}
		pivot := map[string]int{"inside": 1, "outside": 8, "hole": 2}[fields[1]]
		fmt.Fprintf(&source, "func F%d(x %s) bool { return %s >= %d }\n", i, typ, lhs, pivot)
	}
	write(t, dir, "test.go", source.String())
	packages, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{})
	if err != nil {
		t.Fatal(err)
	}
	sites := packages[0].Files[0].Sites
	if len(sites) != len(rows) {
		t.Fatalf("discovered %d sites for %d rows", len(sites), len(rows))
	}
	for i, row := range rows {
		fields := strings.Fields(row)
		site := sites[i]
		targeted := fields[2] == "targeted"
		var seen analysis.Summary
		zero := uint64(0)
		if site.Domain.Signed {
			zero = analysis.RankSigned(0)
		}
		seen.Observe(zero, site.Domain.Pivot)
		present := false
		for _, candidate := range analysis.Plan(site.Op, site.Domain, seen, targeted) {
			present = present || candidate.Kind == "boundary"
		}
		if present != (!targeted || fields[1] == "inside") {
			t.Errorf("%s: boundary candidate present = %t", row, present)
		}
	}
}
