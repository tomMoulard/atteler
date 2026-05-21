package skill

import (
	"errors"
	"fmt"
	"strings"
)

// TriggerEvalResult is one evaluated trigger fixture.
type TriggerEvalResult struct {
	Prompt   string
	Reason   string
	Expected bool
	Actual   bool
}

// BuildTriggerEvals returns small fixtures that document when a generated skill
// should and should not activate. They are persisted with accepted skills and
// used by tests as a guard against over-broad trigger descriptions.
func BuildTriggerEvals(suggestion Suggestion) []TriggerEval {
	if len(suggestion.Steps) == 0 {
		return nil
	}

	workflow := humanWorkflow(suggestion.Steps)

	name := strings.TrimSuffix(suggestion.Name, " Skill")
	if strings.TrimSpace(name) == "" {
		name = suggestion.Slug
	}

	evals := []TriggerEval{
		{
			Prompt:        fmt.Sprintf("Run the %s workflow: %s.", suggestion.Slug, workflow),
			ShouldTrigger: true,
			Reason:        "mentions the full synthesized workflow",
		},
		{
			Prompt:        fmt.Sprintf("Use the %s skill for this task.", name),
			ShouldTrigger: true,
			Reason:        "explicitly asks for the generated skill",
		},
		{
			Prompt:        fmt.Sprintf("Only %s.", humanStep(suggestion.Steps[0])),
			ShouldTrigger: false,
			Reason:        "single-step requests should not trigger a multi-step skill",
		},
		{
			Prompt:        "Summarize the project README.",
			ShouldTrigger: false,
			Reason:        "unrelated prompt does not mention the workflow",
		},
	}

	return evals
}

// ValidateTriggerEvals checks that the generated or supplied trigger fixtures
// agree with the conservative trigger matcher before a skill is saved.
func ValidateTriggerEvals(suggestion Suggestion) ([]TriggerEvalResult, error) {
	evals := suggestion.TriggerEvals
	if len(evals) == 0 {
		evals = BuildTriggerEvals(suggestion)
	}

	if len(evals) == 0 {
		return nil, errors.New("skill: trigger evals are required")
	}

	results := make([]TriggerEvalResult, 0, len(evals))
	seenTrigger := false
	seenReject := false

	for _, eval := range evals {
		prompt := strings.TrimSpace(eval.Prompt)
		if prompt == "" {
			return results, errors.New("skill: trigger eval prompt is required")
		}

		actual := PromptTriggers(suggestion, prompt)
		result := TriggerEvalResult{
			Prompt:   prompt,
			Reason:   eval.Reason,
			Expected: eval.ShouldTrigger,
			Actual:   actual,
		}
		results = append(results, result)

		if eval.ShouldTrigger {
			seenTrigger = true
		} else {
			seenReject = true
		}

		if actual != eval.ShouldTrigger {
			return results, fmt.Errorf(
				"skill: trigger eval %q expected should_trigger=%t, got %t",
				prompt,
				eval.ShouldTrigger,
				actual,
			)
		}
	}

	if !seenTrigger {
		return results, errors.New("skill: at least one positive trigger eval is required")
	}

	if !seenReject {
		return results, errors.New("skill: at least one negative trigger eval is required")
	}

	return results, nil
}

// PromptTriggers reports whether prompt matches the whole synthesized workflow.
// It is intentionally conservative: either the prompt names the skill/slug, or
// it mentions all non-placeholder anchors from every workflow step.
func PromptTriggers(suggestion Suggestion, prompt string) bool {
	prompt = normalizeStep(prompt)
	if prompt == "" || len(suggestion.Steps) == 0 {
		return false
	}

	promptTokens := tokenSet(prompt)
	if containsAllTokens(promptTokens, slugTokens(suggestion.Slug)) {
		return true
	}

	name := normalizeStep(strings.TrimSuffix(suggestion.Name, " Skill"))
	if name != "" && containsAllTokens(promptTokens, strings.Fields(name)) {
		return true
	}

	for _, step := range suggestion.Steps {
		anchors := stepAnchorTokens(step)
		if len(anchors) == 0 {
			continue
		}

		if !containsAllTokens(promptTokens, anchors) {
			return false
		}
	}

	return true
}

func humanWorkflow(steps []string) string {
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		parts = append(parts, humanStep(step))
	}

	return strings.Join(parts, " → ")
}

func humanStep(step string) string {
	step = strings.ReplaceAll(step, "{{", "<")
	step = strings.ReplaceAll(step, "}}", ">")

	return step
}

func slugTokens(slug string) []string {
	return strings.Fields(strings.ReplaceAll(slug, "-", " "))
}

func tokenSet(text string) map[string]struct{} {
	fields := strings.Fields(separators.ReplaceAllString(strings.ToLower(text), " "))
	out := make(map[string]struct{}, len(fields))

	for _, field := range fields {
		out[field] = struct{}{}
	}

	return out
}

func containsAllTokens(textTokens map[string]struct{}, tokens []string) bool {
	required := 0

	for _, token := range tokens {
		if token == "" {
			continue
		}

		required++

		if _, ok := textTokens[token]; !ok {
			return false
		}
	}

	return required > 0
}

func stepAnchorTokens(step string) []string {
	fields := strings.Fields(separators.ReplaceAllString(step, " "))

	anchors := make([]string, 0, len(fields))
	for _, field := range fields {
		if isPlaceholderName(field) || isStopWord(field) {
			continue
		}

		anchors = append(anchors, field)
	}

	return anchors
}

func isPlaceholderName(field string) bool {
	switch field {
	case parameterIssue, parameterPath, parameterURL, parameterNumber, parameterID:
		return true
	default:
		return false
	}
}

func isStopWord(field string) bool {
	switch field {
	case "a", "an", "and", "for", "in", "of", "on", "or", "the", "to", "with":
		return true
	default:
		return false
	}
}
