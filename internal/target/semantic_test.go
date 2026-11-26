package target

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSemanticRulesPICT(t *testing.T) {
	type family struct {
		path, name, params, ret, args, kind string
		alternatives                        []string
	}
	families := map[string]family{
		"extrema":      {"math", "Min", "x, y float64", "float64", "x, y", "min-max", []string{"Max"}},
		"rounding":     {"math", "Floor", "x float64", "float64", "x", "rounding-mode", []string{"Ceil", "Round", "RoundToEven", "Trunc"}},
		"prefix":       {"strings", "HasPrefix", "s, prefix string", "bool", "s, prefix", "affix-contains", []string{"Contains"}},
		"replacement":  {"strings", "Replace", "s, old, new string, n int", "string", `s, "o", "x", 1`, "replacement-count", []string{`ReplaceAll(s, "o", "x")`}},
		"search":       {"strings", "LastIndex", "s, substr string", "int", "s, substr", "search-direction", []string{"Index"}},
		"literal-list": {"slices", "Contains", "unused []int, x int", "bool", "[]int{1, 2, 3}, x", "slice-element-remove", []string{"[]int{1, 2}", "[]int{1, 3}", "[]int{2, 3}"}},
	}
	data, err := os.Open("../../verification/pict/semantic-rules.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	reader := csv.NewReader(data)
	reader.Comma = '\t'
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/semantic\n\ngo 1.26.0\n")
	type want struct {
		pattern, binding, kind string
		replacements           []string
	}
	expected := make(map[string][]want)
	for i, row := range rows[1:] {
		rule, ok := families[row[0]]
		if !ok {
			t.Fatalf("unknown rule %q", row[0])
		}
		pkg := fmt.Sprintf("p%d", i)
		if err := os.Mkdir(filepath.Join(dir, pkg), 0700); err != nil {
			t.Fatal(err)
		}
		prefix := rule.path + "."
		header := fmt.Sprintf("package %s\nimport %q\n", pkg, rule.path)
		var replacements = slices.Clone(rule.alternatives)
		switch row[1] {
		case "aliased":
			header = fmt.Sprintf("package %s\nimport alias %q\n", pkg, rule.path)
			prefix = "alias."
		case "dot":
			header = fmt.Sprintf("package %s\nimport . %q\n", pkg, rule.path)
			prefix = ""
		case "shadowed":
			header = fmt.Sprintf("package %s\nvar %s struct { %s func(%s) %s }\n", pkg, rule.path, rule.name, rule.params, rule.ret)
			replacements = nil
		}
		if row[0] == "replacement" && len(replacements) > 0 {
			replacements[0] = prefix + replacements[0]
		}
		params := rule.params
		if row[0] == "replacement" {
			params = "s string"
		}
		body := fmt.Sprintf("func F(%s) %s { return %s%s(%s) }\n", params, rule.ret, prefix, rule.name, rule.args)
		write(t, dir, pkg+"/f.go", header+body)
		expected[row[2]] = append(expected[row[2]], want{"./" + pkg, row[1], rule.kind, replacements})
	}
	for _, mode := range []string{"full", "source-only"} {
		t.Run(mode, func(t *testing.T) {
			var patterns []string
			byPath := make(map[string]want)
			for _, w := range expected[mode] {
				patterns = append(patterns, w.pattern)
				byPath["example.org/semantic/"+strings.TrimPrefix(w.pattern, "./")] = w
			}
			packages, err := Discover(context.Background(), dir, patterns, "", nil, Discovery{Full: mode == "full", SourceOnly: mode == "source-only"})
			if err != nil {
				t.Fatal(err)
			}
			if len(packages) != len(patterns) {
				t.Fatalf("processed %d of %d PICT rows", len(packages), len(patterns))
			}
			for _, pkg := range packages {
				w := byPath[pkg.Path]
				var found []string
				for _, file := range pkg.Files {
					for _, edit := range file.Edits {
						if edit.Kind == w.kind {
							if edit.Site != nil {
								t.Errorf("%s unexpectedly routed to a comparison probe", edit.Kind)
							}
							found = append(found, edit.After)
						}
					}
				}
				slices.Sort(found)
				slices.Sort(w.replacements)
				if !slices.Equal(found, w.replacements) {
					t.Errorf("%s %s %s: got %q, want %q", pkg.Path, w.binding, w.kind, found, w.replacements)
				}
			}
		})
	}
}

func TestSemanticRulesBuildAndChangeResults(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/semantic\n\ngo 1.26.0\n")
	const source = `package semantic
import ("bytes"; "math"; s "strings"; "slices")
func low(a,b int) int { return min(a,b) }
func high(xs []int) int { return (slices.Max[[]int,int])(xs) }
func round(x float64) float64 { return math.Floor(x) }
func prefix(x string) bool { return (s.HasPrefix)(x,"AB") }
func suffix(x []byte) bool { return bytes.HasSuffix(x,[]byte("AB")) }
func cut(x string) (string, bool) { return s.CutPrefix(x,"AB") }
func first(x string) string {
    return (s.Replace)(x, "o", "X",
       1, // replace exactly once
    )
}
func all(x []byte) []byte {
    return bytes.ReplaceAll(x, []byte("o"), []byte("X"), // replace all matches
    )
}
func pos(x []byte) int { return bytes.IndexByte(x,'a') }
func contains(x int) bool { return slices.Contains([]int{1, 2, 3}, x) }
func index(x int) int { return slices.Index([]int{1, 2, 3}, x) }
func roundFirst(a,b,c int) int { return 1 + (a*b/c) }
func roundSecond(a,b,c uint8) uint8 { return a*b/c }
`
	write(t, dir, "f.go", source)
	write(t, dir, "f_test.go", `package semantic
import "testing"
func TestResults(t *testing.T) {
    check := func(name string, same bool) { t.Run(name, func(t *testing.T) {
        if !same { t.Fatal("unexpected result") }
    }) }
    check("low", low(1,2) == 1)
    check("high", high([]int{1,2}) == 2)
    check("round", round(1.25) == 1)
    check("prefix", !prefix("XAB"))
    check("suffix", !suffix([]byte("ABX")))
    _, hasPrefix := cut("ABX")
    check("cut", hasPrefix)
    check("first", first("oo") == "Xo")
    check("all", string(all([]byte("oo"))) == "XX")
    check("pos", pos([]byte("aa")) == 0)
    check("contains", contains(1))
    check("index", index(3) == 2)
    check("roundFirst", roundFirst(3,4,2) == 7)
    check("roundSecond", roundSecond(4,3,2) == 6)
}
`)
	run := func() ([]byte, error) {
		cmd := exec.Command("go", "test", "-p=2", "-count=1", ".")
		cmd.Dir = dir
		return cmd.CombinedOutput()
	}
	if output, err := run(); err != nil {
		t.Fatalf("original fixture: %v\n%s", err, output)
	}
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{SourceOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	type candidate struct {
		kind, before, after string
	}
	cases := map[string]candidate{
		"low":         {"min-max", "min", "max"},
		"high":        {"min-max", "Max", "Min"},
		"round":       {"rounding-mode", "Floor", "Ceil"},
		"prefix":      {"affix-contains", "HasPrefix", "Contains"},
		"suffix":      {"affix-contains", "HasSuffix", "Contains"},
		"cut":         {"affix-end", "CutPrefix", "CutSuffix"},
		"first":       {"replacement-count", "(s.Replace)(x, \"o\", \"X\",\n       1, // replace exactly once\n    )", `(s.ReplaceAll)(x, "o", "X")`},
		"all":         {"replacement-count", "bytes.ReplaceAll(x, []byte(\"o\"), []byte(\"X\"), // replace all matches\n    )", "bytes.Replace(x, []byte(\"o\"), []byte(\"X\"), 1, // replace all matches\n    )"},
		"pos":         {"search-direction", "IndexByte", "LastIndexByte"},
		"contains":    {"slice-element-remove", "[]int{1, 2, 3}", "[]int{2, 3}"},
		"index":       {"slice-element-remove", "[]int{1, 2, 3}", "[]int{2, 3}"},
		"roundFirst":  {"integer-rounding-order", "a*b/c", "((a / c) * b)"},
		"roundSecond": {"integer-rounding-order", "a*b/c", "(a * (b / c))"},
	}
	lines := strings.Split(source, "\n")
	var edits []Edit
	for name, want := range cases {
		index := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, "func "+name+"(") })
		if index < 0 {
			t.Fatalf("fixture missing %s", name)
		}
		line := index + 1
		if !strings.Contains(lines[index], "return") {
			line++
		}
		for _, file := range pkgs[0].Files {
			for _, edit := range file.Edits {
				if edit.Line == line && edit.Kind == want.kind && edit.Before == want.before && edit.After == want.after {
					edits = append(edits, edit)
				}
			}
		}
	}
	if len(edits) != len(cases) {
		t.Fatalf("found %d of %d distinct semantic edits: %+v", len(edits), len(cases), edits)
	}
	slices.SortFunc(edits, func(a, b Edit) int { return b.Start - a.Start })
	modified := source
	for _, edit := range edits {
		if modified[edit.Start:edit.End] != edit.Before {
			t.Fatalf("edits overlap: %+v", edit)
		}
		modified = modified[:edit.Start] + edit.After + modified[edit.End:]
	}
	write(t, dir, "f.go", modified)
	output, err := run()
	if err == nil {
		t.Fatalf("mutant unexpectedly survived:\n%s", output)
	}
	for name := range cases {
		if !strings.Contains(string(output), "--- FAIL: TestResults/"+name+" ") {
			t.Errorf("%s did not reach the expected failing assertion:\n%s", name, output)
		}
	}
}

func TestSemanticRulesLeaveUnsupportedShapesAlone(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/semantic\n\ngo 1.26.0\n")
	write(t, dir, "f.go", `package semantic
import (. "strings"; s "slices")
type imposter string
func (imposter) HasPrefix(string) bool { return true }
func f(x string) bool {
    Contains := func(x,y string) bool { return false }
    _ = Contains
    return HasPrefix(x,"AB")
}
func method(x imposter) bool { return x.HasPrefix("AB") }
func shadow(x,y int) int {
    max := func(x,y int) int { return x }
    _ = max
    return min(x,y)
}
const limited uint8 = min(1, 256)
func all(s string) string { return Replace(s,"a","b",-1) }
func twice(s string) string { return Replace(s,"a","b",2) }
func counted(s string, n int) string { return Replace(s,"a","b",n) }
func untyped(a uint8) uint8 { return (1<<31) * 0 / a }
func floating(a,b,c float64) float64 { return a*b/c }
func dynamic(f func() int, b,c int) int { return f()*b/c }
func keyed(x int) bool { return s.Contains([]int{0:1, 2:2}, x) }
func duplicate(x int) bool { return s.Contains([]int{1, 1, 2}, x) }
func unknown(x,y int) bool { return s.Contains([]int{1, y}, x) }
func single(x int) bool { return s.Contains([]int{1}, x) }
func long(x int) bool { return s.Contains([]int{1,2,3,4,5,6,7,8,9}, x) }
`)
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range pkgs[0].Files {
		for _, edit := range file.Edits {
			switch edit.Kind {
			case "min-max", "affix-contains", "replacement-count", "integer-rounding-order", "slice-element-remove":
				t.Errorf("unexpected mutation in unsupported form: %+v", edit)
			}
		}
	}
}

func TestSemanticRulesObserveSourceScope(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/semantic\n\ngo 1.26.0\n")
	write(t, dir, "f.go", `package semantic
import "strings"
func first(x string) bool { return strings.HasPrefix(x,"AB") }
func last(x string) bool { return strings.HasSuffix(x,"AB") }
`)
	file := filepath.Join(dir, "f.go")
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", Scope{file: {{Start: 4, End: 4}}}, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, file := range pkgs[0].Files {
		for _, edit := range file.Edits {
			if edit.Kind == "affix-contains" {
				lines = append(lines, edit.Line)
			}
		}
	}
	if !slices.Equal(lines, []int{4}) {
		t.Fatalf("mutation lines %v, want [4]", lines)
	}
	pkgs, err = Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs[0].Files) != 0 {
		t.Fatalf("semantic rules ran without the full or source-only catalogue")
	}
}

func TestSemanticRulesRetainImportReferences(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/semantic\n\ngo 1.26.0\n")
	write(t, dir, "qualified.go", `package semantic
import ("strings"; clock "time")
func Qualified(x string) string { return strings.Replace(x, "a", "b", int(clock.Nanosecond)) }
`)
	write(t, dir, "dot.go", `package semantic
import ("strings"; . "time")
func Dotted(x string) string { return strings.Replace(x, "a", "b", int(Nanosecond)) }
`)
	write(t, dir, "shadow.go", `package semantic
import . "strings"
func Shadowed(x string) string {
    ReplaceAll := func(string, string, string) string { return "" }
    _ = ReplaceAll
    return Replace(x, "a", "b", 1)
}
`)
	write(t, dir, "slice.go", `package semantic
import ("slices"; clock "time")
func Slice(x int) bool { return slices.Contains([]int{0, int(clock.Nanosecond)}, x) }
`)
	write(t, dir, "named.go", `package semantic
import ("net"; "slices")
func Named(x byte) bool { return slices.Contains(net.IP{1, 2}, x) }
`)
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{SourceOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var reduced []string
	for _, file := range pkgs[0].Files {
		for _, edit := range file.Edits {
			switch edit.Kind {
			case "replacement-count":
				t.Errorf("count mutation drops an import reference or changes its binding: %+v", edit)
			case "slice-element-remove":
				if filepath.Base(edit.File) != "named.go" {
					t.Errorf("slice mutation drops a constant import reference: %+v", edit)
				}
				reduced = append(reduced, edit.After)
			}
		}
	}
	slices.Sort(reduced)
	if !slices.Equal(reduced, []string{"net.IP{1}", "net.IP{2}"}) {
		t.Fatalf("slice substitution must retain its imported type: %q", reduced)
	}
}

func TestSemanticRulesOmitProvenIdentities(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/semantic\n\ngo 1.26.0\n")
	write(t, dir, "f.go", `package semantic
func Min(x int) int { return min(x) }
func Max(x float64) float64 { return max(x) }
func DividePositive(a,b int8) int8 { return a*b/1 }
func DivideNegative(a,b int8) int8 { return a*b/ -1 }
func ZeroLeft(b,c int8) int8 { return 0*b/c }
func ZeroRight(a,c int8) int8 { return a*0/c }
func OneLeft(b,c int8) int8 { return 1*b/c }
func OneRight(a,c int8) int8 { return a*1/c }
`)
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	var reorders []string
	for _, file := range pkgs[0].Files {
		for _, edit := range file.Edits {
			switch edit.Kind {
			case "min-max":
				t.Errorf("unary extrema produce no semantic difference: %+v", edit)
			case "integer-rounding-order":
				reorders = append(reorders, edit.After)
			}
		}
	}
	slices.Sort(reorders)
	if !slices.Equal(reorders, []string{"((1 / c) * b)", "(a * (1 / c))"}) {
		t.Fatalf("reordered expressions = %q", reorders)
	}
}

func TestRoundingDivisionByUnitWithSignedWrap(t *testing.T) {
	for _, divisor := range []int8{-1, 1} {
		for x := -128; x <= 127; x++ {
			for y := -128; y <= 127; y++ {
				a, b := int8(x), int8(y)
				original := (a * b) / divisor
				if original != (a/divisor)*b || original != a*(b/divisor) {
					t.Fatalf("reordering changes %d * %d / %d", a, b, divisor)
				}
			}
		}
	}
}
