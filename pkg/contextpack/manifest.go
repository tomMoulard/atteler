package contextpack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/llm"
)

const unknownTimestamp = "unknown"

// Manifest captures audit metadata for content omitted during compaction.
type Manifest struct {
	Estimator       string         `json:"estimator"`
	Policy          string         `json:"policy"`
	Budget          string         `json:"budget"`
	TruncatedReason string         `json:"truncated_reason,omitempty"`
	Ranges          []OmittedRange `json:"ranges"`
	Items           []OmittedItem  `json:"items,omitempty"`
	OmittedCount    int            `json:"omitted_count"`
	RangeCount      int            `json:"range_count"`
	Truncated       bool           `json:"truncated"`
}

// OmittedRange describes a contiguous original-message index range that was
// omitted from the compacted output.
type OmittedRange struct {
	Roles      []llm.Role `json:"roles"`
	StartIndex int        `json:"start_index"`
	EndIndex   int        `json:"end_index"`
}

// OmittedItem is the replay/audit record for one omitted message.
type OmittedItem struct {
	Role        llm.Role      `json:"role"`
	Timestamp   string        `json:"timestamp"`
	Hash        string        `json:"hash"`
	Summary     string        `json:"summary"`
	WhyDropped  string        `json:"why_dropped"`
	Signals     []string      `json:"signals,omitempty"`
	TokenBudget TokenEstimate `json:"token_budget"`
	Index       int           `json:"index"`
}

type markerBuildOptions struct {
	itemLimit    int
	rangeLimit   int
	summaryRunes int
}

func buildManifestMarker(
	messages []llm.Message,
	omitted []int,
	analyses []messageAnalysis,
	metadata []MessageMetadata,
	estimator Estimator,
	policy Policy,
	maxMarkerUpperBound int,
	maxTokens int,
) (llm.Message, Manifest, bool) {
	if len(omitted) == 0 {
		return llm.Message{}, Manifest{}, false
	}

	if maxMarkerUpperBound <= 0 {
		return llm.Message{}, Manifest{}, false
	}

	maxItems := min(policy.ManifestMaxItems, len(omitted))

	maxRanges := policy.ManifestMaxRanges
	if maxRanges <= 0 {
		maxRanges = len(omitted)
	}

	maxRanges = min(maxRanges, len(omitted))

	summaryOptions := uniquePositiveInts(policy.ManifestSummaryRunes, 96, 64, 32, 16, 0)
	for _, summaryRunes := range summaryOptions {
		for itemLimit := maxItems; itemLimit >= 1; itemLimit-- {
			for rangeLimit := maxRanges; rangeLimit >= 1; rangeLimit-- {
				marker, manifest := newManifestMarker(messages, omitted, analyses, metadata, estimator, policy, maxTokens, markerBuildOptions{
					itemLimit:    itemLimit,
					rangeLimit:   rangeLimit,
					summaryRunes: summaryRunes,
				})

				if estimator.EstimateMessage(marker).UpperBoundTokens <= maxMarkerUpperBound {
					return marker, manifest, true
				}
			}
		}
	}

	return llm.Message{}, Manifest{}, false
}

func newManifestMarker(
	messages []llm.Message,
	omitted []int,
	analyses []messageAnalysis,
	metadata []MessageMetadata,
	estimator Estimator,
	policy Policy,
	maxTokens int,
	build markerBuildOptions,
) (llm.Message, Manifest) {
	profile := estimator.Profile()
	ranges, rangeCount := omittedRanges(messages, omitted, build.rangeLimit)
	items := make([]OmittedItem, 0, min(build.itemLimit, len(omitted)))

	for _, index := range omitted {
		if len(items) >= build.itemLimit {
			break
		}

		analysis := analyses[index]
		items = append(items, OmittedItem{
			Index:       index,
			Role:        messages[index].Role,
			Timestamp:   timestampForIndex(metadata, index),
			Hash:        messageHash(messages[index]),
			Summary:     summarizeContent(messages[index].Content, build.summaryRunes),
			WhyDropped:  droppedReason(analysis, policy),
			Signals:     append([]string(nil), analysis.Signals...),
			TokenBudget: estimator.EstimateMessage(messages[index]),
		})
	}

	truncated := len(items) < len(omitted) || len(ranges) < rangeCount || build.summaryRunes < policy.ManifestSummaryRunes

	manifest := Manifest{
		Estimator:    estimatorProfileSummary(profile),
		Policy:       policy.summary(),
		Budget:       fmt.Sprintf("max=%d;upper_bound", maxTokens),
		OmittedCount: len(omitted),
		RangeCount:   rangeCount,
		Ranges:       ranges,
		Items:        items,
		Truncated:    truncated,
	}
	if truncated {
		manifest.TruncatedReason = "fit-budget"
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		data = fmt.Appendf(nil, `{"omitted_count":%d,"error":"manifest marshal failed"}`, len(omitted))
	}

	return llm.Message{
		Role:    llm.RoleSystem,
		Content: "[context evidence manifest]\n" + string(data),
	}, manifest
}

func timestampForIndex(metadata []MessageMetadata, index int) string {
	if index >= 0 && index < len(metadata) && strings.TrimSpace(metadata[index].Timestamp) != "" {
		return strings.TrimSpace(metadata[index].Timestamp)
	}

	return unknownTimestamp
}

func omittedRanges(messages []llm.Message, omitted []int, limit int) (ranges []OmittedRange, rangeCount int) {
	if len(omitted) == 0 {
		return nil, 0
	}

	start := omitted[0]
	end := omitted[0]
	roles := []llm.Role{messages[omitted[0]].Role}

	flush := func() {
		rangeCount++

		if limit <= 0 || len(ranges) >= limit {
			return
		}

		ranges = append(ranges, OmittedRange{
			StartIndex: start,
			EndIndex:   end,
			Roles:      uniqueRoles(roles),
		})
	}

	for _, index := range omitted[1:] {
		if index == end+1 {
			end = index
			roles = append(roles, messages[index].Role)

			continue
		}

		flush()

		start = index
		end = index
		roles = []llm.Role{messages[index].Role}
	}

	flush()

	return ranges, rangeCount
}

func uniqueRoles(roles []llm.Role) []llm.Role {
	seen := make(map[llm.Role]struct{}, len(roles))
	out := make([]llm.Role, 0, len(roles))

	for _, role := range roles {
		if _, ok := seen[role]; ok {
			continue
		}

		seen[role] = struct{}{}
		out = append(out, role)
	}

	return out
}

func messageHash(msg llm.Message) string {
	data, err := json.Marshal(msg)
	if err != nil {
		data = []byte(string(msg.Role) + "\x00" + msg.Content)
	}

	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

func summarizeContent(content string, maxRunes int) string {
	content = strings.TrimSpace(strings.Join(strings.Fields(content), " "))
	if content == "" || maxRunes == 0 {
		return ""
	}

	if maxRunes < 0 || utf8.RuneCountInString(content) <= maxRunes {
		return content
	}

	runes := []rune(content)
	if maxRunes <= 1 {
		return string(runes[:maxRunes])
	}

	return string(runes[:maxRunes-1]) + "…"
}

func droppedReason(analysis messageAnalysis, policy Policy) string {
	if len(analysis.Signals) == 0 {
		return "lower score than retained messages under " + policy.summary()
	}

	return "lower score after preserving stronger evidence; signals=" + strings.Join(analysis.Signals, ",")
}

func estimatorProfileSummary(profile EstimatorProfile) string {
	parts := []string{
		profile.Name,
		"provider=" + profile.Provider,
		fmt.Sprintf("cpt=%d", profile.CharsPerToken),
		fmt.Sprintf("overhead=%d", profile.MessageOverheadTokens),
		fmt.Sprintf("err=%d%%", profile.ErrorBoundPercent),
	}
	if profile.Model != "" {
		parts = append(parts, "model="+profile.Model)
	}

	return strings.Join(parts, ";")
}

func uniquePositiveInts(values ...int) []int {
	seen := make(map[int]struct{}, len(values))

	out := make([]int, 0, len(values))
	for _, value := range values {
		if value < 0 {
			continue
		}

		if _, ok := seen[value]; ok {
			continue
		}

		seen[value] = struct{}{}
		out = append(out, value)
	}

	return out
}
