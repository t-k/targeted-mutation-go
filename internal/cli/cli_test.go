package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/t-k/targeted-mutation-go/internal/campaign"
)

func TestCLIHelpAndInvalidArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		text string
	}{
		{[]string{"--help"}, 0, "targeted-mutation-go analyze"},
		{[]string{"analyze", "--jobs=0"}, 1, "positive"},
		{[]string{"analyze", "--build-timeout=0s"}, 1, "positive"},
		{[]string{"analyze", "--dir"}, 1, "flag needs an argument"},
		{[]string{"analyze", ".", "--dir"}, 1, "flag needs an argument"},
		{[]string{"analyze", "--format=xml"}, 1, "format"},
		{[]string{"analyze", "--undo-changes"}, 1, "--since"},
		{[]string{"analyze", "--conventional", "--verify-witnesses"}, 1, "--conventional"},
		{[]string{"not-a-command"}, 1, "unknown command"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Execute(context.Background(), tc.args, &stdout, &stderr); code != tc.code ||
			!strings.Contains(stdout.String()+stderr.String(), tc.text) {
			t.Errorf("%v: code %d, stdout %s, stderr %s", tc.args, code, &stdout, &stderr)
		}
	}
}

func TestCLIReportsVerifiedWitnessInJSON(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":   "module example.org/limit\n\ngo 1.26.0\n",
		"limit.go": "package limit\nfunc isFree(x int) bool { return x >= 5000 }\n",
		"limit_test.go": `package limit
import "testing"
func TestFree(t *testing.T) {
	if isFree(4800) || !isFree(5000) { t.Fatal("wrong limit") }
}
`,
	}
	for path, data := range files {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"analyze", "--dir", dir, "--format=json", "--verify-witnesses", "."}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code %d, stdout %s, stderr %s", code, &stdout, &stderr)
	}
	var report campaign.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Schema != 1 || len(report.Findings) != 1 || report.Findings[0].Witness == nil ||
		!report.Findings[0].Witness.Verified || report.Findings[0].Witness.Input != "4999" {
		t.Fatalf("incorrect report: %s", &stdout)
	}

	// Verify interspersed flags after package pattern and clean --list text summary without empty [].
	stdout.Reset()
	stderr.Reset()
	if listCode := Execute(context.Background(), []string{"analyze", ".", "--dir", dir, "--jobs=1", "--list"}, &stdout, &stderr); listCode != 0 {
		t.Fatalf("--list with interspersed flags failed (%d): %s", listCode, &stderr)
	}
	if strings.Contains(stdout.String(), "[]") || !strings.Contains(stdout.String(), "1 sites, 3 candidates") {
		t.Fatalf("unexpected --list text output:\n%s", &stdout)
	}
	stdout.Reset()
	stderr.Reset()
	if dashCode := Execute(context.Background(), []string{"analyze", "--dir", dir, "--list", "--", "."}, &stdout, &stderr); dashCode != 0 {
		t.Fatalf("--list with -- separator failed (%d): %s", dashCode, &stderr)
	}
}

func TestCLIZeroTestsWarningAndRequireTests(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.org/notests\n\ngo 1.26.0\n",
		"lib.go": "package lib\nfunc Check(x int) bool { return x <= 10 }\n",
	}
	for path, data := range files {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"analyze", "--dir", dir, "--jobs=1", "."}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: stderr=%s", code, &stderr)
	}
	if !strings.Contains(stderr.String(), "0 tests ran") || !strings.Contains(stderr.String(), "--test-package") {
		t.Fatalf("expected zero-test warning with --test-package hint, got stderr=%q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Execute(context.Background(), []string{"analyze", "--dir", dir, "--jobs=1", "--require-tests", "."}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 with --require-tests, got %d: stderr=%s", code, &stderr)
	}
	if !strings.Contains(stderr.String(), "0 tests ran") {
		t.Fatalf("expected zero-test error message, got stderr=%q", stderr.String())
	}
}

func TestCLILinesOutOfRangeAndNoSiteHint(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.org/lines\n\ngo 1.26.0\n",
		"topic.go": `package lines

import "strings"

func Allowed(s string) bool {
	return strings.HasPrefix(s, "events.")
}
`,
		"topic_test.go": `package lines
import "testing"
func TestAllowed(t *testing.T) {
	if !Allowed("events.a") { t.Fatal("want true") }
}
`,
	}
	for path, data := range files {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"analyze", "--dir", dir, "--lines", "topic.go:999", "."}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "exceeds file length") {
		t.Fatalf("expected out-of-range error (code 1), got code=%d stderr=%q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Execute(context.Background(), []string{"analyze", "--dir", dir, "--jobs=1", "--lines", "topic.go:6", "."}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 for valid line with no default sites, got %d: stderr=%s", code, &stderr)
	}
	if !strings.Contains(stderr.String(), "--full") {
		t.Fatalf("expected --full hint on stderr, got %q", stderr.String())
	}
}

func TestCLIWriteBaselineNotificationAndBaselineCommand(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":   "module example.org/base\n\ngo 1.26.0\n",
		"limit.go": "package base\nfunc Free(x int) bool { return x >= 100 }\n",
		"limit_test.go": `package base
import "testing"
func TestFree(t *testing.T) {
	if Free(80) || !Free(100) { t.Fatal("wrong") }
}
`,
	}
	for path, data := range files {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	baselineFile := filepath.Join(dir, "baseline.json")
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"analyze", "--dir", dir, "--jobs=1", "--write-baseline", baselineFile, "."}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("analyze --write-baseline expected exit 2, got %d: stderr=%s", code, &stderr)
	}
	if !strings.Contains(stderr.String(), "wrote baseline") || !strings.Contains(stderr.String(), "(1 findings)") {
		t.Fatalf("expected baseline write notification on stderr, got %q", stderr.String())
	}

	baselineFile2 := filepath.Join(dir, "baseline2.json")
	stdout.Reset()
	stderr.Reset()
	code = Execute(context.Background(), []string{"baseline", "--dir", dir, "--jobs=1", "--out", baselineFile2, "."}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("baseline --out expected exit 0, got %d: stderr=%s", code, &stderr)
	}
	if !strings.Contains(stderr.String(), "wrote baseline") || !strings.Contains(stderr.String(), "(1 findings)") {
		t.Fatalf("expected baseline command notification on stderr, got %q", stderr.String())
	}
	if _, err := os.Stat(baselineFile2); err != nil {
		t.Fatalf("baseline file not created: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if badCode := Execute(context.Background(), []string{"baseline", "--dir", dir, "."}, &stdout, &stderr); badCode != 1 {
		t.Fatalf("baseline without --out expected exit 1, got %d", badCode)
	}
}
