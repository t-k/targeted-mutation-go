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
