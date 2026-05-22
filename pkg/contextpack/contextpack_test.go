package contextpack

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
)

func TestCompact_NoOpWhenWithinProviderUpperBound(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "follow instructions"},
		{Role: llm.RoleUser, Content: "hello"},
		{Role: llm.RoleAssistant, Content: "hi"},
	}
	estimator := NewEstimator("openai", "gpt-4.1")

	result := CompactWithOptions(messages, Options{
		Provider:  "openai",
		Model:     "gpt-4.1",
		MaxTokens: estimator.EstimateMessages(messages).UpperBoundTokens,
	})

	require.True(t, reflect.DeepEqual(result.Messages, messages), "messages changed on no-op: got %+v want %+v", result.Messages, messages)
	assert.False(t, result.Stats.Compressed)
	assert.True(t, result.Stats.FitsBudget)
	assert.Equal(t, len(messages), result.Stats.OriginalCount)
	assert.Equal(t, len(messages), result.Stats.OutputCount)
	assert.Equal(t, 0, result.Stats.OmittedCount)
	assert.Contains(t, result.Stats.Estimator, "openai-calibrated")
}

func TestCompact_TrimsOlderMessagesWithEvidenceManifest(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "system one"},
		{Role: llm.RoleUser, Content: strings.Repeat("old user ", 80)},
		{Role: llm.RoleAssistant, Content: strings.Repeat("old assistant ", 80)},
		{Role: llm.RoleSystem, Content: "system two"},
		{Role: llm.RoleUser, Content: "new user"},
		{Role: llm.RoleAssistant, Content: "new assistant"},
	}
	estimator := DefaultEstimator()
	budget := estimator.EstimateMessages([]llm.Message{messages[0], messages[3], messages[4], messages[5]}).UpperBoundTokens + 450

	result := CompactWithOptions(messages, Options{MaxTokens: budget})

	require.True(t, result.Stats.Compressed, "expected compression: %+v", result.Stats)
	assert.True(t, result.Stats.FitsBudget)
	assert.Positive(t, result.Stats.OmittedCount)
	assert.LessOrEqual(t, result.Stats.OutputEstimatedUpperBound, budget)

	marker := requireManifestMarker(t, result.Messages)
	assert.Contains(t, marker.Content, "evidence manifest")
	assert.Equal(t, result.Stats.OmittedCount, result.Manifest.OmittedCount)
	assert.NotEmpty(t, result.Manifest.Ranges)
	assert.NotEmpty(t, result.Manifest.Items)
	assert.Equal(t, unknownTimestamp, result.Manifest.Items[0].Timestamp)
	assert.Contains(t, result.Manifest.Items[0].Hash, "sha256:")
	assert.NotEmpty(t, result.Manifest.Items[0].Summary)
	assert.NotEmpty(t, result.Manifest.Items[0].WhyDropped)
}

func TestCompact_PreservesOldCriticalInstructionsUnderTightBudget(t *testing.T) {
	t.Parallel()

	critical := "MUST keep the migration lock; see pkg/contextpack/contextpack.go:42"
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "Answer safely."},
		{Role: llm.RoleUser, Content: critical},
		{Role: llm.RoleAssistant, Content: strings.Repeat("bulk analysis ", 80)},
		{Role: llm.RoleUser, Content: strings.Repeat("recent filler ", 80)},
		{Role: llm.RoleAssistant, Content: "latest acknowledgement"},
	}
	estimator := DefaultEstimator()
	budget := estimator.EstimateMessages(messages).UpperBoundTokens - 1

	result := CompactWithOptions(messages, Options{MaxTokens: budget})

	require.False(t, result.Stats.HardBudgetFailure, "unexpected hard failure: %+v", result.Stats)
	assert.True(t, result.Stats.Compressed)
	assert.True(t, result.Stats.FitsBudget)
	assert.LessOrEqual(t, result.Stats.OutputEstimatedUpperBound, budget)
	assertMessageContentPresent(t, result.Messages, critical)
	assert.NotEmpty(t, result.Manifest.Ranges)
	assert.Contains(t, result.Manifest.Policy, "preserve_pinned=true")
}

func TestCompact_SystemMessageOverflowIsHardBudgetFailure(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: strings.Repeat("system policy ", 80)},
		{Role: llm.RoleUser, Content: "can be dropped"},
	}
	estimator := DefaultEstimator()
	budget := estimator.EstimateMessage(messages[0]).UpperBoundTokens - 1

	result := CompactWithOptions(messages, Options{MaxTokens: budget})

	assert.True(t, result.Stats.HardBudgetFailure)
	assert.False(t, result.Stats.FitsBudget)
	assert.False(t, result.Stats.Compressed)
	assert.Contains(t, result.Stats.BudgetFailureReason, "system messages")
	assert.Equal(t, messages, result.Messages)
}

func TestCompact_ProviderVarianceUsesUpperBoundBeforeClaimingFit(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("token variance ", 8)}}
	openAI := NewEstimator("openai", "gpt-4.1")
	anthropic := NewEstimator("anthropic", "claude-sonnet-4-20250514")
	openAIUpper := openAI.EstimateMessages(messages).UpperBoundTokens
	anthropicUpper := anthropic.EstimateMessages(messages).UpperBoundTokens
	require.Greater(t, anthropicUpper, openAIUpper)

	openAIResult := CompactWithOptions(messages, Options{Provider: "openai", Model: "gpt-4.1", MaxTokens: openAIUpper})
	anthropicResult := CompactWithOptions(messages, Options{Provider: "anthropic", Model: "claude-sonnet-4-20250514", MaxTokens: openAIUpper})

	assert.True(t, openAIResult.Stats.FitsBudget)
	assert.False(t, openAIResult.Stats.Compressed)
	assert.Contains(t, openAIResult.Stats.Estimator, "openai-calibrated")

	assert.Contains(t, anthropicResult.Stats.Estimator, "anthropic-calibrated")
	assert.False(t, !anthropicResult.Stats.Compressed && !anthropicResult.Stats.HardBudgetFailure && anthropicResult.Stats.FitsBudget, "anthropic budget check claimed the unmodified context fit: %+v", anthropicResult.Stats)
}

func TestCompact_HardFailsWhenPinnedEvidenceAndManifestCannotFit(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "policy"},
		{Role: llm.RoleUser, Content: strings.Repeat("MUST preserve this critical constraint ", 20)},
		{Role: llm.RoleAssistant, Content: "filler"},
	}
	estimator := DefaultEstimator()
	budget := estimator.EstimateMessages([]llm.Message{messages[0], messages[1]}).UpperBoundTokens - 1

	result := CompactWithOptions(messages, Options{MaxTokens: budget})

	assert.True(t, result.Stats.HardBudgetFailure)
	assert.False(t, result.Stats.FitsBudget)
	assert.Contains(t, result.Stats.BudgetFailureReason, "pinned evidence")
	assert.Equal(t, messages, result.Messages)
}

func TestCompact_ModelContextWindowCapsUnsetBudget(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{{Role: llm.RoleSystem, Content: "policy"}}
	for range 30 {
		messages = append(messages, llm.Message{Role: llm.RoleUser, Content: strings.Repeat("filler ", 220)})
	}

	result := CompactWithOptions(messages, Options{Model: "openai/gpt-4"})

	assert.True(t, result.Stats.Compressed)
	assert.True(t, result.Stats.FitsBudget)
	assert.Equal(t, 8192, result.Stats.MaxEstimatedTokens)
	assert.LessOrEqual(t, result.Stats.OutputEstimatedUpperBound, 8192)
	assert.Contains(t, result.Stats.Estimator, "openai-calibrated")
}

func TestCompact_ManifestUsesMessageMetadataTimestamps(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "policy"},
		{Role: llm.RoleUser, Content: "old evidence"},
		{Role: llm.RoleAssistant, Content: "new answer"},
		{Role: llm.RoleTool, Content: "newer tool output"},
	}
	metadata := []MessageMetadata{
		{},
		{Timestamp: "2026-05-22T10:00:00Z"},
		{},
		{},
	}

	result := CompactWithOptions(messages, Options{
		Estimator: fixedTokenEstimator{},
		Metadata:  metadata,
		MaxTokens: 3,
		Policy: Policy{
			RolePriority: map[llm.Role]int{
				llm.RoleUser:      0,
				llm.RoleAssistant: 0,
				llm.RoleTool:      100,
			},
		},
	})

	require.True(t, result.Stats.Compressed, "expected compression: %+v", result.Stats)
	require.NotEmpty(t, result.Manifest.Items)
	assert.Equal(t, "2026-05-22T10:00:00Z", result.Manifest.Items[0].Timestamp)
}

func TestCompact_EvidenceManifestKeepsAtLeastOneItem(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "policy"},
		{Role: llm.RoleUser, Content: "older filler"},
		{Role: llm.RoleAssistant, Content: "newer answer"},
	}

	result := CompactWithOptions(messages, Options{
		Estimator: fixedTokenEstimator{},
		MaxTokens: 2,
		Policy: Policy{
			ManifestMaxItems:     1,
			ManifestMaxRanges:    1,
			ManifestSummaryRunes: 0,
		},
	})

	require.True(t, result.Stats.Compressed, "expected compression: %+v", result.Stats)
	require.Len(t, result.Manifest.Items, 1)
	assert.NotEmpty(t, result.Manifest.Items[0].Hash)
	assert.NotEmpty(t, result.Manifest.Items[0].WhyDropped)
}

func TestCompact_PolicyRolePriorityCanBeatRecency(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "policy"},
		{Role: llm.RoleUser, Content: "older user detail"},
		{Role: llm.RoleAssistant, Content: "newer assistant detail"},
		{Role: llm.RoleTool, Content: "newer tool detail"},
	}

	result := CompactWithOptions(messages, Options{
		Estimator: fixedTokenEstimator{},
		MaxTokens: 3,
		Policy: Policy{
			RecencyWeight:        1,
			ImportanceWeight:     1,
			ManifestMaxItems:     1,
			ManifestMaxRanges:    1,
			ManifestSummaryRunes: 16,
			RolePriority: map[llm.Role]int{
				llm.RoleUser:      100,
				llm.RoleAssistant: 0,
				llm.RoleTool:      0,
			},
		},
	})

	require.True(t, result.Stats.Compressed, "expected compression: %+v", result.Stats)
	assert.True(t, result.Stats.FitsBudget)
	assert.Equal(t, 2, result.Stats.OmittedCount)
	assertMessageContentPresent(t, result.Messages, "older user detail")
	assertExactMessageContentAbsent(t, result.Messages, "newer assistant detail")
	assertExactMessageContentAbsent(t, result.Messages, "newer tool detail")
	assert.Contains(t, result.Manifest.Policy, "role=user:100")
}

type fixedTokenEstimator struct{}

func (fixedTokenEstimator) EstimateMessage(llm.Message) TokenEstimate {
	return TokenEstimate{Tokens: 1, ErrorBoundTokens: 0, UpperBoundTokens: 1}
}

func (e fixedTokenEstimator) EstimateMessages(messages []llm.Message) TokenEstimate {
	var total TokenEstimate
	for _, message := range messages {
		total = addTokenEstimate(total, e.EstimateMessage(message))
	}

	return total
}

func (fixedTokenEstimator) Profile() EstimatorProfile {
	return EstimatorProfile{Name: "fixed-test", Provider: "test", CharsPerToken: 1, MessageOverheadTokens: 1}
}

func requireManifestMarker(t *testing.T, messages []llm.Message) llm.Message {
	t.Helper()

	for _, message := range messages {
		if message.Role == llm.RoleSystem && strings.Contains(message.Content, "evidence manifest") {
			return message
		}
	}

	require.Fail(t, "manifest marker not found", "messages = %+v", messages)

	return llm.Message{}
}

func assertMessageContentPresent(t *testing.T, messages []llm.Message, content string) {
	t.Helper()

	for _, message := range messages {
		if message.Content == content {
			return
		}
	}

	assert.Failf(t, "message content not retained", "missing %q in %+v", content, messages)
}

func assertExactMessageContentAbsent(t *testing.T, messages []llm.Message, content string) {
	t.Helper()

	for _, message := range messages {
		if message.Content == content {
			assert.Failf(t, "message content unexpectedly retained", "unexpected %q in %+v", content, messages)

			return
		}
	}
}
