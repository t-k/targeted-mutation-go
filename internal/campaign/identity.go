package campaign

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"github.com/t-k/targeted-mutation-go/internal/target"
)

type sourceAnchor struct {
	key        string
	occurrence int
	indent     int
}

func sourceAnchors(data []byte, owners []target.Declaration) []sourceAnchor {
	type anchorKey struct{ owner, code string }
	seen := make(map[anchorKey]int)
	keys := make(map[anchorKey]string)
	var anchors []sourceAnchor
	for line := range strings.SplitSeq(string(data), "\n") {
		number := len(anchors) + 1
		for len(owners) != 0 && owners[0].End < number {
			owners = owners[1:]
		}
		key := anchorKey{code: strings.TrimSpace(line)}
		if len(owners) != 0 && owners[0].Start <= number {
			key.owner = owners[0].Name
		}
		id, exists := keys[key]
		if !exists {
			hash := sha256.Sum256([]byte(key.owner + "\x00" + key.code))
			id = hex.EncodeToString(hash[:16])
			keys[key] = id
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		anchors = append(anchors, sourceAnchor{key: id, occurrence: seen[key], indent: indent})
		seen[key]++
	}
	return anchors
}

func (s *session) identify(f *Finding) {
	var anchor sourceAnchor
	if lines := s.anchors[f.File]; f.Line > 0 && f.Line <= len(lines) {
		anchor = lines[f.Line-1]
	} else {
		anchor.occurrence = f.Line
	}
	data, _ := json.Marshal([]any{f.File, f.Rule, f.Expression, f.Mutation, anchor.key,
		anchor.occurrence, max(0, f.Column-anchor.indent)})
	hash := sha256.Sum256(data)
	f.ID = hex.EncodeToString(hash[:16])
}

type shardSelection struct{ index, total uint64 }

func parseShard(raw string) (shardSelection, error) {
	if raw == "" {
		return shardSelection{}, nil
	}
	first, second, found := strings.Cut(raw, "/")
	index, indexErr := strconv.ParseUint(first, 10, 32)
	total, totalErr := strconv.ParseUint(second, 10, 32)
	if !found || indexErr != nil || totalErr != nil || index >= total {
		return shardSelection{}, fmt.Errorf("invalid shard %q: expected K/N with 0 <= K < N", raw)
	}
	return shardSelection{index: index, total: total}, nil
}

func (s *session) enabled(f Finding) bool {
	if slices.Contains(s.cfg.ExcludeRules, f.Rule) {
		return false
	}
	if s.shard.total <= 1 {
		return true
	}
	hash := sha256.Sum256([]byte(f.ID))
	return binary.BigEndian.Uint64(hash[:8])%s.shard.total == s.shard.index
}

func (s *session) comparisonEnabled(kind string) bool {
	return s.cfg.Comparison == "" || s.cfg.Comparison == "all" || s.cfg.Comparison == kind
}

func (s *session) preview(report *Report) {
	report.Evaluation = "listed"
	for _, site := range s.sites {
		if site.String || site.Arith {
			continue
		}
		for _, candidate := range analysis.PreviewSite(site.Op, site.Domain, site.Margin, site.Float, !s.cfg.Conventional) {
			f := s.describe(probeJob{site: site, candidate: candidate}, analysis.Summary{})
			if s.comparisonEnabled(candidate.Kind) && s.enabled(f) {
				if s.isIgnored(f) {
					report.Suppressed++
					continue
				}
				f.Status = "candidate"
				report.Findings = append(report.Findings, f)
			}
		}
	}
	for _, source := range s.sources {
		f := source.finding
		if s.isIgnored(f) {
			report.Suppressed++
			continue
		}
		f.Status = "candidate"
		report.Findings = append(report.Findings, f)
	}
	slices.SortStableFunc(report.Findings, compareFindings)
}
