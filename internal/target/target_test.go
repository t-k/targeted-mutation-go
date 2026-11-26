package target

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
)

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
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, false)
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
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, true)
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
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, true)
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
			pkgs, err := Discover(context.Background(), dir, []string{"."}, "", scope, true)
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
