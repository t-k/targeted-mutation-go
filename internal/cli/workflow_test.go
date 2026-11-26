package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/campaign"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, data := range files {
		file := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCLIListsStableShardsWithoutRunningTests(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/listing.tsv")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod":         "module example.org/listing\n\ngo 1.26.0\n",
		"broken_test.go": "package listing\nimport \"testing\"\nfunc TestBug(t *testing.T) { t.Fatal(\"must not run\") }\n",
	})
	const body = "func F(x int) bool { return x >= 1 }\nfunc G(x int) bool { return x >= 1 }\n"
	collect := func(profile, shard, move string) map[string]bool {
		t.Helper()
		prefix := "package listing\n"
		if move == "moved" {
			prefix += "\n// Move declarations down.\n\n"
		}
		writeFiles(t, dir, map[string]string{"listing.go": prefix + body})
		args := []string{"--list", "--exclude-rule=negation"}
		if profile == "source" {
			args = append(args, "--source-only")
		}
		switch shard {
		case "first":
			args = append(args, "--shard=0/2")
		case "second":
			args = append(args, "--shard=1/2")
		}
		report, status := analyzeJSON(t, dir, append(args, ".")...)
		if status != 0 || report.Builds != 0 || report.SourceRuns != 0 || report.Replays != 0 {
			t.Fatalf("preview ran tests: exit %d, %+v", status, report)
		}
		ids := make(map[string]bool)
		for _, finding := range report.Findings {
			if finding.Status != "candidate" || finding.ID == "" || ids[finding.ID] || finding.Rule == "negation" {
				t.Fatalf("invalid preview finding: %+v", finding)
			}
			ids[finding.ID] = true
		}
		return ids
	}
	oracles := make(map[string]map[string]bool)
	for _, profile := range []string{"targeted", "source"} {
		whole := collect(profile, "all", "original")
		first, second := collect(profile, "first", "original"), collect(profile, "second", "original")
		if len(whole) == 0 || len(first)+len(second) != len(whole) {
			t.Fatalf("shards did not partition %s", profile)
		}
		joined := maps.Clone(first)
		maps.Copy(joined, second)
		if !maps.Equal(whole, joined) {
			t.Fatalf("shards did not cover %s", profile)
		}
		oracles[profile+"/all"], oracles[profile+"/first"], oracles[profile+"/second"] = whole, first, second
	}
	for _, row := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] {
		fields := strings.Fields(row)
		key := fmt.Sprintf("%s/%s", fields[0], fields[1])
		if actual := collect(fields[0], fields[1], fields[2]); !maps.Equal(oracles[key], actual) {
			t.Errorf("unstable IDs for %s: %v", row, actual)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, data)
	}
}

func analyzeJSON(t *testing.T, dir string, args ...string) (campaign.Report, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), append([]string{"analyze", "--dir", dir, "--format=json", "--jobs=2"}, args...), &stdout, &stderr)
	var report campaign.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("CLI exit %d: %s\n%s", code, &stderr, &stdout)
	}
	return report, code
}

func TestCLIProducesVerifiedSuggestions(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/suggestions.tsv")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var production, tests strings.Builder
	production.WriteString("package suggestions\n")
	tests.WriteString("package suggestions\nimport \"testing\"\n")
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")[1:]
	for i, row := range rows {
		fields := strings.Fields(row)
		params, args := "x int", ""
		if fields[0] == "extra" {
			params, args = "x int, tag string", `, "label"`
		}
		fmt.Fprintf(&production, "func Case%d(%s) bool { return x >= 18 }\n", i, params)
		body := fmt.Sprintf("if Case%d(17%s) || !Case%d(20%s) { t.Fatal(\"boundary\") }", i, args, i, args)
		if fields[1] == "weak-test" {
			body = fmt.Sprintf("_ = Case%d(18%s)", i, args)
		}
		if fields[2] == "nested" {
			body = `t.Run("nested", func(t *testing.T) { ` + body + ` })`
		}
		fmt.Fprintf(&tests, "func TestCase%d(t *testing.T) { %s }\n", i, body)
	}
	writeFiles(t, dir, map[string]string{
		"go.mod": "module example.org/suggestions\n\ngo 1.26.0\n", "real.go": production.String(), "real_test.go": tests.String(),
	})
	report, code := analyzeJSON(t, dir, "--verify-witnesses", "--suggest-assertions", "--comparison=boundary", ".")
	if code != 2 || len(report.Findings) != len(rows) || report.Incomplete != 0 {
		t.Fatalf("exit %d, report %+v", code, report)
	}
	for i, f := range report.Findings {
		fields := strings.Fields(rows[i])
		wantCall, wantStatus := fmt.Sprintf("Case%d(18)", i), fields[1]
		if fields[0] == "extra" {
			wantCall = fmt.Sprintf(`Case%d(18, "")`, i)
		}
		if wantStatus == "weak-test" {
			wantStatus = "assertion-gap"
			name := fmt.Sprintf("TestCase%d", i)
			if fields[2] == "nested" {
				name += "/nested"
			}
			if len(f.Tests) != 1 || f.Tests[0] != name {
				t.Errorf("%s: tests %v, want %q", rows[i], f.Tests, name)
			}
		}
		if f.Status != wantStatus || f.Witness == nil || !f.Witness.Verified || f.Witness.Call != wantCall ||
			!strings.Contains(f.Witness.Code, "if got := "+wantCall+"; got != true") {
			t.Errorf("%s: finding %+v, witness %+v", rows[i], f, f.Witness)
		}
	}
}

func TestCLIFormatsReportsAndSuppressesKnownGaps(t *testing.T) {
	dir := t.TempDir()
	const source = "package reports\nfunc Okay(x int) bool { return x >= 1 }\n"
	writeFiles(t, dir, map[string]string{
		"go.mod": "module example.org/reports\n\ngo 1.26.0\n", "reports.go": source,
		"reports_test.go": "package reports\nimport \"testing\"\nfunc Test(t *testing.T) { if !Okay(2) { t.Fatal(\"bad value\") } }\n",
	})
	baseline := filepath.Join(dir, "baseline.json")
	first, code := analyzeJSON(t, dir, "--comparison=boundary", "--write-baseline="+baseline, ".")
	if code != 2 || len(first.Findings) != 1 || first.Findings[0].ID == "" {
		t.Fatalf("missing baseline candidate: exit %d, %+v", code, first)
	}
	writeFiles(t, dir, map[string]string{"reports.go": "\n// Move the source.\n" + source})
	filtered, code := analyzeJSON(t, dir, "--comparison=boundary", "--baseline="+baseline, ".")
	if code != 0 || filtered.Suppressed != 1 || len(filtered.Findings) != 0 {
		t.Fatalf("known gap not suppressed: exit %d, %+v", code, filtered)
	}
	for _, format := range []string{"jsonl", "sarif"} {
		var stdout, stderr bytes.Buffer
		code := Execute(context.Background(), []string{"analyze", "--dir", dir, "--comparison=boundary", "--format=" + format, "."}, &stdout, &stderr)
		if code != 2 || !strings.Contains(stdout.String(), first.Findings[0].ID) {
			t.Fatalf("%s exit %d, stdout %s, stderr %s", format, code, &stdout, &stderr)
		}
		if format == "jsonl" {
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if len(lines) != 2 || !json.Valid([]byte(lines[0])) || !json.Valid([]byte(lines[1])) {
				t.Fatalf("invalid JSONL: %s", &stdout)
			}
		} else if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"version":"2.1.0"`) &&
			!strings.Contains(stdout.String(), `"version": "2.1.0"`) {
			t.Fatalf("invalid SARIF: %s", &stdout)
		}
	}
}

func TestCLIResumesCompletedResultsAndRejectsChangedTests(t *testing.T) {
	dir := t.TempDir()
	test := "package resume\nimport \"testing\"\nfunc Test(t *testing.T) { if !Ready(1) { t.Fatal(\"wrong value\") } }\n"
	writeFiles(t, dir, map[string]string{
		"go.mod":         "module example.org/resume\n\ngo 1.26.0\n",
		"resume.go":      "package resume\nfunc Ready(x int) bool { return x >= 1 }\n",
		"resume_test.go": test,
	})
	path := filepath.Join(dir, "outcomes.jsonl")
	args := []string{"--checkpoint=" + path, "--conventional", "--comparison=boundary", "--show-killed", "."}
	first, firstExit := analyzeJSON(t, dir, args...)
	second, secondExit := analyzeJSON(t, dir, args...)
	if firstExit != 0 || secondExit != 0 || first.Killed != 1 || second.Killed != 1 || first.Replays != 1 || second.Replays != 0 || second.Resumed != 1 {
		t.Fatalf("first exit %d report %+v, resumed exit %d report %+v", firstExit, first, secondExit, second)
	}
	writeFiles(t, dir, map[string]string{"resume_test.go": strings.ReplaceAll(test, "Ready(1)", "Ready(2)")})
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), append([]string{"analyze", "--dir", dir}, args...), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "fingerprint") {
		t.Fatalf("changed tests reused a checkpoint: exit %d, %s %s", code, &stdout, &stderr)
	}
}

func TestCLIVerifiesConcreteSourceWitness(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod":        "module example.org/sourcewitness\n\ngo 1.26.0\n",
		"affix.go":      "package sourcewitness\nimport \"strings\"\nfunc Match(s string) bool { return strings.HasPrefix(s, \"needle\") }\n",
		"affix_test.go": "package sourcewitness\nimport \"testing\"\nfunc TestMatch(t *testing.T) { if !Match(\"needle\") { t.Fatal(\"prefix\") } }\n",
	})
	r, code := analyzeJSON(t, dir, "--source-only", "--verify-witnesses", ".")
	if code != 2 {
		t.Fatalf("exit %d, report %+v", code, r)
	}
	for _, f := range r.Findings {
		if f.Rule == "affix-contains" {
			if f.Status != "source-survivor" || f.SourceWitness == nil || !f.SourceWitness.Verified ||
				f.SourceWitness.Original != "false" || f.SourceWitness.Mutated != "true" || !strings.Contains(f.SourceWitness.Code, "Match(") {
				t.Fatalf("missing concrete source witness: %+v", f)
			}
			return
		}
	}
	t.Fatalf("missing affix mutant: %+v", r)
}

func TestCLIRecognizesWeakenedTestsSinceRevision(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/weakened.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] {
		fields := strings.Fields(row)
		t.Run(strings.Join(fields, "/"), func(t *testing.T) {
			dir := t.TempDir()
			const testPrefix = "package weakened\nimport \"testing\"\nfunc TestReady(t *testing.T) { "
			const strong = `if !Ready(1) { t.Fatal("boundary") }`
			old := strong
			if fields[0] == "broken" {
				old = `t.Fatal("baseline broken")`
			}
			writeFiles(t, dir, map[string]string{
				"go.mod":        "module example.org/weakened\n\ngo 1.26.0\n",
				"limit.go":      "package weakened\nfunc Ready(x int) bool { return x >= 1 }\n",
				"limit_test.go": testPrefix + old + " }\n",
			})
			runGit(t, dir, "init", "-q")
			runGit(t, dir, "add", ".")
			runGit(t, dir, "-c", "user.name=TMT Test", "-c", "user.email=tmt@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "base")
			current := map[string]string{"loose": `if !Ready(2) { t.Fatal("loose") }`, "unreached": "", "detects": strong + " // still checks the boundary\n"}[fields[1]]
			writeFiles(t, dir, map[string]string{"limit_test.go": testPrefix + current + " }\n"})
			args := []string{"--since=HEAD", "--comparison=boundary"}
			if fields[2] == "source" {
				args = append(args, "--source-only")
			}
			r, code := analyzeJSON(t, dir, append(args, ".")...)
			want := 2
			if fields[0] == "broken" {
				want = 3
			} else if fields[1] == "detects" {
				want = 0
			}
			if code != want {
				t.Fatalf("exit %d want %d, report %+v", code, want, r)
			}
			weakened := false
			for _, f := range r.Findings {
				if f.Status == "weakened-test" && (f.Rule == "boundary" || f.Rule == "source-operator" && f.Mutation == ">") {
					weakened = len(f.Tests) == 1 && f.Tests[0] == "TestReady"
				}
				if f.Status == "input-gap" || f.Status == "source-survivor" {
					t.Fatalf("ordinary finding outside the diff: %+v", f)
				}
			}
			if weakened != (want == 2) {
				t.Fatalf("unexpected weakened classification with exit %d: %+v", code, r.Findings)
			}
		})
	}
}

func TestCLISelectsMutationSitesAndTestsIndependently(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/selection.tsv")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod":     "module example.org/workflow\n\ngo 1.26.0\n",
		"lib/lib.go": "package lib\nfunc Eligible(x int) bool { return x >= 1 }\nfunc Other(x int) bool { return x >= 3 }\n",
		"lib/lib_test.go": `package lib
import "testing"
func TestBoundary(t *testing.T) { if !Eligible(2) { t.Fatal("eligible") } }
func TestOther(t *testing.T) { if !Other(3) { t.Fatal("other") } }
`,
		"client/client_test.go": `package client
import ("testing"; "example.org/workflow/lib")
func TestBoundary(t *testing.T) { if !lib.Eligible(2) { t.Fatal("eligible") } }
func TestOther(t *testing.T) { if !lib.Other(3) { t.Fatal("other") } }
`,
	})
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "-c", "user.name=TMT Test", "-c", "user.email=tmt@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "base")
	writeFiles(t, dir, map[string]string{"lib/lib.go": "package lib\nfunc Eligible(x int) bool { return x >= 2 }\nfunc Other(x int) bool { return x >= 3 }\n"})
	for _, row := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] {
		fields := strings.Fields(row)
		t.Run(strings.Join(fields, "/"), func(t *testing.T) {
			args := []string{"--show-killed", "--conventional", "--comparison=boundary"}
			wantSites, wantKilled := 2, 2
			switch fields[0] {
			case "file":
				args = append(args, "--lines=lib/lib.go")
			case "line":
				args, wantSites, wantKilled = append(args, "--lines=lib/lib.go:2"), 1, 1
			case "intersection":
				args, wantSites, wantKilled = append(args, "--since=HEAD", "--lines=lib/lib.go:2-3"), 1, 1
			}
			consumer := "example.org/workflow/lib"
			if fields[1] == "dependent" {
				args, consumer = append(args, "--test-package=./client"), "example.org/workflow/client"
			}
			if fields[2] == "boundary" {
				args, wantKilled = append(args, "--test=^TestBoundary$"), 1
			}
			report, code := analyzeJSON(t, dir, append(args, "./lib")...)
			if code != 0 || report.Sites != wantSites || report.Killed != wantKilled {
				t.Fatalf("exit %d, report %+v", code, report)
			}
			for _, finding := range report.Findings {
				if len(finding.Packages) != 1 || finding.Packages[0] != consumer {
					t.Fatalf("expected consumer %q: %+v", consumer, finding)
				}
			}
		})
	}
}
