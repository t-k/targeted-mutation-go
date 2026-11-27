package reporting

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/campaign"
)

const baselineSchema = 1

// Baseline holds the stable IDs of previously accepted gaps.
type Baseline struct {
	ids map[string]struct{}
}

type baselineDocument struct {
	Schema int      `json:"schema"`
	IDs    []string `json:"ids"`
}

// Suppresses reports whether a known gap may be hidden without hiding errors.
func (b Baseline) Suppresses(finding campaign.Finding, id func(campaign.Finding) string) bool {
	if !IsGap(finding.Status) {
		return false
	}
	_, known := b.ids[findingID(finding, id)]
	return known
}

// ReadBaseline loads a versioned stable-ID snapshot.
func ReadBaseline(path string) (Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Baseline{}, err
	}
	var document baselineDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return Baseline{}, fmt.Errorf("baseline %q: %w", path, err)
	}
	if document.Schema != baselineSchema {
		return Baseline{}, fmt.Errorf("baseline %q has unsupported schema %d", path, document.Schema)
	}
	if document.IDs == nil {
		return Baseline{}, fmt.Errorf("baseline %q is missing an ids array", path)
	}
	known := Baseline{ids: make(map[string]struct{}, len(document.IDs))}
	for _, id := range document.IDs {
		if strings.TrimSpace(id) == "" {
			return Baseline{}, fmt.Errorf("baseline %q contains an empty ID", path)
		}
		known.ids[id] = struct{}{}
	}
	return known, nil
}

// WriteBaseline saves one complete set of known gaps in stable ID order.
func WriteBaseline(path string, report campaign.Report, id func(campaign.Finding) string) error {
	_, err := WriteBaselineCount(path, report, id)
	return err
}

// WriteBaselineCount saves one complete set of known gaps in stable ID order
// and returns the number of unique IDs written.
func WriteBaselineCount(path string, report campaign.Report, id func(campaign.Finding) string) (int, error) {
	document := baselineDocument{Schema: baselineSchema, IDs: []string{}}
	for _, finding := range report.Findings {
		if !IsGap(finding.Status) {
			continue
		}
		stableID := findingID(finding, id)
		if strings.TrimSpace(stableID) == "" {
			return 0, fmt.Errorf("baseline %q: gap at %s:%d has no stable ID", path, finding.File, finding.Line)
		}
		document.IDs = append(document.IDs, stableID)
	}
	slices.Sort(document.IDs)
	document.IDs = slices.Compact(document.IDs)
	if err := publishBaseline(path, document); err != nil {
		return 0, err
	}
	return len(document.IDs), nil
}

func publishBaseline(path string, document baselineDocument) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".tmt-baseline-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
