package reporting

import (
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/t-k/targeted-mutation-go/internal/campaign"
)

const sarifSchema = "https://docs.oasis-open.org/sarif/sarif/v2.1.0/errata01/os/schemas/sarif-schema-2.1.0.json"
const sarifSourceRoot = "%SRCROOT%"

// SARIFPaths describes the roots used to resolve findings and emit relative URIs.
// RepositoryDir defaults to ProjectDir. WorkspaceDirs lists additional module
// roots outside RepositoryDir (such as go.work siblings) allowed under %SRCROOT%.
// Without ProjectDir, source columns are omitted.
type SARIFPaths struct {
	ProjectDir    string
	RepositoryDir string
	WorkspaceDirs []string
}

type sarifOutcome struct {
	text, kind, level string
}

var sarifOutcomes = map[string]sarifOutcome{
	"input-gap":          {"The test inputs leave a distinguishable mutation alive", "fail", "warning"},
	"assertion-gap":      {"A test passed after the mutation changed behavior", "fail", "warning"},
	"source-survivor":    {"The source mutation survived the tests", "fail", "warning"},
	"untested-change":    {"The tests passed after restoring the previous source", "fail", "warning"},
	"weakened-test":      {"The current tests miss a mutation the previous tests detected", "fail", "warning"},
	"killed":             {"The tests detected the mutation", "pass", "none"},
	"non-viable":         {"The mutation could not be compiled", "notApplicable", "none"},
	"unreached":          {"The tests did not reach the site", "notApplicable", "none"},
	"unstable":           {"The replay produced inconsistent evidence", "fail", "error"},
	"timeout":            {"The mutation evaluation timed out", "fail", "error"},
	"build-error":        {"The build could not complete the mutation evaluation", "fail", "error"},
	"error":              {"The mutation evaluation failed", "fail", "error"},
	"cancelled":          {"The mutation evaluation was cancelled", "fail", "error"},
	"candidate":          {"A mutation candidate was listed", "informational", "none"},
	"not-evaluated":      {"The mutation was not evaluated", "fail", "error"},
	"audit-disagreement": {"The probe verdict disagreed with the source replay", "fail", "error"},
	"other":              {"The campaign reported a finding", "informational", "none"},
}

type sarifRule struct {
	ID               string       `json:"id"`
	ShortDescription sarifMessage `json:"shortDescription"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID              string            `json:"ruleId"`
	Kind                string            `json:"kind"`
	Level               string            `json:"level"`
	Message             sarifMessage      `json:"message"`
	PartialFingerprints map[string]string `json:"partialFingerprints,omitempty"`
	Locations           []sarifLocation   `json:"locations,omitempty"`
	Properties          sarifFinding      `json:"properties"`
}

type sarifFinding struct {
	Finding campaign.Finding `json:"finding"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           *sarifRegion          `json:"region,omitempty"`
}

type sarifArtifactLocation struct {
	URI       string `json:"uri"`
	URIBaseID string `json:"uriBaseId,omitempty"`
}

type sarifRegion struct {
	StartLine   int `json:"startLine"`
	StartColumn int `json:"startColumn,omitempty"`
}

// WriteSARIF writes a SARIF 2.1.0 log. Paths may be supplied to resolve source locations.
func WriteSARIF(w io.Writer, report campaign.Report, id func(campaign.Finding) string, paths ...SARIFPaths) error {
	var project SARIFPaths
	if len(paths) != 0 {
		project = paths[0]
	}
	effectiveBase, workspaceDirs := project.resolveBases()
	results := make([]sarifResult, 0, len(report.Findings))
	rules := make(map[string]sarifRule)
	var source sarifSource
	for _, finding := range report.Findings {
		ruleID := finding.Status
		outcome, ok := sarifOutcomes[ruleID]
		if !ok {
			ruleID, outcome = "other", sarifOutcomes["other"]
		}
		rules[ruleID] = sarifRule{ID: ruleID, ShortDescription: sarifMessage{Text: outcome.text}}
		result := sarifResult{
			RuleID: ruleID, Kind: outcome.kind, Level: outcome.level,
			Message: sarifMessage{Text: sarifText(outcome.text, finding)}, Properties: sarifFinding{Finding: finding},
		}
		if stableID := findingID(finding, id); stableID != "" {
			result.PartialFingerprints = map[string]string{"targetedMutationId/v1": stableID}
		}
		if uri := artifactURIWithBases(finding.File, project, effectiveBase, workspaceDirs); uri != "" {
			location := sarifPhysicalLocation{
				ArtifactLocation: sarifArtifactLocation{URI: uri, URIBaseID: sarifSourceRoot},
			}
			if finding.Line > 0 {
				location.Region = &sarifRegion{StartLine: finding.Line, StartColumn: source.column(finding, project, workspaceDirs)}
			}
			result.Locations = []sarifLocation{{PhysicalLocation: location}}
		}
		results = append(results, result)
	}
	descriptors := make([]sarifRule, 0, len(rules))
	for _, rule := range rules {
		descriptors = append(descriptors, rule)
	}
	slices.SortFunc(descriptors, func(a, b sarifRule) int { return strings.Compare(a.ID, b.ID) })
	run := map[string]any{
		"tool": map[string]any{"driver": map[string]any{
			"name": "targeted-mutation-go", "rules": descriptors,
		}},
		"columnKind":  "utf16CodeUnits",
		"results":     results,
		"invocations": []map[string]bool{{"executionSuccessful": report.Incomplete == 0}},
		"properties":  map[string]any{"report": reportSummary{Report: report}},
	}
	if effectiveBase != "" {
		path := strings.TrimRight(filepath.ToSlash(effectiveBase), "/") + "/"
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		uri := url.URL{Scheme: "file", Path: path}
		run["originalUriBaseIds"] = map[string]sarifArtifactLocation{sarifSourceRoot: {URI: uri.String()}}
	}
	log := map[string]any{"$schema": sarifSchema, "version": "2.1.0", "runs": []any{run}}
	return json.NewEncoder(w).Encode(log)
}

func sarifText(title string, finding campaign.Finding) string {
	parts := []string{title}
	if finding.Expression != "" || finding.Mutation != "" {
		parts = append(parts, "Original expression\n"+finding.Expression+"\nMutation\n"+finding.Mutation)
	}
	if finding.Detail != "" {
		parts = append(parts, finding.Detail)
	}
	return strings.Join(parts, "\n")
}

func artifactURI(file string, paths SARIFPaths) string {
	effectiveBase, workspaceDirs := paths.resolveBases()
	return artifactURIWithBases(file, paths, effectiveBase, workspaceDirs)
}

func artifactURIWithBases(file string, paths SARIFPaths, effectiveBase string, workspaceDirs []string) string {
	if file == "" {
		return ""
	}
	relative := file
	if paths.ProjectDir != "" {
		source := resolveSource(paths.ProjectDir, file)
		repoBase := paths.repository()
		if source == "" || repoBase == "" || effectiveBase == "" {
			return ""
		}
		if !isWithinDir(repoBase, source) && !isWithinAnyDir(source, workspaceDirs) {
			return ""
		}
		if !isPhysicallyAllowed(source, repoBase, workspaceDirs) {
			return ""
		}
		var err error
		relative, err = filepath.Rel(effectiveBase, source)
		if err != nil {
			return ""
		}
	}
	if !filepath.IsLocal(relative) || filepath.Clean(relative) == "." {
		return ""
	}
	uri := url.URL{Path: filepath.ToSlash(filepath.Clean(relative))}
	return uri.String()
}

func (paths SARIFPaths) resolveBases() (string, []string) {
	base := paths.repository()
	if base == "" || len(paths.WorkspaceDirs) == 0 {
		return base, nil
	}
	effective := base
	var allowed []string
	for _, raw := range paths.WorkspaceDirs {
		if raw == "" {
			continue
		}
		dir := resolveSource(paths.ProjectDir, raw)
		if dir == "" || !isExistingDir(dir) {
			continue
		}
		common := commonDir(effective, dir)
		if common == "" {
			continue
		}
		if isFSRoot(common) && !isFSRoot(effective) && !isFSRoot(dir) {
			continue
		}
		effective = common
		allowed = append(allowed, dir)
	}
	return effective, allowed
}

func commonDir(a, b string) string {
	a, b = filepath.Clean(a), filepath.Clean(b)
	volA, volB := filepath.VolumeName(a), filepath.VolumeName(b)
	if volA != volB {
		return ""
	}
	sep := string(filepath.Separator)
	restA := strings.TrimPrefix(a[len(volA):], sep)
	restB := strings.TrimPrefix(b[len(volB):], sep)
	var partsA, partsB []string
	if restA != "" {
		partsA = strings.Split(restA, sep)
	}
	if restB != "" {
		partsB = strings.Split(restB, sep)
	}
	limit := min(len(partsA), len(partsB))
	i := 0
	for i < limit && partsA[i] == partsB[i] {
		i++
	}
	if i == 0 {
		return volA + sep
	}
	return volA + sep + filepath.Join(partsA[:i]...)
}

func isFSRoot(dir string) bool {
	clean := filepath.Clean(dir)
	return clean == filepath.Dir(clean)
}

func isExistingDir(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

func isWithinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	clean := filepath.Clean(rel)
	return filepath.IsLocal(clean) && clean != "."
}

func isWithinAnyDir(target string, dirs []string) bool {
	for _, dir := range dirs {
		if isWithinDir(dir, target) {
			return true
		}
	}
	return false
}

func (paths SARIFPaths) repository() string {
	if paths.RepositoryDir != "" {
		return resolveSource("", paths.RepositoryDir)
	}
	if paths.ProjectDir != "" {
		return resolveSource("", paths.ProjectDir)
	}
	return ""
}

func resolveSource(project, file string) string {
	if !filepath.IsAbs(file) {
		file = filepath.Join(project, file)
	}
	resolved, err := filepath.Abs(file)
	if err != nil {
		return ""
	}
	return resolved
}

func isPhysicallyAllowed(source, repoBase string, workspaceDirs []string) bool {
	realSource, existing, ok := evalNearestExisting(source)
	if !ok {
		return false
	}
	if !existing {
		return true
	}
	if realRepo, err := filepath.EvalSymlinks(repoBase); err == nil && isWithinDir(realRepo, realSource) {
		return true
	}
	for _, dir := range workspaceDirs {
		if realDir, err := filepath.EvalSymlinks(dir); err == nil && isWithinDir(realDir, realSource) {
			return true
		}
	}
	return false
}

func evalNearestExisting(path string) (resolved string, existing bool, ok bool) {
	cur := filepath.Clean(path)
	var suffix []string
	for {
		realCur, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if len(suffix) > 0 {
				slices.Reverse(suffix)
				realCur = filepath.Join(append([]string{realCur}, suffix...)...)
			}
			return realCur, true, true
		}
		if !os.IsNotExist(err) {
			return "", false, false
		}
		if linfo, lerr := os.Lstat(cur); lerr == nil && linfo.Mode()&os.ModeSymlink != 0 {
			return "", false, false
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false, true
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

type sarifSource struct {
	file  string
	lines []string
}

func (s *sarifSource) column(finding campaign.Finding, paths SARIFPaths, workspaceDirs []string) int {
	if paths.ProjectDir == "" || finding.Column <= 0 {
		return 0
	}
	file := resolveSource(paths.ProjectDir, finding.File)
	repoBase := paths.repository()
	if file == "" || repoBase == "" || !isPhysicallyAllowed(file, repoBase, workspaceDirs) {
		return 0
	}
	if s.file != file {
		s.file, s.lines = file, nil
		data, err := os.ReadFile(file)
		if err == nil {
			s.lines = strings.Split(string(data), "\n")
		}
	}
	if finding.Line > len(s.lines) || finding.Column > len(s.lines[finding.Line-1])+1 {
		return 0
	}
	prefix := s.lines[finding.Line-1][:finding.Column-1]
	if !utf8.ValidString(prefix) {
		return 0
	}
	// Go reports UTF-8 byte offsets. SARIF counts UTF-16 code units.
	column := 1
	for _, rune := range prefix {
		column++
		if rune > 0xffff {
			column++
		}
	}
	return column
}
