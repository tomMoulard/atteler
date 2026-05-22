package modelroute

import (
	"strings"
	"time"
)

const (
	// BuiltinCatalogVersion changes whenever maintained model metadata changes.
	BuiltinCatalogVersion = "2026-05-22.4"

	capabilityText        = "text"
	capabilityTools       = "tools"
	capabilityReasoning   = "reasoning"
	capabilityVision      = "vision"
	capabilityPromptCache = "prompt_cache"
	capabilityLocal       = "local"
)

var (
	builtinCatalogUpdatedAt = time.Date(2026, time.May, 22, 0, 0, 0, 0, time.UTC)
	builtinCatalogStaleAt   = time.Date(2026, time.August, 20, 0, 0, 0, 0, time.UTC)
)

// Catalog is a versioned metadata snapshot used to build route candidates from
// provider-backed model facts instead of ad-hoc CLI numbers.
type Catalog struct {
	Version    string          `json:"version"`
	UpdatedAt  time.Time       `json:"updated_at"`
	StaleAfter time.Time       `json:"stale_after"`
	Models     []ModelMetadata `json:"models"`
}

// ModelMetadata is the maintained, provider-backed metadata for one model.
// Costs are stored per token so they can be used directly by Candidate.
//
//nolint:govet // Field order keeps provider facts grouped in JSON artifacts.
type ModelMetadata struct {
	Provider              string   `json:"provider"`
	Name                  string   `json:"name"`
	Aliases               []string `json:"aliases,omitempty"`
	ContextWindow         int      `json:"context_window"`
	MaxOutputTokens       int      `json:"max_output_tokens,omitempty"`
	InputTokenCost        float64  `json:"input_token_cost"`
	CachedInputTokenCost  float64  `json:"cached_input_token_cost,omitempty"`
	CacheWriteTokenCost   float64  `json:"cache_write_token_cost,omitempty"`
	OutputTokenCost       float64  `json:"output_token_cost"`
	Capabilities          []string `json:"capabilities,omitempty"`
	Deprecated            bool     `json:"deprecated,omitempty"`
	Source                string   `json:"source"`
	SourceURL             string   `json:"source_url,omitempty"`
	SourcePublished       string   `json:"source_published,omitempty"`
	ProviderReportedAlias string   `json:"provider_reported_alias,omitempty"`
}

// BuiltinCatalog returns the maintained metadata snapshot for built-in
// providers. The prices are public API list prices captured in the catalog
// version; callers can layer live provider availability and telemetry on top.
func BuiltinCatalog() Catalog {
	return Catalog{
		Version:    BuiltinCatalogVersion,
		UpdatedAt:  builtinCatalogUpdatedAt,
		StaleAfter: builtinCatalogStaleAt,
		Models: []ModelMetadata{
			openAIMetadata("gpt-5.5", 1_050_000, 128_000, 5, 0.5, 30, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			openAIMetadata("gpt-5.4", 1_050_000, 128_000, 2.5, 0.25, 15, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			openAIMetadata("gpt-5.4-mini", 400_000, 128_000, 0.75, 0.075, 4.5, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			openAIMetadata("gpt-5.4-nano", 400_000, 128_000, 0.2, 0.02, 1.25, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			openAIMetadata("gpt-4.1", 1_047_576, 32_768, 2, 0.5, 8, capabilityText, capabilityTools, capabilityVision, capabilityPromptCache),
			openAIMetadata("gpt-4.1-mini", 1_047_576, 32_768, 0.4, 0.1, 1.6, capabilityText, capabilityTools, capabilityVision, capabilityPromptCache),
			openAIMetadata("gpt-4.1-nano", 1_047_576, 32_768, 0.1, 0.025, 0.4, capabilityText, capabilityTools, capabilityVision, capabilityPromptCache),
			openAIMetadata("o4-mini", 200_000, 100_000, 1.1, 0.275, 4.4, capabilityText, capabilityTools, capabilityVision, capabilityReasoning, capabilityPromptCache),
			codexMetadata("gpt-5.5", 1_050_000, 5, 0.5, 30, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			codexMetadata("gpt-5.4", 1_050_000, 2.5, 0.25, 15, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			codexMetadata("gpt-5.4-mini", 400_000, 0.75, 0.075, 4.5, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			codexMetadata("gpt-5.3-codex", 200_000, 1.75, 0.175, 14, capabilityText, capabilityTools, capabilityReasoning, capabilityPromptCache),
			anthropicMetadata("anthropic", "claude-opus-4-7", 1_000_000, 128_000, 5, 0.5, 25, false, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("anthropic", "claude-opus-4-6", 1_000_000, 128_000, 5, 0.5, 25, false, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("anthropic", "claude-opus-4-5-20251101", 200_000, 64_000, 5, 0.5, 25, false, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("anthropic", "claude-opus-4-20250514", 200_000, 32_000, 15, 1.5, 75, true, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("anthropic", "claude-sonnet-4-6", 1_000_000, 64_000, 3, 0.3, 15, false, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("anthropic", "claude-sonnet-4-5-20250929", 200_000, 64_000, 3, 0.3, 15, false, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("anthropic", "claude-sonnet-4-20250514", 200_000, 64_000, 3, 0.3, 15, true, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("anthropic", "claude-haiku-4-5-20251001", 200_000, 64_000, 1, 0.1, 5, false, capabilityText, capabilityTools, capabilityVision, capabilityPromptCache),
			anthropicMetadata("claude-code", "claude-opus-4-7", 1_000_000, 128_000, 5, 0.5, 25, false, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("claude-code", "claude-opus-4-6", 1_000_000, 128_000, 5, 0.5, 25, false, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("claude-code", "claude-opus-4-5-20251101", 200_000, 64_000, 5, 0.5, 25, false, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("claude-code", "claude-opus-4-1-20250805", 200_000, 64_000, 15, 1.5, 75, false, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("claude-code", "claude-opus-4-20250514", 200_000, 32_000, 15, 1.5, 75, true, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("claude-code", "claude-sonnet-4-6", 1_000_000, 64_000, 3, 0.3, 15, false, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("claude-code", "claude-sonnet-4-5-20250929", 200_000, 64_000, 3, 0.3, 15, false, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("claude-code", "claude-sonnet-4-20250514", 200_000, 64_000, 3, 0.3, 15, true, capabilityText, capabilityTools, capabilityReasoning, capabilityVision, capabilityPromptCache),
			anthropicMetadata("claude-code", "claude-haiku-4-5-20251001", 200_000, 64_000, 1, 0.1, 5, false, capabilityText, capabilityTools, capabilityVision, capabilityPromptCache),
			ollamaMetadata("llama3.2"),
			ollamaMetadata("llama3.1"),
			ollamaMetadata("qwen2.5"),
			ollamaMetadata("mistral"),
			ollamaMetadata("gemma3"),
			ollamaMetadata("deepseek-r1"),
		},
	}
}

// ID returns a stable provider/model identifier for metadata.
func (m ModelMetadata) ID() string {
	return (Candidate{Provider: m.Provider, Name: m.Name}).ID()
}

// Candidate converts maintained metadata into a routable candidate.
func (m ModelMetadata) Candidate(priority int) Candidate {
	return Candidate{
		Name:                 m.Name,
		Provider:             m.Provider,
		InputTokenCost:       m.InputTokenCost,
		CachedInputTokenCost: m.CachedInputTokenCost,
		CacheWriteTokenCost:  m.CacheWriteTokenCost,
		OutputTokenCost:      m.OutputTokenCost,
		Priority:             priority,
		MaxInputTokens:       m.ContextWindow,
		MaxOutputTokens:      m.MaxOutputTokens,
		Capabilities:         append([]string(nil), m.Capabilities...),
		MetadataVersion:      BuiltinCatalogVersion,
		MetadataSource:       m.Source,
		MetadataSourceURL:    m.SourceURL,
		MetadataPublished:    m.SourcePublished,
		Aliases:              m.allAliases(),
		Deprecated:           m.Deprecated,
	}
}

// IsStale reports whether catalog is past its explicit freshness horizon.
func (c Catalog) IsStale(now time.Time) bool {
	return !c.StaleAfter.IsZero() && now.After(c.StaleAfter)
}

// Lookup returns metadata for provider/model. If provider is empty, the first
// model with a matching provider-local name is returned.
func (c Catalog) Lookup(provider, name string) (ModelMetadata, bool) {
	provider = normalize(provider)
	name = normalizeModelName(name)

	for i := range c.Models {
		model := c.Models[i]
		if !model.matchesName(name) {
			continue
		}

		if provider == "" || normalize(model.Provider) == provider {
			return model, true
		}
	}

	return ModelMetadata{}, false
}

func (m ModelMetadata) matchesName(name string) bool {
	if normalizeModelName(m.Name) == name {
		return true
	}

	if normalizeModelName(m.ProviderReportedAlias) == name {
		return true
	}

	for _, alias := range m.Aliases {
		if normalizeModelName(alias) == name {
			return true
		}
	}

	return false
}

func (m ModelMetadata) allAliases() []string {
	aliases := append([]string(nil), m.Aliases...)
	if m.ProviderReportedAlias != "" {
		aliases = append(aliases, m.ProviderReportedAlias)
	}

	return aliases
}

// Candidate returns a catalog-backed candidate for id. The id may be either
// provider-qualified ("openai/gpt-4.1-mini") or provider-local when unambiguous.
func (c Catalog) Candidate(id string) (Candidate, bool) {
	candidate, _, ok := c.resolveCandidate(id)

	return candidate, ok
}

func (c Catalog) resolveCandidate(id string) (Candidate, string, bool) {
	provider, model := splitID(id)

	metadata, reason, ok := c.lookupCandidateMetadata(provider, model)
	if !ok {
		return Candidate{}, reason, false
	}

	candidate := metadata.Candidate(0)
	candidate.MetadataVersion = c.Version

	return candidate, "", true
}

func (c Catalog) lookupCandidateMetadata(provider, model string) (ModelMetadata, string, bool) {
	provider = normalize(provider)

	model = normalizeModelName(model)
	if model == "" {
		return ModelMetadata{}, ReasonUnknownMetadata, false
	}

	if provider != "" {
		metadata, ok := c.Lookup(provider, model)
		if !ok {
			return ModelMetadata{}, ReasonUnknownMetadata, false
		}

		return metadata, "", true
	}

	var match ModelMetadata

	matches := 0

	for i := range c.Models {
		metadata := c.Models[i]
		if !metadata.matchesName(model) {
			continue
		}

		matches++
		if matches == 1 {
			match = metadata
		}
	}

	switch matches {
	case 0:
		return ModelMetadata{}, ReasonUnknownMetadata, false
	case 1:
		return match, "", true
	default:
		return ModelMetadata{}, ReasonAmbiguousMetadata, false
	}
}

func (c Catalog) candidateFailureReason(id string) string {
	_, reason, ok := c.resolveCandidate(id)
	if ok {
		return ""
	}

	return reason
}

// Candidates returns all catalog-backed candidates for ids and the ids that had
// no maintained metadata.
func (c Catalog) Candidates(ids []string) (candidates []Candidate, unknown []string) {
	candidates = make([]Candidate, 0, len(ids))
	seenCandidates := make(map[string]bool, len(ids))
	seenUnknown := make(map[string]bool, len(ids))

	for _, id := range ids {
		candidate, ok := c.Candidate(id)
		if !ok {
			unknownID := normalize(id)
			if unknownID == "" || seenUnknown[unknownID] {
				continue
			}

			seenUnknown[unknownID] = true

			unknown = append(unknown, id)

			continue
		}

		candidateID := normalize(candidate.ID())
		if candidateID == "" || seenCandidates[candidateID] {
			continue
		}

		seenCandidates[candidateID] = true

		candidates = append(candidates, candidate)
	}

	return candidates, unknown
}

func openAIMetadata(name string, contextWindow, maxOutputTokens int, inputPerMillion, cachedPerMillion, outputPerMillion float64, capabilities ...string) ModelMetadata {
	metadata := pricedMetadata("openai", name, contextWindow, maxOutputTokens, inputPerMillion, cachedPerMillion, 0, outputPerMillion, false, "OpenAI API model comparison and pricing", "https://developers.openai.com/api/docs/models/compare", "2026-05-22", capabilities...)
	switch name {
	case "gpt-4.1", "gpt-4.1-mini", "gpt-4.1-nano":
		metadata.ProviderReportedAlias = name + "-2025-04-14"
	}

	return metadata
}

func codexMetadata(name string, contextWindow int, inputPerMillion, cachedPerMillion, outputPerMillion float64, capabilities ...string) ModelMetadata {
	return pricedMetadata("codex", name, contextWindow, 128_000, inputPerMillion, cachedPerMillion, 0, outputPerMillion, false, "OpenAI API pricing (Codex reference)", "https://developers.openai.com/api/docs/pricing", "2026-05-22", capabilities...)
}

func anthropicMetadata(provider, name string, contextWindow, maxOutputTokens int, inputPerMillion, cacheReadPerMillion, outputPerMillion float64, deprecated bool, capabilities ...string) ModelMetadata {
	return pricedMetadata(provider, name, contextWindow, maxOutputTokens, inputPerMillion, cacheReadPerMillion, inputPerMillion*1.25, outputPerMillion, deprecated, "Anthropic Claude pricing", "https://platform.claude.com/docs/en/about-claude/pricing", "2026-05-22", capabilities...)
}

func ollamaMetadata(name string) ModelMetadata {
	return ModelMetadata{
		Provider:      "ollama",
		Name:          name,
		ContextWindow: 128_000,
		Capabilities:  []string{capabilityText, capabilityTools, capabilityLocal},
		Source:        "local Ollama catalog",
	}
}

func pricedMetadata(provider, name string, contextWindow, maxOutputTokens int, inputPerMillion, cachedPerMillion, cacheWritePerMillion, outputPerMillion float64, deprecated bool, source, sourceURL, sourcePublished string, capabilities ...string) ModelMetadata {
	return ModelMetadata{
		Provider:             provider,
		Name:                 name,
		ContextWindow:        contextWindow,
		MaxOutputTokens:      maxOutputTokens,
		InputTokenCost:       perToken(inputPerMillion),
		CachedInputTokenCost: perToken(cachedPerMillion),
		CacheWriteTokenCost:  perToken(cacheWritePerMillion),
		OutputTokenCost:      perToken(outputPerMillion),
		Capabilities:         append([]string(nil), capabilities...),
		Deprecated:           deprecated,
		Source:               source,
		SourceURL:            sourceURL,
		SourcePublished:      sourcePublished,
	}
}

func perToken(perMillion float64) float64 {
	if perMillion <= 0 {
		return 0
	}

	return perMillion / 1_000_000
}

func splitID(id string) (provider, model string) {
	provider, model, ok := strings.Cut(strings.TrimSpace(id), "/")
	if !ok {
		return "", strings.TrimSpace(id)
	}

	return strings.TrimSpace(provider), strings.TrimSpace(model)
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeModelName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
