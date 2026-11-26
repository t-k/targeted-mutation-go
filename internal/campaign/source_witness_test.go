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
