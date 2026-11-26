package campaign

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

func TestUnusedRepairIsBoundToTheEditedFile(t *testing.T) {
	const path = "/tmp/private-overlay/source-0.go"
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
	diags[1].path = "/tmp/other/source-0.go"
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
