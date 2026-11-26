package reporting

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/campaign"
)

func TestReportingPICT(t *testing.T) {
	data, err := os.ReadFile("../../verification/pict/reporting.tsv")
	if err != nil {
		t.Fatal(err)
	}
	repository := t.TempDir()
	project := filepath.Join(repository, "module")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	unicodeLine := "var a = \"🎉é\"; _ = a"
	locations := map[string]struct {
		file, uri, contents string
		column, utf16       int
	}{
		"plain":          {"a.go", "module/a.go", "package demo\nvar a int\n", 5, 5},
		"reserved":       {"a : #?%.go", "module/a%20:%20%23%3F%25.go", "", 1, 0},
		"unicode-column": {"é.go", "module/%C3%A9.go", "package demo\n" + unicodeLine + "\n", strings.Index(unicodeLine, "_") + 1, 16},
		"outside":        {"../../outside.go", "", "", 1, 0},
		"absent":         {},
	}
	for _, location := range locations {
		if location.contents != "" {
			if err := os.WriteFile(filepath.Join(project, location.file), []byte(location.contents), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	baselinePath := filepath.Join(repository, "baseline.json")
	if err := os.WriteFile(baselinePath, []byte(`{"schema":1,"ids":["existing"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	baseline, err := ReadBaseline(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		alert, kind, level string
		suppressible       bool
		incomplete         int
	}
	outcomes := map[string]outcome{
		"input-gap":          {"input-gap", "fail", "warning", true, 0},
		"assertion-gap":      {"assertion-gap", "fail", "warning", true, 0},
		"source-survivor":    {"source-survivor", "fail", "warning", true, 0},
		"untested-change":    {"untested-change", "fail", "warning", true, 0},
		"weakened-test":      {"weakened-test", "fail", "warning", true, 0},
		"killed":             {"killed", "pass", "none", false, 0},
		"non-viable":         {"non-viable", "notApplicable", "none", false, 0},
		"unreached":          {"unreached", "notApplicable", "none", false, 0},
		"unstable":           {"unstable", "fail", "error", false, 1},
		"timeout":            {"timeout", "fail", "error", false, 1},
		"build-error":        {"build-error", "fail", "error", false, 1},
		"error":              {"error", "fail", "error", false, 1},
		"cancelled":          {"cancelled", "fail", "error", false, 1},
		"candidate":          {"candidate", "informational", "none", false, 0},
		"not-evaluated":      {"not-evaluated", "fail", "error", false, 1},
		"audit-disagreement": {"audit-disagreement", "fail", "error", false, 1},
		"unknown":            {"other", "informational", "none", false, 0},
	}
	for _, row := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] {
		t.Run(strings.ReplaceAll(row, "\t", "/"), func(t *testing.T) {
			status, subcase, membership := splitPICT(row)
			where, expected := locations[subcase], outcomes[status]
			id := func(campaign.Finding) string { return "new" }
			if membership == "present" {
				id = func(campaign.Finding) string { return "existing" }
			}
			line := 2
			if where.file == "" {
				line = 0
			}
			finding := campaign.Finding{Status: status, File: where.file, Line: line, Column: where.column,
				Expression: "a > 1", Mutation: "a >= 1", Rule: "boundary", Evidence: "replayed", Detail: "first line\nsecond line"}
			run := campaign.Report{Schema: 1, Sites: 1, Incomplete: expected.incomplete, Findings: []campaign.Finding{finding}}
			if got, want := baseline.Suppresses(finding, id), expected.suppressible && membership == "present"; got != want {
				t.Errorf("suppression = %t, want %t", got, want)
			}

			var out bytes.Buffer
			if err := WriteJSONL(&out, run, id); err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(bytes.TrimSuffix(out.Bytes(), []byte("\n")), []byte("\n"))
			if len(lines) != 2 {
				t.Fatalf("want two records, got %q", out.String())
			}
			var header struct {
				Type   string                     `json:"type"`
				Schema int                        `json:"schema"`
				Report map[string]json.RawMessage `json:"report"`
			}
			var record struct {
				Type    string           `json:"type"`
				Schema  int              `json:"schema"`
				ID      string           `json:"id"`
				Finding campaign.Finding `json:"finding"`
			}
			decode(t, lines[0], &header)
			decode(t, lines[1], &record)
			if header.Type != "summary" || header.Schema != 1 || string(header.Report["sites"]) != "1" ||
				string(header.Report["incomplete"]) != fmt.Sprint(expected.incomplete) || header.Report["findings"] != nil {
				t.Errorf("invalid JSONL summary: %s", lines[0])
			}
			if record.Type != "finding" || record.Schema != 1 || record.ID != id(finding) || !reflect.DeepEqual(record.Finding, finding) {
				t.Errorf("invalid JSONL finding: %s", lines[1])
			}

			out.Reset()
			if err := WriteSARIF(&out, run, id, SARIFPaths{ProjectDir: project, RepositoryDir: repository}); err != nil {
				t.Fatal(err)
			}
			var sarif struct {
				Version string
				Runs    []struct {
					ColumnKind         string
					Invocations        []struct{ ExecutionSuccessful bool }
					OriginalURIBaseIDs map[string]struct{ URI string }
					Tool               struct {
						Driver struct {
							Name  string
							Rules []struct{ ID string }
						}
					}
					Results []struct {
						RuleID, Level, Kind string
						Message             struct{ Text string }
						PartialFingerprints map[string]string
						Locations           []struct {
							PhysicalLocation struct {
								ArtifactLocation struct{ URI, URIBaseID string }
								Region           struct{ StartLine, StartColumn int }
							}
						}
						Properties struct{ Finding campaign.Finding }
					}
				}
			}
			decode(t, out.Bytes(), &sarif)
			if sarif.Version != "2.1.0" || len(sarif.Runs) != 1 {
				t.Fatalf("invalid SARIF log: %s", out.String())
			}
			s := sarif.Runs[0]
			if s.ColumnKind != "utf16CodeUnits" || s.Tool.Driver.Name != "targeted-mutation-go" ||
				len(s.Invocations) != 1 || s.Invocations[0].ExecutionSuccessful != (expected.incomplete == 0) || len(s.Results) != 1 {
				t.Fatalf("invalid SARIF run: %s", out.String())
			}
			result := s.Results[0]
			if result.RuleID != expected.alert || result.Kind != expected.kind || result.Level != expected.level ||
				result.PartialFingerprints["targetedMutationId/v1"] != id(finding) || !strings.Contains(result.Message.Text, finding.Detail) ||
				!reflect.DeepEqual(result.Properties.Finding, finding) {
				t.Errorf("invalid SARIF result: %+v", result)
			}
			declared := false
			for _, rule := range s.Tool.Driver.Rules {
				declared = declared || rule.ID == result.RuleID
			}
			if !declared {
				t.Errorf("missing SARIF rule %q", result.RuleID)
			}
			if where.uri == "" {
				if len(result.Locations) != 0 {
					t.Errorf("unexpected SARIF location: %+v", result.Locations)
				}
				return
			}
			if len(result.Locations) != 1 {
				t.Fatalf("missing SARIF location: %+v", result)
			}
			location := result.Locations[0].PhysicalLocation
			if location.ArtifactLocation.URI != where.uri || location.Region.StartLine != line || location.Region.StartColumn != where.utf16 {
				t.Errorf("invalid SARIF location: %+v, want %s:%d:%d", location, where.uri, line, where.utf16)
			}
			base, err := url.Parse(s.OriginalURIBaseIDs[location.ArtifactLocation.URIBaseID].URI)
			if err != nil || base.Scheme != "file" || base.Path != filepath.ToSlash(repository)+"/" {
				t.Errorf("location does not resolve against the repository: %+v, %v", base, err)
			}
		})
	}
}

func TestWriteJSONLPreservesOrderAndEmptyRuns(t *testing.T) {
	for _, findings := range [][]campaign.Finding{nil, {{Expression: "first"}, {Expression: "second"}}} {
		var out bytes.Buffer
		if err := WriteJSONL(&out, campaign.Report{Schema: 1, Findings: findings}, expressionID); err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(&out)
		var metadata map[string]any
		if err := decoder.Decode(&metadata); err != nil || metadata["type"] != "summary" {
			t.Fatalf("summary: %v, %v", metadata, err)
		}
		for _, finding := range findings {
			var record map[string]any
			if err := decoder.Decode(&record); err != nil || record["id"] != finding.Expression {
				t.Fatalf("finding order: %v, %v", record, err)
			}
		}
		if err := decoder.Decode(&metadata); !errors.Is(err, io.EOF) {
			t.Fatalf("after last finding: %v", err)
		}
	}
}

func TestReportWritersReturnIOFailures(t *testing.T) {
	wantErr := errors.New("sink rejected bytes")
	run := campaign.Report{Schema: 1, Findings: []campaign.Finding{{Status: "input-gap", File: "a:literal.go", Line: 1}}}
	if err := WriteJSONL(rejectWrites{wantErr}, run, expressionID); !errors.Is(err, wantErr) {
		t.Errorf("JSONL write error = %v", err)
	}
	if err := WriteSARIF(rejectWrites{wantErr}, run, expressionID); !errors.Is(err, wantErr) {
		t.Errorf("SARIF write error = %v", err)
	}
	if uri := artifactURI("a:literal.go", SARIFPaths{}); uri != "./a:literal.go" {
		t.Errorf("relative URI confused with scheme: %q", uri)
	}
}

func TestWriteSARIFResolvesSiblingWorkspaceDirs(t *testing.T) {
	ws := t.TempDir()
	workspaceDir := filepath.Join(ws, "workspace")
	producerDir := filepath.Join(ws, "producer")
	unrelatedDir := filepath.Join(ws, "unrelated")
	for _, dir := range []string{workspaceDir, producerDir, unrelatedDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	unicodeLine := `var v = "🎉é"; _ = v`
	if err := os.WriteFile(filepath.Join(producerDir, "value.go"), []byte("package producer\n"+unicodeLine+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelatedDir, "secret.go"), []byte("package unrelated\nvar x = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	col := strings.Index(unicodeLine, "_") + 1
	run := campaign.Report{
		Schema: 1,
		Sites:  2,
		Findings: []campaign.Finding{
			{Status: "input-gap", File: "../producer/value.go", Line: 2, Column: col, Expression: "v > 0", Mutation: "v >= 0"},
			{Status: "input-gap", File: "../unrelated/secret.go", Line: 2, Column: 5, Expression: "x > 0", Mutation: "x >= 0"},
		},
	}

	var out bytes.Buffer
	paths := SARIFPaths{
		ProjectDir:    workspaceDir,
		RepositoryDir: workspaceDir,
		WorkspaceDirs: []string{producerDir, filepath.Join(ws, "nonexistent")},
	}
	if err := WriteSARIF(&out, run, expressionID, paths); err != nil {
		t.Fatal(err)
	}
	var sarif struct {
		Runs []struct {
			OriginalURIBaseIDs map[string]struct{ URI string }
			Results            []struct {
				Locations []struct {
					PhysicalLocation struct {
						ArtifactLocation struct{ URI, URIBaseID string }
						Region           struct{ StartLine, StartColumn int }
					}
				}
			}
		}
	}
	decode(t, out.Bytes(), &sarif)
	if len(sarif.Runs) != 1 || len(sarif.Runs[0].Results) != 2 {
		t.Fatalf("unexpected SARIF structure: %s", out.String())
	}
	s := sarif.Runs[0]
	base, err := url.Parse(s.OriginalURIBaseIDs[sarifSourceRoot].URI)
	if err != nil || base.Scheme != "file" || base.Path != filepath.ToSlash(ws)+"/" {
		t.Fatalf("unexpected %%SRCROOT%% base URI: %+v (%v)", base, err)
	}
	if len(s.Results[0].Locations) != 1 {
		t.Fatalf("expected sibling location, got %+v", s.Results[0].Locations)
	}
	loc := s.Results[0].Locations[0].PhysicalLocation
	if loc.ArtifactLocation.URI != "producer/value.go" || loc.ArtifactLocation.URIBaseID != sarifSourceRoot ||
		loc.Region.StartLine != 2 || loc.Region.StartColumn != 16 {
		t.Errorf("unexpected sibling location: %+v", loc)
	}
	if len(s.Results[1].Locations) != 0 {
		t.Errorf("unexpected location for sibling outside WorkspaceDirs: %+v", s.Results[1].Locations)
	}

	// Symlink inside repository pointing outside must be rejected physically.
	escapeLink := filepath.Join(workspaceDir, "escape.go")
	if err := os.Symlink(filepath.Join(unrelatedDir, "secret.go"), escapeLink); err == nil {
		out.Reset()
		escapeRun := campaign.Report{
			Schema:   1,
			Sites:    1,
			Findings: []campaign.Finding{{Status: "input-gap", File: "escape.go", Line: 2, Column: 5, Expression: "x > 0", Mutation: "x >= 0"}},
		}
		if err := WriteSARIF(&out, escapeRun, expressionID, paths); err != nil {
			t.Fatal(err)
		}
		var escapeSarif struct {
			Runs []struct {
				Results []struct {
					Locations []any
				}
			}
		}
		decode(t, out.Bytes(), &escapeSarif)
		if len(escapeSarif.Runs[0].Results[0].Locations) != 0 {
			t.Fatalf("expected physical symlink escape outside repo/WorkspaceDirs to omit SARIF location: %+v", escapeSarif.Runs[0].Results[0].Locations)
		}
	}
}

func TestBaselineSnapshotRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "baseline.json")
	findings := []campaign.Finding{{Status: "input-gap", Expression: "z", Line: 1}, {Status: "assertion-gap", Expression: "a"},
		{Status: "source-survivor", Expression: "a"}, {Status: "killed", Expression: "k"}, {Status: "timeout", Expression: "e"}}
	if err := WriteBaseline(file, campaign.Report{Findings: findings}, expressionID); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Schema int
		IDs    []string
	}
	decode(t, raw, &doc)
	if doc.Schema != 1 || !reflect.DeepEqual(doc.IDs, []string{"a", "z"}) {
		t.Errorf("invalid baseline snapshot: %s", raw)
	}
	known, err := ReadBaseline(file)
	if err != nil {
		t.Fatal(err)
	}
	moved := findings[0]
	moved.Line = 42
	if !known.Suppresses(moved, expressionID) {
		t.Errorf("known finding at new location was not suppressed")
	}
	if err := WriteBaseline(file, campaign.Report{}, expressionID); err != nil {
		t.Fatal(err)
	}
	empty, err := ReadBaseline(file)
	if err != nil || empty.Suppresses(moved, expressionID) {
		t.Fatalf("empty snapshot did not replace the previous baseline: %v", err)
	}
}

func TestReadBaselineRejectsInvalidSnapshots(t *testing.T) {
	cases := []string{"", `null`, `{}`, `{"schema":2,"ids":[]}`, `{"schema":1}`, `{"schema":1,"ids":null}`,
		`{"schema":1,"ids":[" "]}`, `{"schema":1,"ids":[2]}`, `{"schema":1,"ids":[]} {}`}
	file := filepath.Join(t.TempDir(), "baseline.json")
	for _, input := range cases {
		if err := os.WriteFile(file, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadBaseline(file); err == nil {
			t.Errorf("accepted malformed baseline %q", input)
		}
	}
}

func TestWriteBaselinePrePublishFailureLeavesOldSnapshot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "baseline.json")
	run := campaign.Report{Findings: []campaign.Finding{{Status: "input-gap", Expression: "old"}}}
	if err := WriteBaseline(file, run, expressionID); err != nil {
		t.Fatal(err)
	}
	if err := WriteBaseline(file, run, func(campaign.Finding) string { return "" }); err == nil {
		t.Fatal("wrote baseline with missing stable ID")
	}
	known, err := ReadBaseline(file)
	if err != nil || !known.Suppresses(run.Findings[0], expressionID) {
		t.Fatalf("previous baseline missing after failed write: %v", err)
	}
}

func TestConcurrentBaselinePublicationsExposeCompleteSnapshots(t *testing.T) {
	file := filepath.Join(t.TempDir(), "baseline.json")
	old := campaign.Finding{Status: "input-gap", Expression: "old"}
	new := campaign.Finding{Status: "input-gap", Expression: "new"}
	if err := WriteBaseline(file, campaign.Report{Findings: []campaign.Finding{old}}, expressionID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var writers sync.WaitGroup
	for _, finding := range []campaign.Finding{old, new} {
		writers.Go(func() {
			for range 8 {
				if err := WriteBaseline(file, campaign.Report{Findings: []campaign.Finding{finding}}, expressionID); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	go func() {
		writers.Wait()
		close(done)
	}()
	for {
		known, err := ReadBaseline(file)
		if err != nil {
			t.Fatalf("partial baseline became visible: %v", err)
		}
		if known.Suppresses(old, expressionID) == known.Suppresses(new, expressionID) {
			t.Fatal("visible baseline is not a complete snapshot")
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func splitPICT(row string) (string, string, string) {
	fields := strings.Fields(row)
	return fields[0], fields[1], fields[2]
}

func expressionID(f campaign.Finding) string { return f.Expression }

func decode(t *testing.T, data []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("invalid JSON: %v in %s", err, data)
	}
}

type rejectWrites struct{ err error }

func (w rejectWrites) Write([]byte) (int, error) { return 0, w.err }
