package contextpack

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tommoulard/atteler/pkg/llm"
)

func TestCompact_NoOpWhenWithinBudget(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "follow instructions"},
		{Role: llm.RoleUser, Content: "hello"},
		{Role: llm.RoleAssistant, Content: "hi"},
	}

	result := Compact(messages, EstimateMessages(messages))

	if !reflect.DeepEqual(result.Messages, messages) {
		t.Fatalf("messages changed on no-op: got %+v want %+v", result.Messages, messages)
	}
	if result.Stats.Compressed {
		t.Fatal("expected no compression")
	}
	if result.Stats.OriginalCount != len(messages) || result.Stats.OutputCount != len(messages) {
		t.Fatalf("unexpected counts: %+v", result.Stats)
	}
	if result.Stats.OmittedCount != 0 {
		t.Fatalf("omitted count = %d, want 0", result.Stats.OmittedCount)
	}
}

func TestCompact_TrimsOlderMessagesAndPreservesOrder(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "system one"},
		{Role: llm.RoleUser, Content: strings.Repeat("old user ", 12)},
		{Role: llm.RoleAssistant, Content: strings.Repeat("old assistant ", 12)},
		{Role: llm.RoleSystem, Content: "system two"},
		{Role: llm.RoleUser, Content: "new user"},
		{Role: llm.RoleAssistant, Content: "new assistant"},
	}
	budget := EstimateMessage(messages[0]) + EstimateMessage(messages[3]) +
		EstimateMessage(messages[4]) + EstimateMessage(messages[5]) +
		EstimateMessage(OmissionMarker(2))

	result := Compact(messages, budget)

	want := []llm.Message{
		messages[0],
		OmissionMarker(2),
		messages[3],
		messages[4],
		messages[5],
	}
	if !reflect.DeepEqual(result.Messages, want) {
		t.Fatalf("unexpected compacted messages:\ngot  %+v\nwant %+v", result.Messages, want)
	}
	if !result.Stats.Compressed {
		t.Fatal("expected compression")
	}
	if result.Stats.OriginalCount != len(messages) || result.Stats.OutputCount != len(want) || result.Stats.OmittedCount != 2 {
		t.Fatalf("unexpected stats: %+v", result.Stats)
	}
}

func TestCompact_StaysWithinBudgetWhenPossible(t *testing.T) {
	t.Parallel()

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "policy"},
		{Role: llm.RoleUser, Content: strings.Repeat("one ", 20)},
		{Role: llm.RoleAssistant, Content: strings.Repeat("two ", 20)},
		{Role: llm.RoleUser, Content: "latest question"},
	}
	budget := EstimateMessage(messages[0]) + EstimateMessage(messages[3]) + EstimateMessage(OmissionMarker(2))

	result := Compact(messages, budget)

	if got := EstimateMessages(result.Messages); got > budget {
		t.Fatalf("estimated tokens = %d, budget = %d, messages = %+v", got, budget, result.Messages)
	}
	if result.Stats.OutputEstimatedTokens > budget {
		t.Fatalf("stats output tokens = %d, budget = %d", result.Stats.OutputEstimatedTokens, budget)
	}
	if result.Stats.OmittedCount != 2 {
		t.Fatalf("omitted count = %d, want 2", result.Stats.OmittedCount)
	}
	if got := result.Messages[len(result.Messages)-1]; got != messages[3] {
		t.Fatalf("newest message not preserved: got %+v want %+v", got, messages[3])
	}
}
