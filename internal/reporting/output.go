// Package reporting writes CI report formats and stores known findings.
package reporting

import (
	"encoding/json"
	"io"

	"github.com/t-k/targeted-mutation-go/internal/campaign"
)

const jsonlSchema = 1

type reportSummary struct {
	campaign.Report
	Findings []campaign.Finding `json:"findings,omitempty"`
}

// WriteJSONL writes a summary followed by one record per finding.
func WriteJSONL(w io.Writer, report campaign.Report, id func(campaign.Finding) string) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	header := struct {
		Type   string        `json:"type"`
		Schema int           `json:"schema"`
		Report reportSummary `json:"report"`
	}{"summary", jsonlSchema, reportSummary{Report: report}}
	if err := encoder.Encode(header); err != nil {
		return err
	}
	for _, finding := range report.Findings {
		record := struct {
			Type    string           `json:"type"`
			Schema  int              `json:"schema"`
			ID      string           `json:"id,omitempty"`
			Finding campaign.Finding `json:"finding"`
		}{"finding", jsonlSchema, findingID(finding, id), finding}
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return nil
}

func findingID(f campaign.Finding, id func(campaign.Finding) string) string {
	if id == nil {
		return ""
	}
	return id(f)
}

// IsGap reports whether a finding represents a confirmed testing deficiency.
func IsGap(status string) bool {
	switch status {
	case "input-gap", "assertion-gap", "source-survivor", "untested-change", "weakened-test":
		return true
	}
	return false
}
