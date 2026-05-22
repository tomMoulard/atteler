package contextpack

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/llm"
)

const (
	defaultMessageOverheadTokens = 6
	defaultCharsPerToken         = 3
	defaultErrorBoundPercent     = 25
	providerAnthropicName        = "anthropic"
	providerClaudeCodeName       = "claude-code"
	providerCodexName            = "codex"
	providerGenericName          = "generic"
	providerOllamaName           = "ollama"
	providerOpenAIName           = "openai"
)

// TokenEstimate is a calibrated token estimate with an explicit conservative
// upper bound. Compaction uses UpperBoundTokens for budget decisions so a rough
// point estimate is never treated as proof that a provider will accept a prompt.
type TokenEstimate struct {
	Tokens           int `json:"tokens"`
	ErrorBoundTokens int `json:"error_bound_tokens"`
	UpperBoundTokens int `json:"upper_bound_tokens"`
}

// EstimatorProfile documents the provider/model calibration used for token
// budgeting. The package intentionally keeps this dependency-free; callers can
// supply an Estimator backed by a provider tokenizer when one is available.
type EstimatorProfile struct {
	Name                  string
	Provider              string
	Model                 string
	CharsPerToken         int
	MessageOverheadTokens int
	ErrorBoundPercent     int
}

// Estimator estimates token usage for messages under a provider/model profile.
type Estimator interface {
	EstimateMessage(llm.Message) TokenEstimate
	EstimateMessages([]llm.Message) TokenEstimate
	Profile() EstimatorProfile
}

type calibratedEstimator struct {
	profile EstimatorProfile
}

// NewEstimator returns a provider/model-aware calibrated estimator. It uses
// conservative chars-per-token ratios and provider-specific message overheads;
// callers that have access to an exact tokenizer should pass it through
// Options.Estimator.
func NewEstimator(provider, model string) Estimator {
	provider, model = normalizeEstimatorTarget(provider, model)

	profile := EstimatorProfile{
		Name:                  "generic-conservative",
		Provider:              provider,
		Model:                 model,
		CharsPerToken:         defaultCharsPerToken,
		MessageOverheadTokens: defaultMessageOverheadTokens,
		ErrorBoundPercent:     defaultErrorBoundPercent,
	}

	switch provider {
	case providerOpenAIName:
		profile.Name = "openai-calibrated"
		profile.MessageOverheadTokens = 4
		profile.ErrorBoundPercent = 12
	case providerCodexName:
		profile.Name = "codex-calibrated"
		profile.MessageOverheadTokens = 5
		profile.ErrorBoundPercent = 12
	case providerAnthropicName, providerClaudeCodeName:
		profile.Name = "anthropic-calibrated"
		profile.MessageOverheadTokens = 7
		profile.ErrorBoundPercent = 18
	case providerOllamaName:
		profile.Name = "ollama-calibrated"
		profile.MessageOverheadTokens = 8
		profile.ErrorBoundPercent = 20
	}

	return calibratedEstimator{profile: profile}
}

// DefaultEstimator returns the package's conservative provider-agnostic
// estimator.
func DefaultEstimator() Estimator {
	return NewEstimator("", "")
}

func (e calibratedEstimator) Profile() EstimatorProfile {
	return e.profile
}

func (e calibratedEstimator) EstimateMessages(messages []llm.Message) TokenEstimate {
	total := TokenEstimate{}
	for _, msg := range messages {
		total = addTokenEstimate(total, e.EstimateMessage(msg))
	}

	return total
}

func (e calibratedEstimator) EstimateMessage(msg llm.Message) TokenEstimate {
	profile := e.profile
	base := profile.MessageOverheadTokens + estimateTextTokensWithProfile(string(msg.Role), profile) + estimateTextTokensWithProfile(msg.Content, profile)

	if len(msg.ToolCalls) > 0 {
		base += estimateJSONTokens(msg.ToolCalls, profile)
	}

	if msg.ToolResult != nil {
		base += estimateJSONTokens(msg.ToolResult, profile)
	}

	if base == 0 {
		return TokenEstimate{}
	}

	errorBound := ceilDiv(base*profile.ErrorBoundPercent, 100)

	return TokenEstimate{
		Tokens:           base,
		ErrorBoundTokens: errorBound,
		UpperBoundTokens: base + errorBound,
	}
}

// EstimateMessages returns a lightweight point token estimate for a message
// slice using the default conservative estimator. Use CompactWithOptions or
// NewEstimator when provider/model-specific budget checks are required.
func EstimateMessages(messages []llm.Message) int {
	return DefaultEstimator().EstimateMessages(messages).Tokens
}

// EstimateMessage returns a lightweight point token estimate for one message
// using the default conservative estimator.
func EstimateMessage(msg llm.Message) int {
	return DefaultEstimator().EstimateMessage(msg).Tokens
}

func addTokenEstimate(left, right TokenEstimate) TokenEstimate {
	return TokenEstimate{
		Tokens:           left.Tokens + right.Tokens,
		ErrorBoundTokens: left.ErrorBoundTokens + right.ErrorBoundTokens,
		UpperBoundTokens: left.UpperBoundTokens + right.UpperBoundTokens,
	}
}

func estimateJSONTokens(value any, profile EstimatorProfile) int {
	data, err := json.Marshal(value)
	if err != nil {
		return 0
	}

	return estimateTextTokensWithProfile(string(data), profile)
}

func estimateTextTokensWithProfile(text string, profile EstimatorProfile) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}

	charsPerToken := profile.CharsPerToken
	if charsPerToken <= 0 {
		charsPerToken = defaultCharsPerToken
	}

	runes := utf8.RuneCountInString(text)

	return ceilDiv(runes, charsPerToken)
}

func ceilDiv(value, divisor int) int {
	if value <= 0 {
		return 0
	}

	if divisor <= 0 {
		return value
	}

	return (value + divisor - 1) / divisor
}

func normalizeEstimatorTarget(provider, model string) (normalizedProvider, normalizedModel string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)

	if prefix, rest, ok := strings.Cut(model, "/"); ok && prefix != "" && rest != "" {
		provider = strings.ToLower(strings.TrimSpace(prefix))
		model = strings.TrimSpace(rest)
	}

	lowerModel := strings.ToLower(model)

	if provider == "" {
		switch {
		case strings.HasPrefix(lowerModel, "claude"):
			provider = providerAnthropicName
		case strings.HasPrefix(lowerModel, "gpt-"), strings.HasPrefix(lowerModel, "o1"), strings.HasPrefix(lowerModel, "o3"), strings.HasPrefix(lowerModel, "o4"):
			provider = providerOpenAIName
		case strings.HasPrefix(lowerModel, "llama"), strings.HasPrefix(lowerModel, "mistral"), strings.HasPrefix(lowerModel, "gemma"), strings.HasPrefix(lowerModel, "qwen"), strings.HasPrefix(lowerModel, "deepseek"):
			provider = providerOllamaName
		default:
			provider = providerGenericName
		}
	}

	return provider, model
}
