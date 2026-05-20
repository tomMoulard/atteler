package events

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupportedEventTypes_ReturnsStableSortedEvents(t *testing.T) {
	t.Parallel()

	got := SupportedEventTypes()

	wantTypes := []string{
		AgentExecute,
		AssistantMessage,
		CommandExecute,
		CommandOutput,
		ContextAdd,
		Error,
		FileRead,
		FileWrite,
		SessionEnd,
		SessionStart,
		ToolExecute,
		UserMessage,
	}
	require.Len(t, got, len(wantTypes))

	gotTypes := make([]string, 0, len(got))
	for _, eventType := range got {
		gotTypes = append(gotTypes, eventType.Type)
		assert.NotEmpty(t, eventType.Description, "description for %s", eventType.Type)
		assert.LessOrEqual(t, len(eventType.Description), 80, "description should stay short for %s", eventType.Type)
	}

	assert.Equal(t, wantTypes, gotTypes)
	assert.True(t, sort.StringsAreSorted(gotTypes), "event types should be sorted")
}

func TestSupportedEventTypes_ReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()

	got := SupportedEventTypes()
	require.NotEmpty(t, got)
	got[0] = SupportedEventType{Type: "changed", Description: "changed"}

	again := SupportedEventTypes()
	assert.Equal(t, AgentExecute, again[0].Type)
	assert.NotEqual(t, "changed", again[0].Description)
}
