package campaign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

func TestSourceWitnessPICT(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/source-witness.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] {
		fields := strings.Fields(row)
		t.Run(strings.Join(fields, "/"), func(t *testing.T) {
			exerciseSourceWitness(t, fields[0], fields[1], fields[2])
		})
	}
}

func exerciseSourceWitness(t *testing.T, family, arguments, mutation string) {
	t.Helper()
	var imports, args, result, original, changed string
	switch family {
	case "integer":
		args, result, original, changed = "x int", "int", "x * 2 / 3", "x / 3 * 2"
		if arguments == "two" {
			args, original, changed = "x, y int", "x * y / 3", "x / 3 * y"
		}
	case "string":
		imports = "import \"strings\"\n"
		args, result, original, changed = "x string", "bool", `strings.HasPrefix(x, "needle")`, `strings.Contains(x, "needle")`
		if arguments == "two" {
			args, original, changed = "x, y string", "strings.HasPrefix(x, y)", "strings.Contains(x, y)"
		}
	case "boolean":
		args, result, original, changed = "x bool", "bool", "x", "!x"
		if arguments == "two" {
			args, original, changed = "x, y bool", "x && y", "x || y"
		}
	case "float":
		imports = "import \"math\"\n"
		args, result, original, changed = "x float64", "float64", "math.Floor(x)", "math.Ceil(x)"
		if arguments == "two" {
			args, original, changed = "x, y float64", "math.Floor(x + y)", "math.Ceil(x + y)"
		}
	}
	switch mutation {
	case "equivalent":
		changed = "(" + original + ")"
	case "panic":
		changed = `func() ` + result + ` { panic("witness-looking output"); return ` + original + ` }()`
	case "build-failure":
		changed = "tmtUndefined + (" + original + ")"
	}
	dir := t.TempDir()
	originalSource := "package witness\n" + imports + "func target(" + args + ") " + result + " { return " + original + " }\n"
	fixture(t, dir, "go.mod", "module example.org/sourcewitness\n\ngo 1.26.0\n")
	fixture(t, dir, "witness.go", originalSource)
	path := filepath.Join(dir, "witness.go")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	env, err := environment(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(originalSource, "return ") + len("return ")
	s := session{cfg: Config{Dir: dir, Jobs: 1, Timeout: 5 * time.Second, BuildTimeout: time.Minute}, env: env,
		scratch: t.TempDir(), packages: []target.Package{{Path: "example.org/sourcewitness", Name: "witness", Dir: dir, Paths: []string{path}}}}
	job := sourceJob{source: []byte(originalSource), edit: &target.Edit{File: path, Start: start, End: start + len(original), Before: original, After: changed}}
	witness, err := s.sourceWitness(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if mutation != "different" {
		if witness != nil {
			t.Fatalf("uncertified witness %+v", witness)
		}
		return
	}
	if witness == nil || !witness.Verified || witness.Original == witness.Mutated ||
		!strings.HasPrefix(witness.Call, "target(") || !strings.Contains(witness.Code, "got != "+witness.Original) {
		t.Fatalf("missing verified, copyable witness: %+v", witness)
	}
	currentSource, err := os.ReadFile(path)
	if err != nil || string(currentSource) != originalSource {
		t.Fatalf("source changed: %v, %s", err, currentSource)
	}
}

func TestSourceWitnessShadowedIdentifiers(t *testing.T) {
	dir := t.TempDir()
	originalSource := `package witness
var testing = 1
var true = 1
var false = 2
func t(x int) int { return x * 2 / 3 }
`
	fixture(t, dir, "go.mod", "module example.org/shadow\n\ngo 1.26.0\n")
	fixture(t, dir, "shadow.go", originalSource)
	path := filepath.Join(dir, "shadow.go")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	env, err := environment(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(originalSource, "x * 2 / 3")
	s := session{cfg: Config{Dir: dir, Jobs: 1, Timeout: 5 * time.Second, BuildTimeout: time.Minute}, env: env,
		scratch: t.TempDir(), packages: []target.Package{{Path: "example.org/shadow", Name: "witness", Dir: dir, Paths: []string{path}}}}
	job := sourceJob{source: []byte(originalSource), edit: &target.Edit{File: path, Start: start, End: start + len("x * 2 / 3"), Before: "x * 2 / 3", After: "x / 3 * 2"}}
	witness, err := s.sourceWitness(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if witness == nil || !witness.Verified || !strings.Contains(witness.Code, "tmtWitnessTesting") || !strings.Contains(witness.Code, "tmtT.Fatalf") {
		t.Fatalf("expected verified collision-free witness, got %+v", witness)
	}
}

func TestSourceWitnessRejectsSideEffects(t *testing.T) {
	cases := []struct {
		name   string
		source string
		wantOK bool
	}{
		{
			name:   "imports_os",
			source: "package side\nimport \"os\"\nfunc f(x int) int { _ = os.PathSeparator; return x * 2 / 3 }\n",
		},
		{
			name:   "imports_net_http",
			source: "package side\nimport \"net/http\"\nfunc f(x int) int { _ = http.MethodGet; return x * 2 / 3 }\n",
		},
		{
			name:   "imports_unsafe",
			source: "package side\nimport \"unsafe\"\nfunc f(x int) int { _ = unsafe.Sizeof(x); return x * 2 / 3 }\n",
		},
		{
			name:   "mutates_global_assign",
			source: "package side\nvar counter int\nfunc f(x int) int { counter = x; return x * 2 / 3 }\n",
		},
		{
			name:   "mutates_global_incdec",
			source: "package side\nvar counter int\nfunc f(x int) int { counter++; return x * 2 / 3 }\n",
		},
		{
			name:   "channel_send",
			source: "package side\nfunc f(x int) int { ch := make(chan int, 1); ch <- x; return x * 2 / 3 }\n",
		},
		{
			name:   "go_stmt",
			source: "package side\nfunc f(x int) int { go func() {}(); return x * 2 / 3 }\n",
		},
		{
			name:   "pure_with_filepath_and_local_mutation",
			source: "package side\nimport \"path/filepath\"\nfunc f(x int) int { _ = filepath.Base(\"a/b\"); y := x; y++; _ = y; return x * 2 / 3 }\n",
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := strings.Index(tc.source, "x * 2 / 3")
			job := sourceJob{
				source: []byte(tc.source),
				edit:   &target.Edit{File: "side.go", Start: start, End: start + len("x * 2 / 3"), Before: "x * 2 / 3", After: "x / 3 * 2"},
			}
			calls := sourceWitnessCalls(job)
			if (len(calls) > 0) != tc.wantOK {
				t.Fatalf("calls = %v, wantOK = %t", calls, tc.wantOK)
			}
		})
	}
}

func TestSourceWitnessLargeLiteralNeighborsAndTempCwd(t *testing.T) {
	dir := t.TempDir()
	originalSource := `package fee
func Fee(cents, bps int) int { return (cents * bps) / 10000 }
`
	fixture(t, dir, "go.mod", "module example.org/fee\n\ngo 1.26.0\n")
	fixture(t, dir, "fee.go", originalSource)
	path := filepath.Join(dir, "fee.go")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	env, err := environment(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(originalSource, "(cents * bps) / 10000")
	s := session{cfg: Config{Dir: dir, Jobs: 1, Timeout: 5 * time.Second, BuildTimeout: time.Minute}, env: env,
		scratch: t.TempDir(), packages: []target.Package{{Path: "example.org/fee", Name: "fee", Dir: dir, Paths: []string{path}}}}
	job := sourceJob{source: []byte(originalSource), edit: &target.Edit{
		File: path, Start: start, End: start + len("(cents * bps) / 10000"),
		Before: "(cents * bps) / 10000", After: "(cents / 10000) * bps",
	}}
	witness, err := s.sourceWitness(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if witness == nil || !witness.Verified || witness.Original == witness.Mutated {
		t.Fatalf("expected verified witness for 10000 scaling, got %+v", witness)
	}

	// Verify runSourceWitness executes in a fresh temp cwd rather than pkg.Dir.
	trial := sourceWitnessTrial{
		test:   "TestTmtCwdCheck",
		file:   filepath.Join(dir, "tmt_cwd_check_test.go"),
		marker: "cwd-check",
		pkg:    target.Package{Path: "example.org/fee", Name: "fee", Dir: dir, Paths: []string{path}},
	}
	generated := `package fee
import (
	"os"
	"testing"
)
func TestTmtCwdCheck(t *testing.T) {
	if err := os.WriteFile("cwd_sentinel.txt", []byte("touched"), 0600); err != nil {
		t.Fatal(err)
	}
}
`
	run, _, err := s.runSourceWitness(ctx, s.scratch, trial, target.Replacement{Path: path, Data: []byte(originalSource)}, generated)
	if err != nil || run.kind != "passed" {
		t.Fatalf("runSourceWitness failed: %+v, %v", run, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cwd_sentinel.txt")); !os.IsNotExist(err) {
		t.Fatalf("runSourceWitness wrote into package dir instead of temp cwd: %v", err)
	}
}
