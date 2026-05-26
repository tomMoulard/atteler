// Package contextpack provides dependency-free helpers for fitting chat context
// into a provider/model-aware token budget while preserving auditable evidence
// about omitted messages.
package contextpack

import (
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/tommoulard/atteler/pkg/llm"
)

// Stats describes the result of a context compaction pass.
type Stats struct {
	BudgetFailureReason         string
	BudgetFailureReasonCode     string
	Estimator                   string
	Provider                    string
	Model                       string
	Policy                      string
	OriginalCount               int
	OutputCount                 int
	OmittedCount                int
	OriginalEstimatedTokens     int
	OutputEstimatedTokens       int
	MaxEstimatedTokens          int
	OriginalEstimateErrorBound  int
	OutputEstimateErrorBound    int
	OriginalEstimatedUpperBound int
	OutputEstimatedUpperBound   int
	Compressed                  bool
	BudgetChecked               bool
	FitsBudget                  bool
	HardBudgetFailure           bool
}

// Result is the compacted message list plus accounting metadata.
type Result struct {
	Manifest Manifest
	Messages []llm.Message
	Stats    Stats
}

// MessageMetadata carries optional audit metadata for a message.
type MessageMetadata struct {
	Timestamp string
	Pinned    bool
	Priority  int
}

// Options configures a compaction pass.
type Options struct {
	Estimator Estimator
	Provider  string
	Model     string
	Metadata  []MessageMetadata
	Policy    Policy
	MaxTokens int
}

// Policy controls how compaction trades recency, role priority, and evidence
// importance. Pinned evidence is preserved by default; if pinned/system content
// cannot fit with an evidence manifest, compaction reports a hard budget
// failure instead of silently amputating it.
type Policy struct {
	RolePriority         map[llm.Role]int
	RecencyWeight        int
	ImportanceWeight     int
	ManifestMaxItems     int
	ManifestMaxRanges    int
	ManifestSummaryRunes int
	DropPinnedWhenNeeded bool
}

type messageAnalysis struct {
	Signals  []string
	Pinned   bool
	Priority int
}

type scoredMessage struct {
	index int
	score int
}

type outputCandidate struct {
	messages []llm.Message
	manifest Manifest
	estimate TokenEstimate
	omitted  int
}

// Compact reduces messages to fit maxEstimatedTokens when possible. It uses the
// default conservative estimator. Prefer CompactWithOptions when the target
// provider/model is known.
//
// A non-positive maxEstimatedTokens value disables compaction unless the target
// model is known through CompactWithOptions, in which case the model context
// window is used as the budget.
func Compact(messages []llm.Message, maxEstimatedTokens int) Result {
	return CompactWithOptions(messages, Options{MaxTokens: maxEstimatedTokens})
}

// CompactWithOptions reduces messages to fit a provider/model-aware token
// budget while preserving system messages and pinned evidence. The compaction
// decision is reproducible from the returned stats and omission manifest.
func CompactWithOptions(messages []llm.Message, opts Options) Result {
	estimator := opts.Estimator
	if estimator == nil {
		estimator = NewEstimator(opts.Provider, opts.Model)
	}

	policy := opts.Policy.withDefaults()
	metadata := opts.Metadata
	profile := estimator.Profile()
	maxTokens := effectiveMaxTokens(opts.MaxTokens, profile)
	originalEstimate := estimator.EstimateMessages(messages)
	baseStats := newStats(messages, originalEstimate, maxTokens, profile, policy)

	if maxTokens <= 0 || originalEstimate.UpperBoundTokens <= maxTokens {
		out := cloneMessages(messages)
		outEstimate := estimator.EstimateMessages(out)
		stats := finishStats(baseStats, out, outEstimate, 0, maxTokens)

		return Result{Messages: out, Stats: stats}
	}

	analyses := analyzeMessages(messages, metadata)

	selected, failureReason, ok := selectRequiredMessages(messages, analyses, estimator, policy, maxTokens)
	if !ok {
		return hardBudgetResult(messages, baseStats, originalEstimate, failureReason)
	}

	best, ok := buildOutputCandidate(messages, selected, analyses, metadata, estimator, policy, maxTokens)
	if !ok {
		selectedUpper := selectedMessagesUpperBound(messages, selected, estimator)
		reason := fmt.Sprintf("preserved evidence requires %d upper-bound tokens before an omission manifest; max %d", selectedUpper, maxTokens)

		return hardBudgetResult(messages, baseStats, originalEstimate, reason)
	}

	best = addBestFittingCandidates(messages, selected, analyses, metadata, estimator, policy, maxTokens, best)
	stats := finishStats(baseStats, best.messages, best.estimate, best.omitted, maxTokens)
	stats.Compressed = best.omitted > 0

	return Result{
		Messages: best.messages,
		Stats:    stats,
		Manifest: best.manifest,
	}
}

func selectRequiredMessages(
	messages []llm.Message,
	analyses []messageAnalysis,
	estimator Estimator,
	policy Policy,
	maxTokens int,
) (selected []bool, failureReason string, ok bool) {
	selected = make([]bool, len(messages))
	for i, msg := range messages {
		if msg.Role == llm.RoleSystem {
			selected[i] = true
		}
	}

	systemUpper := selectedMessagesUpperBound(messages, selected, estimator)
	if systemUpper > maxTokens {
		reason := fmt.Sprintf("preserved system messages require %d upper-bound tokens, max %d", systemUpper, maxTokens)

		return selected, reason, false
	}

	if policy.DropPinnedWhenNeeded {
		return selected, "", true
	}

	for i, msg := range messages {
		if msg.Role != llm.RoleSystem && analyses[i].Pinned {
			selected[i] = true
		}
	}

	pinnedUpper := selectedMessagesUpperBound(messages, selected, estimator)
	if pinnedUpper > maxTokens {
		reason := fmt.Sprintf("preserved system and pinned evidence require %d upper-bound tokens, max %d", pinnedUpper, maxTokens)

		return selected, reason, false
	}

	return selected, "", true
}

func addBestFittingCandidates(
	messages []llm.Message,
	selected []bool,
	analyses []messageAnalysis,
	metadata []MessageMetadata,
	estimator Estimator,
	policy Policy,
	maxTokens int,
	best outputCandidate,
) outputCandidate {
	for _, candidate := range rankCandidates(messages, selected, analyses, policy) {
		trial := append([]bool(nil), selected...)
		trial[candidate.index] = true

		trialOut, ok := buildOutputCandidate(messages, trial, analyses, metadata, estimator, policy, maxTokens)
		if !ok || trialOut.estimate.UpperBoundTokens > maxTokens {
			continue
		}

		selected = trial
		best = trialOut
	}

	return best
}

// OmissionMarker returns the legacy synthetic message used to represent trimmed
// conversation turns when no per-message manifest is available. Compact and
// CompactWithOptions use evidence manifests instead.
func OmissionMarker(omittedCount int) llm.Message {
	if omittedCount < 0 {
		omittedCount = 0
	}

	plural := "messages"
	if omittedCount == 1 {
		plural = "message"
	}

	return llm.Message{
		Role:    llm.RoleSystem,
		Content: fmt.Sprintf("[context compressed: %d older non-system %s omitted]", omittedCount, plural),
	}
}

func effectiveMaxTokens(requested int, profile EstimatorProfile) int {
	contextWindow := modelContextWindow(profile)
	if requested <= 0 {
		return contextWindow
	}

	if contextWindow > 0 && requested > contextWindow {
		return contextWindow
	}

	return requested
}

func modelContextWindow(profile EstimatorProfile) int {
	model := strings.TrimSpace(profile.Model)
	if model == "" {
		return 0
	}

	switch profile.Provider {
	case providerOpenAIName:
		return (&llm.OpenAIProvider{}).ModelContextWindow(model)
	case providerCodexName:
		return (&llm.CodexProvider{}).ModelContextWindow(model)
	case providerAnthropicName:
		return (&llm.AnthropicProvider{}).ModelContextWindow(model)
	case providerClaudeCodeName:
		return (&llm.ClaudeCodeProvider{}).ModelContextWindow(model)
	case providerOllamaName:
		return (&llm.OllamaProvider{}).ModelContextWindow(model)
	default:
		return 0
	}
}

// ModelContextWindow returns the known context window for a provider/model pair,
// or 0 when the provider or model is unknown. It uses the same provider/model
// normalization as NewEstimator so model strings like "openai/gpt-4" work even
// when the caller does not have a registered provider instance.
func ModelContextWindow(provider, model string) int {
	provider, model = normalizeEstimatorTarget(provider, model)

	return modelContextWindow(EstimatorProfile{Provider: provider, Model: model})
}

func newStats(messages []llm.Message, original TokenEstimate, maxTokens int, profile EstimatorProfile, policy Policy) Stats {
	return Stats{
		OriginalCount:               len(messages),
		OriginalEstimatedTokens:     original.Tokens,
		OriginalEstimateErrorBound:  original.ErrorBoundTokens,
		OriginalEstimatedUpperBound: original.UpperBoundTokens,
		MaxEstimatedTokens:          maxTokens,
		BudgetChecked:               maxTokens > 0,
		FitsBudget:                  maxTokens > 0 && original.UpperBoundTokens <= maxTokens,
		Estimator:                   estimatorProfileSummary(profile),
		Provider:                    profile.Provider,
		Model:                       profile.Model,
		Policy:                      policy.summary(),
	}
}

func finishStats(base Stats, out []llm.Message, outEstimate TokenEstimate, omitted, maxTokens int) Stats {
	base.OutputCount = len(out)
	base.OmittedCount = omitted
	base.OutputEstimatedTokens = outEstimate.Tokens
	base.OutputEstimateErrorBound = outEstimate.ErrorBoundTokens
	base.OutputEstimatedUpperBound = outEstimate.UpperBoundTokens
	base.FitsBudget = maxTokens > 0 && outEstimate.UpperBoundTokens <= maxTokens

	return base
}

func hardBudgetResult(messages []llm.Message, base Stats, estimate TokenEstimate, reason string) Result {
	out := cloneMessages(messages)
	stats := finishStats(base, out, estimate, 0, base.MaxEstimatedTokens)
	stats.HardBudgetFailure = true
	stats.FitsBudget = false
	stats.BudgetFailureReason = reason
	stats.BudgetFailureReasonCode = budgetFailureReasonCode(reason)

	return Result{Messages: out, Stats: stats}
}

func budgetFailureReasonCode(reason string) string {
	reason = strings.ToLower(strings.TrimSpace(reason))

	switch {
	case strings.Contains(reason, "system messages"):
		return "required_system_overflow"
	case strings.Contains(reason, "pinned evidence"):
		return "required_pinned_overflow"
	case strings.Contains(reason, "preserved evidence"):
		return "preserved_evidence_overflow"
	case strings.Contains(reason, "omission manifest"):
		return "omission_manifest_overflow"
	default:
		return "required_context_overflow"
	}
}

func buildOutputCandidate(
	messages []llm.Message,
	selected []bool,
	analyses []messageAnalysis,
	metadata []MessageMetadata,
	estimator Estimator,
	policy Policy,
	maxTokens int,
) (outputCandidate, bool) {
	omitted := omittedIndices(selected)
	if len(omitted) == 0 {
		out := cloneMessages(messages)
		estimate := estimator.EstimateMessages(out)

		return outputCandidate{messages: out, estimate: estimate}, estimate.UpperBoundTokens <= maxTokens
	}

	selectedUpper := selectedMessagesUpperBound(messages, selected, estimator)
	markerBudget := maxTokens - selectedUpper

	marker, manifest, ok := buildManifestMarker(messages, omitted, analyses, metadata, estimator, policy, markerBudget, maxTokens)
	if !ok {
		return outputCandidate{}, false
	}

	out := assembleOutput(messages, selected, marker)

	estimate := estimator.EstimateMessages(out)
	if estimate.UpperBoundTokens > maxTokens {
		return outputCandidate{}, false
	}

	return outputCandidate{
		messages: out,
		manifest: manifest,
		estimate: estimate,
		omitted:  len(omitted),
	}, true
}

func assembleOutput(messages []llm.Message, selected []bool, marker llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(messages)+1)
	markerAdded := false

	for i, msg := range messages {
		if selected[i] {
			out = append(out, msg)
			continue
		}

		if !markerAdded {
			out = append(out, marker)
			markerAdded = true
		}
	}

	if !markerAdded {
		out = append(out, marker)
	}

	return out
}

func omittedIndices(selected []bool) []int {
	omitted := make([]int, 0)

	for i, ok := range selected {
		if !ok {
			omitted = append(omitted, i)
		}
	}

	return omitted
}

func selectedMessagesUpperBound(messages []llm.Message, selected []bool, estimator Estimator) int {
	total := 0

	for i, msg := range messages {
		if selected[i] {
			total += estimator.EstimateMessage(msg).UpperBoundTokens
		}
	}

	return total
}

func rankCandidates(messages []llm.Message, selected []bool, analyses []messageAnalysis, policy Policy) []scoredMessage {
	candidates := make([]scoredMessage, 0, len(messages))
	for i, msg := range messages {
		if selected[i] || msg.Role == llm.RoleSystem {
			continue
		}

		candidates = append(candidates, scoredMessage{index: i, score: messageScore(i, msg, analyses[i], policy)})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].index > candidates[j].index
		}

		return candidates[i].score > candidates[j].score
	})

	return candidates
}

func messageScore(index int, msg llm.Message, analysis messageAnalysis, policy Policy) int {
	recency := index + 1

	importance := len(analysis.Signals)
	if analysis.Pinned {
		importance += 100
	}

	return recency*policy.RecencyWeight +
		(importance+analysis.Priority)*policy.ImportanceWeight +
		policy.rolePriority(msg.Role)
}

func analyzeMessages(messages []llm.Message, metadata []MessageMetadata) []messageAnalysis {
	analyses := make([]messageAnalysis, len(messages))
	for i, msg := range messages {
		var meta MessageMetadata
		if i < len(metadata) {
			meta = metadata[i]
		}

		analyses[i] = analyzeMessage(msg, meta)
	}

	return analyses
}

func analyzeMessage(msg llm.Message, meta MessageMetadata) messageAnalysis {
	var signals []string

	required := false

	lower := strings.ToLower(msg.Content)

	if msg.Role == llm.RoleSystem {
		signals = append(signals, "system-instruction")
		required = true
	}

	if containsAny(lower, "must", "never", "do not", "don't", "required", "requirement", "constraint", "acceptance criteria", "instruction", "policy") {
		signals = append(signals, "constraint-or-instruction")
		required = true
	}

	if containsAny(lower, "decision:", "decided", "we chose", "rejected:", "directive:") {
		signals = append(signals, "decision")
	}

	if containsAny(lower, "failed", "failure", "error", "panic", "stack trace", "regression", "not working") {
		signals = append(signals, "failure")
	}

	if containsAny(lower, "unresolved", "open question", "blocked", "follow-up", "todo") {
		signals = append(signals, "unresolved-question")
	}

	if hasFileCitation(msg.Content) {
		signals = append(signals, "file-citation")
	}

	if meta.Pinned {
		signals = append(signals, "metadata-pinned")
		required = true
	}

	if meta.Priority > 0 {
		signals = append(signals, fmt.Sprintf("metadata-priority:%d", meta.Priority))
		required = true
	}

	return messageAnalysis{Pinned: required, Priority: meta.Priority, Signals: signals}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}

	return false
}

func hasFileCitation(content string) bool {
	fields := strings.FieldsFunc(content, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == ',' || r == ';' || r == ')' || r == ']'
	})

	for _, field := range fields {
		field = strings.Trim(field, "`'\"")
		if !strings.Contains(field, ":") {
			continue
		}

		path, line, ok := strings.Cut(field, ":")
		if !ok || path == "" || line == "" {
			continue
		}

		if !hasKnownFileExtension(path) || !allDigits(line) {
			continue
		}

		return true
	}

	return false
}

func hasKnownFileExtension(path string) bool {
	for _, ext := range []string{".go", ".md", ".yaml", ".yml", ".json", ".toml", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java", ".c", ".h", ".sh"} {
		if strings.Contains(path, ext) {
			return true
		}
	}

	return false
}

func allDigits(text string) bool {
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}

	return text != ""
}

func (p Policy) withDefaults() Policy {
	if p.RecencyWeight == 0 {
		p.RecencyWeight = 1
	}

	if p.ImportanceWeight == 0 {
		p.ImportanceWeight = 10
	}

	if p.ManifestMaxItems <= 0 {
		p.ManifestMaxItems = 8
	}

	if p.ManifestMaxRanges <= 0 {
		p.ManifestMaxRanges = 16
	}

	if p.ManifestSummaryRunes <= 0 {
		p.ManifestSummaryRunes = 96
	}

	priorities := defaultRolePriority()
	maps.Copy(priorities, p.RolePriority)
	p.RolePriority = priorities

	return p
}

func defaultRolePriority() map[llm.Role]int {
	return map[llm.Role]int{
		llm.RoleSystem:    100,
		llm.RoleUser:      8,
		llm.RoleAssistant: 5,
		llm.RoleTool:      4,
	}
}

func (p Policy) rolePriority(role llm.Role) int {
	if p.RolePriority == nil {
		return defaultRolePriority()[role]
	}

	return p.RolePriority[role]
}

func (p Policy) summary() string {
	return fmt.Sprintf("recency=%d;importance=%d;role=user:%d,assistant:%d,tool:%d;preserve_pinned=%t", p.RecencyWeight, p.ImportanceWeight, p.rolePriority(llm.RoleUser), p.rolePriority(llm.RoleAssistant), p.rolePriority(llm.RoleTool), !p.DropPinnedWhenNeeded)
}

func cloneMessages(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, len(messages))
	copy(out, messages)

	return out
}
