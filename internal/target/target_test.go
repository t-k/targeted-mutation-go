package target

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
)

func TestProbePreservesBoundaryAfterKeyword(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/tight\n\ngo 1.26.0\n")
	write(t, dir, "tight.go", `package tight
func Rune(x rune) bool { return'a' < x }
func String(x string) bool { if"a" < x { return false }; return true }
`)
	pkg, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	file := pkg[0].Files[0]
	if len(file.Sites) != 2 {
		t.Fatalf("wanted both comparison sites, got %d", len(file.Sites))
	}
	rewritten := Rewrite(file, pkg[0].Prefix)
	if _, err := parser.ParseFile(token.NewFileSet(), file.Path, rewritten, 0); err != nil {
		t.Fatalf("rewritten source lost a token boundary: %v\n%s", err, rewritten)
	}
}

func TestIntegerStepProbePICT(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/literal-probe.tsv")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")[1:]
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/pivots\n\ngo 1.26.0\n")
	var source strings.Builder
	source.WriteString("package pivots\ntype Amount int8\n")
	type example struct {
		row   string
		from  int
		to    int
		op    analysis.Operator
		valid bool
	}
	var cases []example
	for i, row := range rows {
		fields := strings.Fields(row)
		typ := map[string]string{"signed": "int8", "unsigned": "uint8", "named": "Amount"}[fields[1]]
		limit := 127
		if typ == "uint8" {
			limit = 255
		}
		delta := 1
		if fields[2] == "down" {
			delta = -1
		}
		from, op := 4, analysis.GE
		expr := "x >= 4"
		switch fields[0] {
		case "left":
			expr, op = "4 >= x", analysis.LE
		case "parentheses":
			expr = "x >= ((4))"
		case "unary":
			expr = "x >= -4"
		case "maximum":
			from, expr = limit, fmt.Sprintf("x >= %d", limit)
		case "minimum":
			from, expr = 0, "x >= 0"
		}
		to := from + delta
		valid := fields[0] != "unary" && to <= limit && (typ != "uint8" || to >= 0)
		cases = append(cases, example{row, from, to, op, valid})
		fmt.Fprintf(&source, "func F%d(x %s) bool { return %s }\n", i, typ, expr)
	}
	write(t, dir, "pivots.go", source.String())
	pkg, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range cases {
		var got Edit
		for _, edit := range pkg[0].Files[0].Edits {
			if edit.Line == i+3 && edit.Kind == "integer-step" && edit.Before == strconv.Itoa(want.from) &&
				strings.Trim(edit.After, "()") == strconv.Itoa(want.to) {
				got = edit
				break
			}
		}
		if got.Kind == "" || (got.Site != nil) != want.valid {
			t.Errorf("%s: unexpected routing: %+v", want.row, got)
			continue
		}
		if want.valid {
			pivot := uint64(want.to)
			if got.Site.Domain.Signed {
				pivot = analysis.RankSigned(int64(want.to))
			}
			if got.Pivot != pivot || got.Operator != want.op || got.Shift != want.to-want.from {
				t.Errorf("%s: unexpected selector: %+v", want.row, got)
			}
		}
	}
}

func TestForcedComparisonProbePICT(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/forced-comparison.tsv")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")[1:]
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/force\n\ngo 1.26.0\n")
	var source strings.Builder
	source.WriteString("package force\nvar true = false\n")
	for i, row := range rows {
		fields := strings.Fields(row)
		setup := ""
		expr := map[string]string{"parameters": "x > y", "literal": "x > 4", "index": "xs[x] > y",
			"local": "n > y", "division": "x / y > 0"}[fields[1]]
		if fields[1] == "local" {
			setup = "n := x; "
		}
		body := "return " + expr
		switch fields[0] {
		case "if":
			body = "if " + expr + " { return true }; return false"
		case "for":
			body = "for " + expr + " { return true }; return false"
		case "tight-return":
			body = "return(" + expr + ")"
		case "tight-if":
			body = "if(" + expr + ") { return true }; return false"
		}
		fmt.Fprintf(&source, "func F%d(x, y int, xs []int) bool { %s%s }\n", i, setup, body)
	}
	write(t, dir, "force.go", source.String())
	pkg, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		fields := strings.Fields(row)
		kind, change, mask := "condition-constant", "false", uint64(0)
		isReturn := strings.HasSuffix(fields[0], "return")
		if isReturn {
			kind = "return-boolean"
		}
		if fields[2] == "true" {
			change, mask = "(0 == 0)", 7
		}
		valid := fields[1] == "parameters" || fields[1] == "literal"
		if strings.HasPrefix(fields[0], "tight-") && fields[2] == "false" {
			valid = false
		}
		var forced, negated *Edit
		for j := range pkg[0].Files[0].Edits {
			edit := &pkg[0].Files[0].Edits[j]
			if edit.Line != i+3 {
				continue
			}
			if edit.Kind == kind && edit.After == change {
				forced = edit
			}
			if edit.Kind == "condition-negation" {
				negated = edit
			}
		}
		if forced == nil || (forced.Site != nil) != valid || valid && forced.Operator.Mask() != mask {
			t.Errorf("%s: unexpected constant routing %+v", row, forced)
		}
		if !isReturn && (negated == nil || negated.Site == nil || negated.Operator != analysis.LE) {
			t.Errorf("%s: unexpected negation routing %+v", row, negated)
		}
	}
}

func TestTypedDiscoveryAndRewrite(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/shop\n\ngo 1.26.0\n")
	source := `package shop

type Yen int16
const fixed = 2 > 1

func free(yen Yen) bool { return 5000 <= yen }
func pair(left, right uint64) bool { return left > right }
func short(a bool, yen int) bool { return a && yen >
	2 }
func opaque(f func() int) bool { return f() > 0 }
func text(s string) bool { return s == "a" }
func sideEffectInReturn(x int, f func() int) (bool, int) { return x > 0, f() }
func notEvaluated(p *int) { for range [1]bool{*p > 0} {} }
func isConstant(p *int) int { return len([1]bool{*p > 0}) }
`
	write(t, dir, "shop.go", source)
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || len(pkgs[0].Files) != 1 || len(pkgs[0].Files[0].Sites) != 3 {
		t.Fatalf("unexpected discoveries: %#v", pkgs)
	}
	file := pkgs[0].Files[0]
	site := file.Sites[0]
	if site.Op != analysis.GE || site.Domain.Pivot != analysis.RankSigned(5000) ||
		site.Domain.Low != analysis.RankSigned(-32768) || site.Callable != "free" {
		t.Fatalf("wrong reversed signed site: %+v", site)
	}
	if file.Sites[1].Domain != nil {
		t.Fatal("two variable operands acquired a literal threshold")
	}
	for i, candidate := range file.Sites {
		candidate.ID = i
	}
	instrumented := string(Rewrite(file, pkgs[0].Prefix))
	if !strings.Contains(instrumented, "(0, 6, yen, 5000)") ||
		!strings.Contains(instrumented, "const fixed = 2 > 1") ||
		!strings.Contains(instrumented, "return f() > 0") {
		t.Fatalf("unexpected rewrite:\n%s", instrumented)
	}
	if strings.Count(source, "\n") != strings.Count(instrumented, "\n") {
		t.Fatal("rewrite changed source line offsets")
	}
	runtime := Runtime(pkgs[0])
	if len(runtime) != 1 || !strings.Contains(string(runtime[0].Data), "func "+pkgs[0].Prefix+"Compare") {
		t.Fatalf("missing runtime source: %+v", runtime)
	}
}

func write(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFullOnlyCreatesViableArithmeticKinds(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/full\n\ngo 1.26.0\n")
	write(t, dir, "full.go", `package full
func add(n int) int { return n + 1 }
func concat(s string) string { return s + "/" }
func zero(n int) int { return n * 0 }
func generic[T ~int](n T) T { return n + 1 }
func floatZero(n float64) float64 { return n * 0 }
func complexZero(n complex128) complex128 { return n * 0 }
func mixed[T ~int | ~string](n T) T { return n + n }
type S string
type C interface { int | string; int | S }
func intersection[T C](n T) T { return n + n }
func unionZero[T ~int | ~float64](n T) T { return n * 0 }
`)
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	edits := pkgs[0].Files[0].Edits
	want := map[int]string{2: "-", 5: "-", 6: "/", 7: "/", 11: "-", 12: "/"}
	for _, edit := range edits {
		if edit.Kind != "source-operator" {
			continue
		}
		if edit.Line == 3 || edit.Line == 8 || edit.Line == 4 && (edit.After == "/" || edit.After == "%") {
			t.Fatalf("invalid arithmetic edit: %+v", edit)
		}
		if want[edit.Line] == edit.After {
			delete(want, edit.Line)
		}
	}
	if len(want) > 0 {
		t.Fatalf("missing arithmetic edits: %v", want)
	}
}

func TestSourceCatalogue(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/catalogue\n\ngo 1.26.0\n")
	write(t, dir, "entry.go", `package catalogue
import "fmt"
const Limit = 0x10
var Flag = true
var Text = "default"
func operate(a, b int) int {
	a += b
	a++
	if a > b && a != 0 { a = b }
	for a < b { if a == 2 { break }; a++ }
	for range []int{1, 2} { a += b }
	switch a { case 1: a += 2; default: a-- }
	return a + b * 3
}
func masks(x, y uint) uint { x &= y; return x & y }
func fallback(err error) (int, error) {
	if err != nil { return 1, err }
	return 3, fmt.Errorf("%%w: %w", err)
}
type record struct { N int }
func value() record { return record{3} }
func generic[T any](x T) T { return x }
`)
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	edits := pkgs[0].Files[0].Edits
	for _, want := range []struct{ before, after string }{
		{"0x10", "17"}, {"0x10", "15"}, {"true", "false"}, {`"default"`, `""`},
		{"+=", "-="}, {"+=", "="}, {"++", "--"}, {">", "=="}, {"&&", "||"},
		{"a > b", "true"}, {"a < b", "false"}, {"break", "continue"},
		{"", " break;"}, {"&=", "|="}, {"&", "^"}, {"err", "nil"},
		{"%w", "%v"}, {"record{3}", "*new(record)"}, {"x", "*new(T)"},
	} {
		found := slices.ContainsFunc(edits, func(edit Edit) bool { return edit.Before == want.before && edit.After == want.after })
		if !found {
			t.Errorf("missing %q → %q", want.before, want.after)
		}
	}
	if n := len(edits); n < 60 {
		t.Fatalf("expected broad catalogue, got %d", n)
	}
}

func TestSourceEditPICT(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/source-edits.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] {
		t.Run(strings.ReplaceAll(row, "\t", "/"), func(t *testing.T) {
			fields := strings.Fields(row)
			dir := t.TempDir()
			path := filepath.Join(dir, "file.go")
			write(t, dir, "go.mod", "module example.org/pict\n\ngo 1.26.0\n")
			header := map[string]string{
				"plain":   "func fn(x int) int ",
				"named":   "type N int\nfunc fn(x N) N ",
				"generic": "func fn[T ~int](x T) T ",
			}[fields[2]]
			statement, before, after := "{ return x + x }", "+", "-"
			switch fields[0] {
			case "literal":
				statement, before, after = "{ return x + 3 }", "3", "4"
			case "return":
				statement, before, after = "{ return x }", "x", "0"
				if fields[2] == "generic" {
					after = "*new(T)"
				}
			}
			source := "package p\n" + header + statement + "\n"
			write(t, dir, "file.go", source)
			var scope Scope
			if fields[1] == "excluded" {
				scope = Scope{path: {{Start: 1, End: 1}}}
			}
			pkgs, err := Discover(context.Background(), dir, []string{"."}, "", scope, Discovery{Full: true})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, file := range pkgs[0].Files {
				found = found || slices.ContainsFunc(file.Edits, func(e Edit) bool { return e.Before == before && e.After == after })
			}
			if found != (fields[1] == "included") {
				t.Fatalf("found %q → %q: %v", before, after, found)
			}
		})
	}
}

func TestSourceParityPreservesLexicalAndTypeSemantics(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/edges\n\ngo 1.26.0\n")
	write(t, dir, "edge.go", `package edges
//line remapped.go:700
func compact(a,b int) int { return a+-b }
func priority(a,b,c int) int { return a-b*c }
func boxed() any { return 42 }
type E struct{}
func (*E) Error() string { return "" }
func check(e *E) []bool { return []bool{e!=nil} }
func sign(x int) int { return -+x }
`)
	path := filepath.Join(dir, "edge.go")
	scope := Scope{path: {{Start: 3, End: 9}}}
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", scope, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[[3]string]bool{
		{"3", "a+-b", "((a) - (-b))"}:   false,
		{"4", "a-b*c", "((a) / (b*c))"}: false,
		{"4", "-", "/"}:                 false,
		{"5", "42", "0"}:                false,
		{"9", "-", " + "}:               false,
		{"8", "e!=nil", "false"}:        false,
	}
	for _, file := range pkgs[0].Files {
		for _, edit := range file.Edits {
			key := [3]string{strconv.Itoa(edit.Line), edit.Before, edit.After}
			if _, ok := want[key]; ok {
				want[key] = true
			}
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("missing %v", key)
		}
	}
}

func TestSourceOnlyCanMutateGeneratedGo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/generated\n\ngo 1.26.0\n")
	write(t, dir, "computed.go", "// Code generated by fixtures. DO NOT EDIT.\npackage generated\nfunc allowed(x int) bool { return x >= 42 }\n")
	for _, include := range []bool{false, true} {
		pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true, SourceOnly: true, IncludeGenerated: include})
		if err != nil {
			t.Fatal(err)
		}
		if (len(pkgs[0].Files) != 0) != include {
			t.Fatalf("generated option %v: %v", include, pkgs)
		}
		if include {
			file := pkgs[0].Files[0]
			if len(file.Sites) != 0 || !slices.ContainsFunc(file.Edits, func(e Edit) bool { return e.Before == ">=" && e.After == ">" }) {
				t.Fatalf("missing source-only boundary: %+v", file)
			}
		}
	}
}

func TestFullDiscoversOriginalCgoSource(t *testing.T) {
	enabled, err := exec.Command("go", "env", "CGO_ENABLED").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(enabled)) != "1" {
		t.Skip("cgo is disabled")
	}
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/cgo\n\ngo 1.26.0\n")
	write(t, dir, "bridge.go", `package bridge
/* static int base() { return 1; } */
import "C"
func Allowed(n int) bool { return n >= int(C.base()) }
`)
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range pkgs[0].Files {
		if filepath.Base(file.Path) == "bridge.go" &&
			slices.ContainsFunc(file.Edits, func(e Edit) bool { return e.Before == ">=" && e.After == ">" }) {
			return
		}
	}
	t.Fatal("missing source-level cgo mutation")
}
