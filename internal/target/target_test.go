package target

import (
	"context"
	"os"
	"path/filepath"
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
`)
	pkgs, err := Discover(context.Background(), dir, []string{"."}, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	edits := pkgs[0].Files[0].Edits
	if len(edits) != 4 || edits[0].Line != 2 || edits[0].After != "-" ||
		edits[1].Line != 5 || edits[1].After != "-" || edits[2].Line != 6 || edits[2].After != "/" ||
		edits[3].Line != 7 || edits[3].After != "/" {
		t.Fatalf("unbuildable arithmetic candidates: %+v", edits)
	}
}
