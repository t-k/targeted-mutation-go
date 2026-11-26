package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/t-k/targeted-mutation-go/internal/campaign"
)

type fileConfig struct {
	Patterns          []string
	TestPatterns      []string
	Lines             []string
	ExcludeRules      []string
	TestRun           *string
	Comparison        *string
	Tags              *string
	Jobs              *int
	Timeout           *time.Duration
	TimeoutRecheck    *string
	BuildTimeout      *time.Duration
	Full              *bool
	SourceOnly        *bool
	IncludeGenerated  *bool
	Conventional      *bool
	VerifySurvivors   *bool
	VerifyWitnesses   *bool
	SuggestAssertions *bool
	ShowUnreached     *bool
	UndoChanges       *bool
	KilledFindings    *bool
	ShowNonViable     *bool
	Audit             *int
	Format            *string
	Baseline          *string
	Checkpoint        *string

	hasPatterns      bool
	hasTestPatterns  bool
	hasLines         bool
	hasExcludeRules  bool
	dir              string
}

func loadConfigFile(path string) (fileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := parseTOMLConfig(path, string(data))
	if err != nil {
		return fileConfig{}, err
	}
	cfg.dir = filepath.Dir(path)
	return cfg, nil
}

func parseTOMLConfig(path, input string) (fileConfig, error) {
	var cfg fileConfig
	seen := make(map[string]bool)
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	section := ""
	for i := 0; i < len(lines); i++ {
		lineNum := i + 1
		raw := strings.TrimSpace(stripTOMLComment(lines[i]))
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "[") {
			if !strings.HasSuffix(raw, "]") || strings.HasPrefix(raw, "[[") {
				return cfg, fmt.Errorf("%s:%d: invalid section header", path, lineNum)
			}
			section = strings.TrimSpace(raw[1 : len(raw)-1])
			if section != "analyze" {
				return cfg, fmt.Errorf("%s:%d: unknown config section [%s]", path, lineNum, section)
			}
			continue
		}
		eq := strings.IndexByte(raw, '=')
		if eq <= 0 {
			return cfg, fmt.Errorf("%s:%d: expected key = value", path, lineNum)
		}
		key := strings.TrimSpace(raw[:eq])
		val := strings.TrimSpace(raw[eq+1:])
		if key == "" {
			return cfg, fmt.Errorf("%s:%d: empty config key", path, lineNum)
		}
		if seen[key] {
			return cfg, fmt.Errorf("%s:%d: duplicate config key %q", path, lineNum, key)
		}
		seen[key] = true

		// Support multi-line string arrays.
		if strings.HasPrefix(val, "[") && !arrayClosed(val) {
			var buf strings.Builder
			buf.WriteString(val)
			closed := false
			for i+1 < len(lines) {
				i++
				part := strings.TrimSpace(stripTOMLComment(lines[i]))
				if part == "" {
					continue
				}
				buf.WriteByte(' ')
				buf.WriteString(part)
				if arrayClosed(buf.String()) {
					closed = true
					break
				}
			}
			if !closed {
				return cfg, fmt.Errorf("%s:%d: unterminated array for %q", path, lineNum, key)
			}
			val = buf.String()
		}

		if err := assignConfigKey(&cfg, key, val); err != nil {
			return cfg, fmt.Errorf("%s:%d: %w", path, lineNum, err)
		}
	}
	return cfg, nil
}

func assignConfigKey(cfg *fileConfig, key, val string) error {
	switch key {
	case "patterns":
		list, err := parseTOMLStringArray(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.Patterns, cfg.hasPatterns = list, true
	case "test_packages":
		list, err := parseTOMLStringArray(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.TestPatterns, cfg.hasTestPatterns = list, true
	case "lines":
		list, err := parseTOMLStringArray(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.Lines, cfg.hasLines = list, true
	case "exclude_rules":
		list, err := parseTOMLStringArray(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.ExcludeRules, cfg.hasExcludeRules = list, true
	case "test":
		s, err := parseTOMLString(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.TestRun = &s
	case "comparison":
		s, err := parseTOMLString(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.Comparison = &s
	case "tags":
		s, err := parseTOMLString(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.Tags = &s
	case "timeout_recheck":
		s, err := parseTOMLString(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.TimeoutRecheck = &s
	case "format":
		s, err := parseTOMLString(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.Format = &s
	case "baseline":
		s, err := parseTOMLString(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.Baseline = &s
	case "checkpoint":
		s, err := parseTOMLString(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.Checkpoint = &s
	case "timeout":
		s, err := parseTOMLString(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%s: invalid duration %q: %w", key, s, err)
		}
		cfg.Timeout = &d
	case "build_timeout":
		s, err := parseTOMLString(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%s: invalid duration %q: %w", key, s, err)
		}
		cfg.BuildTimeout = &d
	case "jobs":
		n, err := parseTOMLInt(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.Jobs = &n
	case "audit":
		n, err := parseTOMLInt(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.Audit = &n
	case "full":
		b, err := parseTOMLBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.Full = &b
	case "source_only":
		b, err := parseTOMLBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.SourceOnly = &b
	case "include_generated":
		b, err := parseTOMLBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.IncludeGenerated = &b
	case "conventional":
		b, err := parseTOMLBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.Conventional = &b
	case "verify_survivors":
		b, err := parseTOMLBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.VerifySurvivors = &b
	case "verify_witnesses":
		b, err := parseTOMLBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.VerifyWitnesses = &b
	case "suggest_assertions":
		b, err := parseTOMLBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.SuggestAssertions = &b
	case "show_unreached":
		b, err := parseTOMLBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.ShowUnreached = &b
	case "undo_changes":
		b, err := parseTOMLBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.UndoChanges = &b
	case "show_killed":
		b, err := parseTOMLBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.KilledFindings = &b
	case "show_non_viable":
		b, err := parseTOMLBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		cfg.ShowNonViable = &b
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	return nil
}

func (fc fileConfig) apply(cfg *campaign.Config, format, baselinePath *string, showNonViable *bool, setFlags map[string]bool) {
	resolvePath := func(p string) string {
		if p == "" || filepath.IsAbs(p) || fc.dir == "" {
			return p
		}
		return filepath.Join(fc.dir, p)
	}
	if fc.hasTestPatterns && !setFlags["test-package"] {
		cfg.TestPatterns = fc.TestPatterns
	}
	if fc.hasLines && !setFlags["lines"] {
		cfg.Lines = fc.Lines
	}
	if fc.hasExcludeRules && !setFlags["exclude-rule"] {
		cfg.ExcludeRules = fc.ExcludeRules
	}
	if fc.TestRun != nil && !setFlags["test"] {
		cfg.TestRun = *fc.TestRun
	}
	if fc.Comparison != nil && !setFlags["comparison"] {
		cfg.Comparison = *fc.Comparison
	}
	if fc.Tags != nil && !setFlags["tags"] {
		cfg.Tags = *fc.Tags
	}
	if fc.Jobs != nil && !setFlags["jobs"] {
		cfg.Jobs = *fc.Jobs
	}
	if fc.Timeout != nil && !setFlags["timeout"] {
		cfg.Timeout = *fc.Timeout
		cfg.TimeoutExplicit = true
	}
	if fc.TimeoutRecheck != nil && !setFlags["timeout-recheck"] {
		cfg.TimeoutRecheck = *fc.TimeoutRecheck
	}
	if fc.BuildTimeout != nil && !setFlags["build-timeout"] {
		cfg.BuildTimeout = *fc.BuildTimeout
	}
	if fc.Full != nil && !setFlags["full"] {
		cfg.Full = *fc.Full
	}
	if fc.SourceOnly != nil && !setFlags["source-only"] {
		cfg.SourceOnly = *fc.SourceOnly
	}
	if fc.IncludeGenerated != nil && !setFlags["include-generated"] {
		cfg.IncludeGenerated = *fc.IncludeGenerated
	}
	if fc.Conventional != nil && !setFlags["conventional"] {
		cfg.Conventional = *fc.Conventional
	}
	if fc.VerifySurvivors != nil && !setFlags["verify-survivors"] {
		cfg.VerifySurvivors = *fc.VerifySurvivors
	}
	if fc.VerifyWitnesses != nil && !setFlags["verify-witnesses"] {
		cfg.VerifyWitnesses = *fc.VerifyWitnesses
	}
	if fc.SuggestAssertions != nil && !setFlags["suggest-assertions"] {
		cfg.SuggestAssertions = *fc.SuggestAssertions
	}
	if fc.ShowUnreached != nil && !setFlags["show-unreached"] {
		cfg.ShowUnreached = *fc.ShowUnreached
	}
	if fc.UndoChanges != nil && !setFlags["undo-changes"] {
		cfg.UndoChanges = *fc.UndoChanges
	}
	if fc.KilledFindings != nil && !setFlags["show-killed"] {
		cfg.KilledFindings = *fc.KilledFindings
	}
	if fc.ShowNonViable != nil && !setFlags["show-non-viable"] {
		*showNonViable = *fc.ShowNonViable
	}
	if fc.Audit != nil && !setFlags["audit"] {
		cfg.Audit = *fc.Audit
	}
	if fc.Format != nil && !setFlags["format"] {
		*format = *fc.Format
	}
	if fc.Baseline != nil && !setFlags["baseline"] {
		*baselinePath = resolvePath(*fc.Baseline)
	}
	if fc.Checkpoint != nil && !setFlags["checkpoint"] {
		cfg.Checkpoint = resolvePath(*fc.Checkpoint)
	}
}

func stripTOMLComment(line string) string {
	inDouble, inSingle, escaped := false, false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '#':
			return line[:i]
		}
	}
	return line
}

func arrayClosed(s string) bool {
	inDouble, inSingle, escaped := false, false, false
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return strings.TrimSpace(s[i+1:]) == ""
			}
		}
	}
	return false
}

func parseTOMLString(val string) (string, error) {
	if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
		inner := val[1 : len(val)-1]
		if strings.ContainsRune(inner, '\'') {
			return "", fmt.Errorf("invalid literal string %s", val)
		}
		return inner, nil
	}
	if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
		s, err := strconv.Unquote(val)
		if err != nil {
			return "", fmt.Errorf("invalid string %s: %w", val, err)
		}
		return s, nil
	}
	return "", fmt.Errorf("expected string, got %q", val)
}

func parseTOMLBool(val string) (bool, error) {
	switch val {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("expected boolean, got %q", val)
	}
}

func parseTOMLInt(val string) (int, error) {
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("expected integer, got %q", val)
	}
	return n, nil
}

func parseTOMLStringArray(val string) ([]string, error) {
	if !strings.HasPrefix(val, "[") || !strings.HasSuffix(val, "]") {
		return nil, fmt.Errorf("expected array, got %q", val)
	}
	inner := strings.TrimSpace(val[1 : len(val)-1])
	if inner == "" {
		return nil, nil
	}
	var out []string
	start := 0
	inDouble, inSingle, escaped := false, false, false
	for i := 0; i <= len(inner); i++ {
		if i < len(inner) {
			c := inner[i]
			if escaped {
				escaped = false
				continue
			}
			if inDouble {
				if c == '\\' {
					escaped = true
				} else if c == '"' {
					inDouble = false
				}
				continue
			}
			if inSingle {
				if c == '\'' {
					inSingle = false
				}
				continue
			}
			if c == '"' {
				inDouble = true
				continue
			}
			if c == '\'' {
				inSingle = true
				continue
			}
			if c != ',' {
				continue
			}
		}
		item := strings.TrimSpace(inner[start:i])
		start = i + 1
		if item == "" {
			if i == len(inner) {
				break
			}
			return nil, fmt.Errorf("empty array element")
		}
		s, err := parseTOMLString(item)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
