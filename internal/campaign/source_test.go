package campaign

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

func TestSourceComparisonDoesNotAcceptProbeIOErrorAsSurvival(t *testing.T) {
	site := target.Site{Op: analysis.EQ}
	s := session{scratch: filepath.Join(t.TempDir(), "absent"), sites: []*target.Site{&site}}
	job := sourceJob{edit: &target.Edit{Site: &site, Operator: analysis.NE}}
	reached := []*consumer{{seen: map[int]analysis.Summary{0: {Flags: analysis.Equal}}}}
	r := s.runSourceComparison(context.Background(), job, reached, commandResult{})
	if r.err == nil {
		t.Fatalf("accepted a replay whose trace could not be opened: %+v", r)
	}
}

func TestUnusedRepairIsBoundToTheEditedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source-0.go")
	source := []byte("package p\nimport f \"fmt\"\nfunc run() { if x:=3; true {} }\n")
	diags := []compilerDiagnostic{
		{path: path, line: 2, column: 8, message: `"fmt" imported as f and not used`},
		{path: path, line: 3, column: 17, message: "declared and not used: x"},
	}
	repair := repairUnused(source, path, diags)
	if !bytes.Contains(repair, []byte(`import _ "fmt"`)) ||
		!bytes.Contains(repair, []byte("{ _ = &x;}")) {
		t.Fatalf("unexpected repair: %s", repair)
	}
	wrongImport := []compilerDiagnostic{{path: path, line: 2, column: 8, message: `"os" imported as f and not used`}}
	if repairUnused(source, path, wrongImport) != nil {
		t.Fatal("diagnostic targeted the wrong import")
	}
	diags[1].path = filepath.Join(t.TempDir(), "other-source-0.go")
	if repairUnused(source, path, diags) != nil {
		t.Fatal("repair crossed overlay boundaries")
	}
}

func TestSourceJobsShareTheUnmodifiedFile(t *testing.T) {
	text := []byte("ab c abc")
	file := &target.File{Path: "/test/file.go", Data: text, Edits: []target.Edit{
		{File: "/test/file.go", Start: 0, End: 2, Before: "ab", After: "X"},
		{File: "/test/file.go", Start: 5, End: 8, Before: "abc", After: "Y"},
	}}
	s := session{cfg: Config{Dir: "/test"}, packages: []target.Package{{Files: []*target.File{file}}}}
	s.collect(nil)
	for i, expected := range []string{"X c abc", "ab c Y"} {
		job := s.sources[i]
		if &job.source[0] != &text[0] || job.replacement.Data != nil {
			t.Fatal("planner copied the file")
		}
		if got := job.materialize(); string(got.Data) != expected || got.Path != file.Path {
			t.Fatalf("wrong overlay: %+v", got)
		}
	}
	if string(text) != "ab c abc" {
		t.Fatal("original snapshot changed")
	}
}

func BenchmarkSourceQueue(b *testing.B) {
	const jobs = 1024
	path := "/test/file.go"
	file := &target.File{Path: path, Data: bytes.Repeat([]byte("x"), 64*1024), Edits: make([]target.Edit, jobs)}
	for i := range file.Edits {
		file.Edits[i] = target.Edit{File: path, Start: i, End: i + 1, Before: "x", After: "y"}
	}
	b.Run("eager", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			queue := make([]target.Replacement, 0, jobs)
			for i := range file.Edits {
				job := sourceJob{source: file.Data, edit: &file.Edits[i]}
				queue = append(queue, job.materialize())
			}
			runtime.KeepAlive(queue)
		}
	})
	b.Run("shared", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			s := session{cfg: Config{Dir: "/test"}, packages: []target.Package{{Files: []*target.File{file}}}}
			s.collect(nil)
			runtime.KeepAlive(s.sources)
		}
	})
}

func TestRepairSelectCaseLocalAfterDeclaration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source-0.go")
	data := []byte("package p\nfunc f(ch chan int) int { select { case <-ch: x := 42; return 0 } }\n")
	column := strings.Index(strings.Split(string(data), "\n")[1], "x :=") + 1
	repair := repairUnused(data, path, []compilerDiagnostic{{path: path, line: 2, column: column, message: "declared and not used: x"}})
	if !strings.Contains(string(repair), "x := 42; _ = &x; return 0") {
		t.Fatalf("declaration was used before introduction: %s", repair)
	}
}

func TestCompilerDiagnosticsSurviveLargeOutput(t *testing.T) {
	var output commandOutput
	_, _ = output.Write([]byte(`{"Action":"build-output","Output":"./tmp/source-0.go:4:6: missing return\n"}` + "\n"))
	_, _ = output.Write([]byte(strings.Repeat("irrelevant output\n", 5000)))
	if len(output.diagnostics) != 1 || output.diagnostics[0].message != "missing return" {
		t.Fatalf("missing diagnostic: %+v", output.diagnostics)
	}
}

func TestUnusedVersionedImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source-0.go")
	data := []byte("package p\nimport \"math/rand/v2\"\nfunc f() int { return 0 }\n")
	diag := compilerDiagnostic{path: path, line: 2, column: 8, message: `"math/rand/v2" imported as rand and not used`}
	if repair := repairUnused(data, path, []compilerDiagnostic{diag}); !strings.Contains(string(repair), `import _ "math/rand/v2"`) {
		t.Fatalf("implicit import should be retained for initialization: %s", repair)
	}
}

func TestCandidateDiagnosticsRejectUnrelatedFailedBuild(t *testing.T) {
	var output commandOutput
	_, _ = output.Write([]byte(`{"ImportPath":"example.org/edited","Action":"build-output","Output":"/tmp/overlay/source-0.go:4:6: missing return\n"}
{"ImportPath":"example.org/edited","Action":"build-fail"}
{"ImportPath":"example.org/other","Action":"build-output","Output":"link: running cc failed\n"}
{"ImportPath":"example.org/other","Action":"build-fail"}
`))
	run := commandResult{diagnostics: output.diagnostics, failedBuilds: output.failedBuilds}
	if diagnostics := candidateDiagnostics(run, "/tmp", "/tmp/overlay/source-0.go"); len(diagnostics) != 0 {
		t.Fatalf("candidate errors masked an unrelated build failure: %+v", diagnostics)
	}
}
