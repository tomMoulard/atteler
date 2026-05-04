// Package contextpack provides dependency-free helpers for fitting chat context
// into an estimated token budget.
package contextpack

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/llm"
)

const (
	messageOverheadTokens = 4
	charsPerToken         = 4
)

// Stats describes the result of a context compaction pass.
type Stats struct {
	OriginalCount           int
	OutputCount             int
	OmittedCount            int
	OriginalEstimatedTokens int
	OutputEstimatedTokens   int
	MaxEstimatedTokens      int
	Compressed              bool
}

// Result is the compacted message list plus accounting metadata.
type Result struct {
	Messages []llm.Message
	Stats    Stats
}

// Compact reduces messages to fit maxEstimatedTokens when possible while always
// preserving system messages, preserving the newest non-system messages that fit,
// and inserting an omission marker for older non-system messages that were
// trimmed. Token counts are estimates, not provider-specific tokenizer results.
//
// A non-positive maxEstimatedTokens value disables compaction and returns a copy
// of the original messages with populated stats.
func Compact(messages []llm.Message, maxEstimatedTokens int) Result {
	originalTokens := EstimateMessages(messages)
	if maxEstimatedTokens <= 0 || originalTokens <= maxEstimatedTokens {
		out := cloneMessages(messages)

		return Result{
			Messages: out,
			Stats: Stats{
				OriginalCount:           len(messages),
				OutputCount:             len(out),
				OriginalEstimatedTokens: originalTokens,
				OutputEstimatedTokens:   originalTokens,
				MaxEstimatedTokens:      maxEstimatedTokens,
			},
		}
	}

	nonSystemCount := countNonSystem(messages)
	if nonSystemCount == 0 {
		out := cloneMessages(messages)

		return Result{
			Messages: out,
			Stats: Stats{
				OriginalCount:           len(messages),
				OutputCount:             len(out),
				OriginalEstimatedTokens: originalTokens,
				OutputEstimatedTokens:   originalTokens,
				MaxEstimatedTokens:      maxEstimatedTokens,
			},
		}
	}

	selected := make([]bool, len(messages))
	used := 0

	for i, msg := range messages {
		if msg.Role == llm.RoleSystem {
			selected[i] = true
			used += EstimateMessage(msg)
		}
	}

	// Reserve marker space before choosing newest messages so the final output
	// includes an explicit indication that older context was omitted.
	marker := OmissionMarker(nonSystemCount)
	used += EstimateMessage(marker)

	selectedNonSystem := 0

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == llm.RoleSystem {
			continue
		}

		cost := EstimateMessage(msg)
		if used+cost > maxEstimatedTokens {
			break
		}

		selected[i] = true
		selectedNonSystem++
		used += cost
	}

	omitted := nonSystemCount - selectedNonSystem
	if omitted <= 0 {
		out := cloneMessages(messages)

		return Result{
			Messages: out,
			Stats: Stats{
				OriginalCount:           len(messages),
				OutputCount:             len(out),
				OriginalEstimatedTokens: originalTokens,
				OutputEstimatedTokens:   originalTokens,
				MaxEstimatedTokens:      maxEstimatedTokens,
			},
		}
	}

	marker = OmissionMarker(omitted)
	out := make([]llm.Message, 0, len(messages)-omitted+1)
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

	return Result{
		Messages: out,
		Stats: Stats{
			OriginalCount:           len(messages),
			OutputCount:             len(out),
			OmittedCount:            omitted,
			OriginalEstimatedTokens: originalTokens,
			OutputEstimatedTokens:   EstimateMessages(out),
			MaxEstimatedTokens:      maxEstimatedTokens,
			Compressed:              true,
		},
	}
}

// EstimateMessages returns a lightweight token estimate for a message slice.
func EstimateMessages(messages []llm.Message) int {
	total := 0
	for _, msg := range messages {
		total += EstimateMessage(msg)
	}

	return total
}

// EstimateMessage returns a lightweight token estimate for one message.
func EstimateMessage(msg llm.Message) int {
	contentTokens := estimateTextTokens(msg.Content)
	roleTokens := estimateTextTokens(string(msg.Role))

	return messageOverheadTokens + roleTokens + contentTokens
}

// OmissionMarker returns the synthetic message used to represent trimmed older
// non-system conversation turns.
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

func estimateTextTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}

	runes := utf8.RuneCountInString(text)

	return (runes + charsPerToken - 1) / charsPerToken
}

func cloneMessages(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, len(messages))
	copy(out, messages)

	return out
}

func countNonSystem(messages []llm.Message) int {
	count := 0

	for _, msg := range messages {
		if msg.Role != llm.RoleSystem {
			count++
		}
	}

	return count
}
