package target

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"go/parser"
	"go/token"
	"math"
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
	var comparisons int
	for _, s := range file.Sites {
		if !s.IsSchemata() {
			comparisons++
		}
	}
	if comparisons != 2 {
		t.Fatalf("wanted both comparison sites, got %d (total sites %d)", comparisons, len(file.Sites))
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
			change = "(false)"
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
	if file.Sites[1].Domain != nil || !file.Sites[1].Margin {
		t.Fatal("two variable operands should set Margin without a literal threshold")
	}
	if file.Sites[2].StartLine >= file.Sites[2].EndLine {
		t.Fatalf("expected multi-line site span, got %d..%d", file.Sites[2].StartLine, file.Sites[2].EndLine)
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

func TestRuntimeInitOrderAndProbeExecution(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/initorder\n\ngo 1.18\n")
	write(t, dir, "a.go", `package initorder
var log []string
var first = record("first", 10, 5)
func record(tag string, x, y int) int {
	if x > y {
		log = append(log, tag)
	}
	return x
}
func CheckFloat(a, b float64) bool { return a < b }
`)
	write(t, dir, "000000_early.go", `package initorder
var zero = record("zero", 2, 1)
`)
	write(t, dir, "initorder_test.go", `package initorder
import (
	"math"
	"reflect"
	"testing"
)
func TestInitOrderAndFloat(t *testing.T) {
	if !reflect.DeepEqual(log, []string{"zero", "first"}) {
		t.Fatalf("var init order broken: %v", log)
	}
	if !CheckFloat(1.5, 2.5) || CheckFloat(math.NaN(), 1.0) {
		t.Fatal("float comparison failed")
	}
}
`)
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{})
	if err != nil {
		t.Fatal(err)
	}
	rt := Runtime(pkgs[0])
	if len(rt) != 1 {
		t.Fatalf("expected 1 runtime file, got %d", len(rt))
	}
	rtBase := filepath.Base(rt[0].Path)
	if rtBase >= "000000_early.go" || rtBase >= "a.go" {
		t.Fatalf("runtime filename %q does not precede package files", rtBase)
	}
	rtSrc := string(rt[0].Data)
	if strings.Contains(rtSrc, "OrUint64") || strings.Contains(rtSrc, "var "+pkgs[0].Prefix+"StateValue =") {
		t.Fatalf("runtime violates Go 1.18 compatibility or var init order")
	}

	// Materialize instrumented files + runtime into a temp copy and run `go vet` and `go test`.
	runDir := t.TempDir()
	write(t, runDir, "go.mod", "module example.org/initorder\n\ngo 1.18\n")
	for _, name := range []string{"000000_early.go", "a.go", "initorder_test.go"} {
		copyData, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		write(t, runDir, name, string(copyData))
	}
	siteID := 0
	for _, f := range pkgs[0].Files {
		for _, s := range f.Sites {
			s.ID = siteID
			siteID++
		}
		write(t, runDir, filepath.Base(f.Path), string(Rewrite(f, pkgs[0].Prefix)))
	}
	// Re-generate runtime after setting site IDs, and ensure ProbeFloat is included.
	rt = Runtime(pkgs[0])
	write(t, runDir, filepath.Base(rt[0].Path), string(rt[0].Data))

	vet := exec.Command("go", "vet", "./...")
	vet.Dir = runDir
	if out, err := vet.CombinedOutput(); err != nil {
		t.Fatalf("go vet failed: %v\n%s", err, out)
	}

	baseEnv := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, "TMT_GO_TRACE=") || strings.HasPrefix(entry, "TMT_GO_MUTATION=")
	})
	tracePath := filepath.Join(t.TempDir(), "trace.bin")
	if err := os.WriteFile(tracePath, make([]byte, siteID*BytesPerSite), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-count=1", ".")
	cmd.Dir = runDir
	cmd.Env = append(slices.Clone(baseEnv), "TMT_GO_TRACE="+tracePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("instrumented test failed: %v\n%s", err, out)
	}
	trace, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	// Check site 1 (CheckFloat): Less (1) and Unordered (32) recorded in slot[0].
	var floatSiteID int
	for _, f := range pkgs[0].Files {
		for _, s := range f.Sites {
			if s.Float {
				floatSiteID = s.ID
			}
		}
	}
	flags := binary.LittleEndian.Uint64(trace[floatSiteID*BytesPerSite:])
	if flags&(analysis.Less|analysis.Unordered) != (analysis.Less | analysis.Unordered) {
		t.Fatalf("expected float Less|Unordered flags, got %d", flags)
	}

	// Check margin-down on site 0 (`record("first", 10, 5)` has x=10, y=5 -> margin-down mutates y to 4, still true; check margin-up/down on a margin test).
	var recordSiteID int
	for _, f := range pkgs[0].Files {
		if filepath.Base(f.Path) == "a.go" {
			for _, s := range f.Sites {
				if !s.Float {
					recordSiteID = s.ID
				}
			}
		}
	}
	// Check gap recorded at slot[4] for x=10 > y=5 (gap = 5) and x=2 > y=1 (gap = 1).
	recordSlot := trace[recordSiteID*BytesPerSite : (recordSiteID+1)*BytesPerSite]
	aboveGap := ^binary.LittleEndian.Uint64(recordSlot[32:40])
	if aboveGap != 1 {
		t.Fatalf("expected AboveGap=1, got %d", aboveGap)
	}

	// Exercise margin-down mutation on CheckFloat? No, exercise margin-up on recordSiteID: 2 > 1+1 is false -> breaks `zero`!
	mutTrace := filepath.Join(t.TempDir(), "mut.bin")
	if err := os.WriteFile(mutTrace, make([]byte, siteID*BytesPerSite), 0600); err != nil {
		t.Fatal(err)
	}
	mutCmd := exec.Command("go", "test", "-count=1", ".")
	mutCmd.Dir = runDir
	mutCmd.Env = append(slices.Clone(baseEnv),
		"TMT_GO_TRACE="+mutTrace,
		fmt.Sprintf("TMT_GO_MUTATION=%d:%d:0:margin-up", recordSiteID, analysis.GT.Mask()),
	)
	if out, err := mutCmd.CombinedOutput(); err == nil {
		t.Fatalf("expected margin-up mutation to kill test, but passed:\n%s", out)
	}
	mutData, err := os.ReadFile(mutTrace)
	if err != nil {
		t.Fatal(err)
	}
	if mutFlags := binary.LittleEndian.Uint64(mutData[recordSiteID*BytesPerSite:]); mutFlags&24 != 24 {
		t.Fatalf("expected Activated|Changed (24) in mutated trace, got %d", mutFlags)
	}
	_ = bytes.Equal
	_ = math.NaN
}

func TestSyntheticProbeFileNameOrdering(t *testing.T) {
	for _, paths := range [][]string{
		{"a.go", "z.go"},
		{"000000_a.go"},
		{"0.go"},
		{"0-.go"},
		{"0!.go"},
	} {
		name, ok := syntheticProbeFileName("tmtGo123456", paths)
		if !ok || name == "" || name[0] < '0' || name[0] > '9' {
			t.Fatalf("synthetic name %q (ok=%v) must start with an ASCII digit for cmd/go SafeArg", name, ok)
		}
		for _, p := range paths {
			if filepath.Base(p) <= name {
				t.Fatalf("synthetic name %q does not precede %q", name, p)
			}
		}
	}
}

func TestFloatAndLenCapAndBlankFunc(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/features\n\ngo 1.26.0\n")
	write(t, dir, "features.go", `package features
type MyFloat float64
func FloatCmp(a, b MyFloat) bool {
	if a <= 0 {
		return false
	}
	return a < b
}
func SliceCheck(xs []int, f func() []int) bool {
	if len(f()) > 0 {
		return false
	}
	return len(xs) > 0 && cap(xs) >= 4
}
func _(x int) bool { return x > 0 }
`)
	// Default mode (Full: false) should still discover float probes and len/cap!
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{})
	if err != nil {
		t.Fatal(err)
	}
	sites := pkgs[0].Files[0].Sites
	if len(sites) != 5 {
		t.Fatalf("got %d sites, want 5: %+v", len(sites), sites)
	}
	if !sites[0].Float || !sites[1].Float {
		t.Fatalf("expected float sites in default mode: %+v", sites[:2])
	}
	if sites[2].Expression != "len(xs) > 0" || sites[3].Expression != "cap(xs) >= 4" {
		t.Fatalf("unexpected len/cap sites: %+v", sites[2:4])
	}
	if sites[4].Callable != "" {
		t.Fatalf("blank function should not get Callable, got %q", sites[4].Callable)
	}

	// Full mode: float source-operator edits route to site (excluding boundary/negation),
	// and condition-negation on ordered float does NOT route to site.
	fullPkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	var routedFloatOps, floatCondNeg int
	for _, e := range fullPkgs[0].Files[0].Edits {
		if e.Kind == "source-operator" && e.Site != nil && e.Site.Float {
			routedFloatOps++
		}
		if e.Kind == "condition-negation" && e.Line == 4 && e.Site != nil {
			floatCondNeg++
		}
	}
	if routedFloatOps == 0 {
		t.Fatal("expected float source-operator edits routed to site")
	}
	if floatCondNeg != 0 {
		t.Fatal("ordered float condition-negation must not route to site (NaN semantics differ)")
	}
}

func TestParseIgnoreComments(t *testing.T) {
	src := `package ign
//line virtual.go:999
func f(x int) bool {
	if x > 0 { // tmt:ignore: intentional
		return true
	}
	// tmt:ignore: multi-line explanation
	// still explaining
	if x < -1 {
		return false
	}
	// tmt:ignore

	if x == 0 {
		return true
	}
	/* tmt:ignore: block comment */
	return x != 5
	// tmt:ignored not a directive
}
`
	fset := token.NewFileSet()
	syntax, err := parser.ParseFile(fset, "ign.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	ignored := ParseIgnoreComments(fset, syntax, []byte(src))
	if got := ignored[4]; got != "intentional" {
		t.Fatalf("line 4 ignored = %q, want %q", got, "intentional")
	}
	if got := ignored[9]; got != "multi-line explanation" {
		t.Fatalf("line 9 ignored = %q, want %q", got, "multi-line explanation")
	}
	if _, ok := ignored[14]; ok {
		t.Fatalf("blank line should prevent tmt:ignore from ignoring line 14: %+v", ignored)
	}
	if got := ignored[18]; got != "block comment" {
		t.Fatalf("line 18 ignored = %q, want %q", got, "block comment")
	}
	if _, ok := ignored[19]; ok {
		t.Fatalf("tmt:ignored should not match: %+v", ignored)
	}
}

func TestTargetBoundaryConditions(t *testing.T) {
	// 1. followsIdentifierByte exact ASCII and UTF-8 boundaries.
	for _, tc := range []struct {
		b    byte
		want bool
	}{
		{'`', false}, {'a', true}, {'z', true}, {'{', false},
		{'@', false}, {'A', true}, {'Z', true}, {'[', false},
		{'/', false}, {'0', true}, {'9', true}, {':', false},
		{'_', true}, {0x7F, false}, {0x80, true},
	} {
		buf := []byte{tc.b, 'x'}
		if got := followsIdentifierByte(buf, 1); got != tc.want {
			t.Errorf("followsIdentifierByte(%#x) = %v, want %v", tc.b, got, tc.want)
		}
	}
	if followsIdentifierByte([]byte("a"), 0) {
		t.Fatal("start==0 must be false")
	}

	// 2. versionSupportsProbes boundaries.
	for _, tc := range []struct {
		v    string
		want bool
	}{
		{"0.9", false}, {"1.17", false}, {"1.18", true}, {"1.22", true}, {"2.0", true},
	} {
		if got := versionSupportsProbes(tc.v); got != tc.want {
			t.Errorf("versionSupportsProbes(%q) = %v, want %v", tc.v, got, tc.want)
		}
	}

	// 3. ParseIgnoreComments at offset 0, 1-space indent, and EOF without trailing newline.
	eofSrc := "// tmt:ignore: top\n // tmt:ignore: indented"
	fset := token.NewFileSet()
	eofAst, err := parser.ParseFile(fset, "eof.go", "package p\n"+eofSrc, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	eofIgnored := ParseIgnoreComments(fset, eofAst, []byte("package p\n"+eofSrc))
	if eofIgnored[2] != "top" || eofIgnored[3] != "indented" {
		t.Fatalf("unexpected EOF ignore map: %+v", eofIgnored)
	}
	if !isLineWhitespaceBefore([]byte("//"), 0) || isLineWhitespaceBefore([]byte("a"), -1) || isLineWhitespaceBefore([]byte("a"), 2) {
		t.Fatal("isLineWhitespaceBefore bounds failed")
	}
	if !isLineWhitespaceAfter([]byte("//"), 2) || isLineWhitespaceAfter([]byte("a"), -1) || isLineWhitespaceAfter([]byte("a"), 2) {
		t.Fatal("isLineWhitespaceAfter bounds failed")
	}

	// 4. Struct tags not mutated, min-bound == "" / != "" / == 0 equivalents omitted, slice-element-remove 8 vs 9, empty block/case, raw `%w`.
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/tbounds\n\ngo 1.26.0\n")
	write(t, dir, "bounds.go", "package tbounds\n"+
		"import (\n\t\"fmt\"\n\t\"slices\"\n)\n"+
		"type Doc struct {\n\tName string `json:\"name\"`\n}\n"+
		"func Check(s string, u uint, xs []int, b uint8, i8 int8) error {\n"+
		"\tif s == \"\" || s != \"\" || u == 0 || u != 0 || len(xs) == 0 || len(xs) != 0 || len(xs) > 0 || len(xs) <= 0 || i8 == -128 {}\n"+
		"\tif b == 255 || b != 255 || b < 255 || b >= 255 || i8 == 127 {}\n"+
		"\tswitch u { case 1: }\n"+
		"\t_ = slices.Contains([]int{1, 2, 3, 4, 5, 6, 7, 8}, 1)\n"+
		"\t_ = slices.Contains([]int{1, 2, 3, 4, 5, 6, 7, 8, 9}, 1)\n"+
		"\treturn fmt.Errorf(`%w`, nil)\n"+
		"}\n")
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, Discovery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	edits := pkgs[0].Files[0].Edits
	sliceRemovals := 0
	foundUnwrap := false
	for _, e := range edits {
		if e.Kind == "string-literal" && strings.Contains(e.Before, "json:") {
			t.Fatalf("struct tag should not be mutated by string-literal: %+v", e)
		}
		if e.Kind == "source-operator" && e.Line == 10 &&
			((e.Before == "==" && e.After == "<=") || (e.Before == "!=" && e.After == ">") ||
				(e.Before == ">" && e.After == "!=") || (e.Before == "<=" && e.After == "==")) {
			t.Fatalf("min-bound equivalent operator swap %s -> %s should be omitted: %+v", e.Before, e.After, e)
		}
		if e.Kind == "source-operator" && e.Line == 11 &&
			((e.Before == "==" && e.After == ">=") || (e.Before == "!=" && e.After == "<") ||
				(e.Before == "<" && e.After == "!=") || (e.Before == ">=" && e.After == "==")) {
			t.Fatalf("max-bound equivalent operator swap %s -> %s should be omitted: %+v", e.Before, e.After, e)
		}
		if e.Kind == "empty-block" && (e.Line == 10 || e.Line == 11) {
			t.Fatalf("already-empty block must not emit empty-block: %+v", e)
		}
		if e.Kind == "empty-case" && e.Line == 12 {
			t.Fatalf("already-empty case must not emit empty-case: %+v", e)
		}
		if e.Kind == "slice-element-remove" {
			sliceRemovals++
		}
		if e.Kind == "errorf-unwrap" {
			foundUnwrap = true
		}
	}
	if sliceRemovals != 8 {
		t.Fatalf("expected 8 slice-element-remove edits (from 8-element slice only, none from 9-element), got %d", sliceRemovals)
	}
	if !foundUnwrap {
		t.Fatal("expected errorf-unwrap on raw `%w` literal")
	}
}

func TestWindowsProbeRuntimeCrossCompiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.org/winprobe\n\ngo 1.18\n")
	pkg := Package{
		Name:   "winprobe",
		Dir:    dir,
		Prefix: "tmtGoWin",
		Paths:  []string{filepath.Join(dir, "sample.go")},
		Files: []*File{{
			Path: filepath.Join(dir, "sample.go"),
			Sites: []*Site{
				{ID: 0},
				{ID: 1, String: true},
				{ID: 2, Float: true},
				{ID: 3, Arith: true, ArithOp: 1},
				{ID: 4, Bool: true},
			},
		}},
	}
	rt := runtimeForGOOS(pkg, "windows")
	if len(rt) != 1 {
		t.Fatalf("expected 1 runtime file, got %d", len(rt))
	}
	if !strings.Contains(string(rt[0].Data), "CreateFileMapping") || !strings.Contains(string(rt[0].Data), "MapViewOfFile") {
		t.Fatalf("windows runtime missing CreateFileMapping/MapViewOfFile")
	}
	write(t, dir, filepath.Base(rt[0].Path), string(rt[0].Data))
	write(t, dir, "sample.go", "package winprobe\nfunc Touch() { _ = tmtGoWinCompare(0, 1, 1, 2) }\n")

	cmd := exec.Command("go", "vet", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("windows probe runtime cross-vet failed: %v\n%s", err, out)
	}
	buildCmd := exec.Command("go", "build", ".")
	buildCmd.Dir = dir
	buildCmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("windows probe runtime cross-compile failed: %v\n%s", err, out)
	}
}
