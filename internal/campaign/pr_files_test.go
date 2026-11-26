package campaign

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func prRows(t *testing.T) [][]string {
	t.Helper()
	table, err := os.ReadFile("../../verification/pict/pr-files.tsv")
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]string
	for _, row := range strings.Split(strings.TrimSpace(string(table)), "\n")[1:] {
		rows = append(rows, strings.Split(strings.TrimSuffix(row, "\r"), "\t"))
	}
	return rows
}

func TestPRTestFilesPICT(t *testing.T) {
	for _, row := range prRows(t) {
		change, location := row[0], row[1]
		switch change {
		case "modified", "deleted", "added", "untracked", "type-change":
		default:
			continue
		}
		t.Run(strings.Join(row, "/"), func(t *testing.T) {
			dir := t.TempDir()
			file := "config_test.go"
			if location == "nested" {
				file = "sub/config_test.go"
			}
			const original = "package fixture\n// Old contents.\n"
			fixture(t, dir, "config.go", "package fixture\n")
			path := filepath.Join(dir, file)
			if change != "added" && change != "untracked" {
				if change == "type-change" && location == "root" {
					if err := os.Symlink("config.go", path); err != nil {
						t.Fatal(err)
					}
				} else {
					fixture(t, dir, file, original)
				}
			}
			commitPRBase(t, dir)
			switch change {
			case "deleted", "type-change":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			switch {
			case change == "type-change" && location == "nested":
				if err := os.Symlink("../config.go", path); err != nil {
					t.Fatal(err)
				}
			case change != "deleted":
				fixture(t, dir, file, "package fixture\n// New contents.\n")
			}
			if change == "added" {
				runPRGit(t, dir, "add", "--", file)
			}
			got, err := previousTestFiles(t.Context(), dir, "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if change == "type-change" {
				if len(got) != 0 {
					t.Fatalf("accepted link transition: %+v", got)
				}
				return
			}
			root, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Path != filepath.Join(root, file) {
				t.Fatalf("incorrect old snapshot: %+v", got)
			}
			if change == "added" || change == "untracked" {
				if got[0].Data != nil {
					t.Fatalf("expected prior absent file: %+v", got[0])
				}
			} else if string(got[0].Data) != original {
				t.Fatalf("got contents %q, want %q", got[0].Data, original)
			}
		})
	}
}

func TestPreviousTestsOverlayUsesGoWithAddedAndDeletedTests(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "go.mod", "module example.org/pr-overlay\n\ngo 1.26.0\n")
	fixture(t, dir, "value.go", "package fixture\n")
	old := "package fixture\nimport \"testing\"\nfunc TestOld(t *testing.T) {}\n"
	fixture(t, dir, "modified_test.go", old)
	fixture(t, dir, "deleted_test.go", strings.Replace(old, "TestOld", "TestDeleted", 1))
	commitPRBase(t, dir)
	broken := strings.Replace(old, "{}", "{ t.Fatal(\"new test ran\") }", 1)
	fixture(t, dir, "modified_test.go", broken)
	fixture(t, dir, "added_test.go", strings.Replace(broken, "TestOld", "TestAdded", 1))
	runPRGit(t, dir, "add", "--", "added_test.go")
	fixture(t, dir, "untracked_test.go", strings.Replace(broken, "TestOld", "TestUntracked", 1))
	if err := os.Remove(filepath.Join(dir, "deleted_test.go")); err != nil {
		t.Fatal(err)
	}
	files, err := previousTestFiles(t.Context(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := makeOverlay(t.TempDir(), files)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "test", "-count=1", "-vet=off", "-v", "-overlay="+overlay, ".")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("PASS: TestOld")) || !bytes.Contains(out, []byte("PASS: TestDeleted")) ||
		bytes.Contains(out, []byte("TestAdded")) || bytes.Contains(out, []byte("TestUntracked")) {
		t.Fatalf("old tests did not run in isolation: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "modified_test.go"))
	if err != nil || string(data) != broken {
		t.Fatalf("modified test changed: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "deleted_test.go")); !os.IsNotExist(err) {
		t.Fatalf("deleted test now exists: %v", err)
	}
}

func runPRGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := git(t.Context(), dir, args...); err != nil {
		t.Fatal(err)
	}
}

func commitPRBase(t *testing.T, dir string) {
	t.Helper()
	runPRGit(t, dir, "init", "-q")
	runPRGit(t, dir, "add", ".")
	runPRGit(t, dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.org", "-c", "commit.gpgsign=false", "commit", "-qm", "base")
}

func TestPRSubhunksPICT(t *testing.T) {
	for _, row := range prRows(t) {
		change, location, layout := row[0], row[1], row[2]
		switch change {
		case "and", "or", "field", "mixed", "replacement":
		default:
			continue
		}
		t.Run(strings.Join(row, "/"), func(t *testing.T) {
			dir := t.TempDir()
			file := "code.go"
			if location == "nested" {
				file = "sub/code.go"
			}
			oldCond, oldLiteral := "a", "cfg{A:a}"
			if layout == "comments" {
				oldCond, oldLiteral = "a /* keep condition */", "cfg{A:a /* keep field */}"
			}
			newCond, newLiteral := oldCond, oldLiteral
			join := " "
			if layout == "multiline" {
				join = "\n "
			}
			switch change {
			case "and":
				newCond += " &&" + join + "b && c"
			case "or":
				newCond += " ||" + join + "b || c"
			case "field":
				newLiteral = strings.TrimSuffix(oldLiteral, "}") + "," + join + "B:b, C:c}"
			case "mixed":
				newCond += " &&" + join + "b"
				newLiteral = strings.TrimSuffix(oldLiteral, "}") + "," + join + "B:b}"
			case "replacement":
				newCond = strings.Replace(oldCond, "a", "b", 1)
			}
			program := func(cond, literal string) string {
				return "package example\ntype cfg struct { A, B, C bool }\nfunc Choose(a, b, c bool) cfg { if " + cond + " { return " + literal + " }; return cfg{} }\n"
			}
			old, current := program(oldCond, oldLiteral), program(newCond, newLiteral)
			fixture(t, dir, file, old)
			commitPRBase(t, dir)
			fixture(t, dir, file, current)
			_, reverts, err := gitScope(context.Background(), dir, "HEAD", true)
			if err != nil {
				t.Fatal(err)
			}
			items := reverts[filepath.Join(dir, filepath.Dir(file))]
			if len(items) == 0 {
				// Git and t.TempDir may spell the same directory with different roots.
				root, err := filepath.EvalSymlinks(dir)
				if err != nil {
					t.Fatal(err)
				}
				items = reverts[filepath.Join(root, filepath.Dir(file))]
			}
			want := 3
			if change == "replacement" {
				want = 1
			}
			if len(items) != want {
				t.Fatalf("got %d candidates, want %d: %+v", len(items), want, items)
			}
			if string(items[0].Data) != old {
				t.Fatalf("whole revert did not restore original: %q", items[0].Data)
			}
			seen := map[string]bool{old: true}
			for _, item := range items[1:] {
				if seen[string(item.Data)] || string(item.Data) == current {
					t.Fatalf("duplicate or unchanged sub-hunk: %q", item.Data)
				}
				seen[string(item.Data)] = true
				if _, err := parser.ParseFile(token.NewFileSet(), file, item.Data, 0); err != nil {
					t.Fatal(err)
				}
				if layout == "comments" && (!bytes.Contains(item.Data, []byte("/* keep condition */")) || !bytes.Contains(item.Data, []byte("/* keep field */"))) {
					t.Fatalf("sub-hunk lost a comment: %q", item.Data)
				}
				if change == "mixed" && bytes.Contains(item.Data, []byte("&&")) == bytes.Contains(item.Data, []byte("B:b")) {
					t.Fatalf("mixed sub-hunk did not leave exactly one addition: %q", item.Data)
				}
			}
			data, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil || string(data) != current {
				t.Fatalf("working file changed: %q, %v", data, err)
			}
		})
	}
}
