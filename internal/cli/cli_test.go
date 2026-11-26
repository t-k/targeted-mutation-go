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
