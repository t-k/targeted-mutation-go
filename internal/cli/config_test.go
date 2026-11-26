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

func TestTOMLConfigParsingAndValidation(t *testing.T) {
	cfg, err := parseTOMLConfig(".tmt.toml", `
# project defaults
[analyze]
patterns = ["./internal/..."]
exclude_rules = [
  "negation", # keep boundaries only
  "empty-block",
]
full = true
jobs = 2
timeout = "15s"
build_timeout = '3m'
format = "json"
baseline = ".tmt-baseline.json"
`)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.hasPatterns || len(cfg.Patterns) != 1 || cfg.Patterns[0] != "./internal/..." {
		t.Fatalf("unexpected patterns: %+v", cfg.Patterns)
	}
	if !cfg.hasExcludeRules || len(cfg.ExcludeRules) != 2 || cfg.ExcludeRules[0] != "negation" || cfg.ExcludeRules[1] != "empty-block" {
		t.Fatalf("unexpected exclude_rules: %+v", cfg.ExcludeRules)
	}
	if cfg.Full == nil || !*cfg.Full || cfg.Jobs == nil || *cfg.Jobs != 2 || cfg.Format == nil || *cfg.Format != "json" {
		t.Fatalf("unexpected scalar values: %+v", cfg)
	}
	if cfg.Timeout == nil || cfg.Timeout.String() != "15s" || cfg.BuildTimeout == nil || cfg.BuildTimeout.String() != "3m0s" {
		t.Fatalf("unexpected durations: timeout=%v build=%v", cfg.Timeout, cfg.BuildTimeout)
	}

	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{"unknown key", `unknown_flag = true`, "unknown config key"},
		{"duplicate key", "full = true\nfull = false\n", "duplicate config key"},
		{"unknown section", "[other]\nfull = true\n", "unknown config section"},
		{"invalid duration", `timeout = "forever"`, "invalid duration"},
		{"invalid bool", `full = yes`, "expected boolean"},
		{"invalid int", `jobs = "4"`, "expected integer"},
		{"unterminated array", "exclude_rules = [\n  \"negation\"\n", "unterminated array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTOMLConfig(".tmt.toml", tc.src); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCLIConfigAutoLoadOverrideAndDisable(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.org/cfgtest\n\ngo 1.26.0\n")
	write("calc.go", `package cfgtest
func Positive(x int) bool { return x > 0 }
`)
	write(".tmt.toml", `
[analyze]
patterns = ["."]
format = "json"
exclude_rules = ["negation"]
`)

	// 1. Auto-loads .tmt.toml from --dir (format=json, exclude_rules=["negation"]).
	var out, errOut bytes.Buffer
	code := Execute(context.Background(), []string{"analyze", "--dir", dir, "--list"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("auto-load failed (code=%d): %s", code, errOut.String())
	}
	var rep campaign.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("expected JSON from .tmt.toml format=json, got %q: %v", out.String(), err)
	}
	for _, f := range rep.Findings {
		if f.Rule == "negation" {
			t.Fatalf("expected negation excluded by .tmt.toml, got %+v", f)
		}
	}

	// 2. CLI --format text overrides .tmt.toml format = "json".
	out.Reset()
	errOut.Reset()
	code = Execute(context.Background(), []string{"analyze", "--dir", dir, "--list", "--format", "text"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("override failed (code=%d): %s", code, errOut.String())
	}
	if strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Fatalf("expected text output overriding format=json, got: %s", out.String())
	}

	// 3. --no-config ignores .tmt.toml (outputs text and includes negation).
	out.Reset()
	errOut.Reset()
	code = Execute(context.Background(), []string{"analyze", "--dir", dir, "--list", "--no-config", "."}, &out, &errOut)
	if code != 0 {
		t.Fatalf("--no-config failed (code=%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "x <= 0") {
		t.Fatalf("expected negation candidate when --no-config is set, got:\n%s", out.String())
	}

	// 4. --config and --no-config together must error.
	out.Reset()
	errOut.Reset()
	code = Execute(context.Background(), []string{"analyze", "--dir", dir, "--config", filepath.Join(dir, ".tmt.toml"), "--no-config", "--list"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got code=%d stderr=%q", code, errOut.String())
	}

	// 5. Explicit missing --config must error.
	out.Reset()
	errOut.Reset()
	code = Execute(context.Background(), []string{"analyze", "--dir", dir, "--config", filepath.Join(dir, "missing.toml"), "--list"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("expected error for missing --config, got code=%d", code)
	}
}
