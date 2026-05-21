// Package skill detects repeated action sequences that can be promoted into
// reusable skill suggestions.
package skill

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
)

const (
	defaultMinSteps       = 2
	defaultMaxSteps       = 6
	defaultMinOccurrences = 2

	parameterID     = "id"
	parameterIssue  = "issue"
	parameterNumber = "number"
	parameterPath   = "path"
	parameterURL    = "url"

	observationInput  = "input"
	observationOutput = "output"
	observationPrompt = "prompt"
	observationStop   = "stop"
	observationTool   = "tool"
	observationVerify = "verify"
)

var (
	separators   = regexp.MustCompile(`[^a-z0-9]+`)
	issueRef     = regexp.MustCompile(`^(?:#\d+|gh-\d+|[a-z][a-z0-9]*-\d+)$`)
	numberValue  = regexp.MustCompile(`^\d+$`)
	hexLikeValue = regexp.MustCompile(`^[0-9a-f]{7,}$`)
)

// Options controls how repeated action sequences are detected.
type Options struct {
	// MinSteps is the shortest multi-step sequence to consider. Values below 2
	// are treated as 2 because a skill suggestion must contain multiple steps.
	MinSteps int
	// MaxSteps is the longest sequence to consider. If unset, a small default is
	// used so suggestions stay focused and explainable.
	MaxSteps int
	// MinOccurrences is the minimum number of non-overlapping repetitions needed.
	// Values below 2 are treated as 2.
	MinOccurrences int
}

// Suggestion describes a repeated workflow that could become a named skill.
//
//nolint:govet // Public field order follows readability and output semantics.
type Suggestion struct {
	// Name is a human-readable skill name derived from the detected steps.
	Name string
	// Slug is a stable lowercase identifier derived from the detected steps.
	Slug string
	// Steps is the normalized, parameterized command/action sequence that
	// repeated. Obvious session-specific values are replaced with placeholders
	// such as {{issue}} or {{path}}.
	Steps []string
	// Workflow contains the per-step provenance needed to turn the suggestion
	// into executable guidance instead of a receipt of action labels.
	Workflow []WorkflowStep
	// Parameters describes placeholders found while normalizing repeated steps.
	Parameters []Parameter
	// TriggerEvals are lightweight fixtures that document prompts expected to
	// trigger or not trigger the generated skill.
	TriggerEvals []TriggerEval
	// Occurrences is the number of non-overlapping times Steps appeared.
	Occurrences int
	// Rationale explains why this sequence was suggested.
	Rationale string
}

// Observation is one recorded action with optional provenance captured by the
// caller. Action is required; the remaining fields are carried into generated
// SKILL.md guidance when the repeated workflow is accepted.
type Observation struct {
	Action               string
	Prompt               string
	ToolClass            string
	Inputs               []string
	Outputs              []string
	VerificationCommands []string
	StopConditions       []string
}

// WorkflowStep is the executable guidance for one synthesized skill step.
type WorkflowStep struct {
	Action               string
	SourceActions        []string
	Prompts              []string
	ToolClasses          []string
	Inputs               []string
	Outputs              []string
	VerificationCommands []string
	StopConditions       []string
}

// Parameter describes one placeholder extracted from repeated observations.
type Parameter struct {
	Name        string
	Placeholder string
	Description string
	Examples    []string
}

// TriggerEval records one intended or rejected prompt for generated-skill
// trigger checks.
type TriggerEval struct {
	Prompt        string
	Reason        string
	ShouldTrigger bool
}

type candidate struct {
	steps            []string
	occurrenceStarts []int
	occurrences      int
	firstIndex       int
}

type normalizedObservation struct {
	action       string
	sourceAction string
	parameters   map[string][]string
	observation  Observation
}

// ParseObservationSpec converts a CLI-friendly observation string into
// provenance. The compact syntax is:
//
//	action | prompt=<prompt> | tool=<class> | input=<value> | output=<value> | verify=<command> | stop=<condition>
//
// If any metadata segment is malformed or unknown, the whole string is treated
// as a literal action to preserve backward compatibility with action labels
// that contain pipe characters.
func ParseObservationSpec(raw string) Observation {
	parts := splitObservationSpec(raw)
	if len(parts) < 2 {
		return Observation{Action: strings.TrimSpace(raw)}
	}

	observation := Observation{Action: parts[0]}
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return Observation{Action: strings.TrimSpace(raw)}
		}

		key = normalizeObservationKey(key)

		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			return Observation{Action: strings.TrimSpace(raw)}
		}

		if !applyObservationMetadata(&observation, key, value) {
			return Observation{Action: strings.TrimSpace(raw)}
		}
	}

	return normalizeObservation(observation)
}

// Suggest returns the strongest repeated multi-step sequence in observed.
// Empty action names are ignored. The boolean return is false when there is no
// sequence that satisfies the default thresholds.
func Suggest(observed []string) (Suggestion, bool) {
	return SuggestWithOptions(observed, Options{})
}

// SuggestWithOptions returns the strongest repeated multi-step sequence in
// observed using opts. Strength favors workflows with more repeated work
// (steps × occurrences), then longer step sequences, then earlier appearance.
func SuggestWithOptions(observed []string, opts Options) (Suggestion, bool) {
	observations := make([]Observation, 0, len(observed))
	for _, action := range observed {
		observations = append(observations, ParseObservationSpec(action))
	}

	return SuggestFromObservations(observations, opts)
}

// SuggestFromObservations returns the strongest repeated multi-step workflow in
// observed using opts. Unlike SuggestWithOptions, it preserves prompt, tool,
// input/output, verification, and stop-condition provenance for persisted
// skills.
func SuggestFromObservations(observed []Observation, opts Options) (Suggestion, bool) {
	opts = normalizeOptions(opts)

	observations := normalizeObservations(observed)
	if len(observations) < opts.MinSteps*opts.MinOccurrences {
		return Suggestion{}, false
	}

	if opts.MaxSteps > len(observations)/opts.MinOccurrences {
		opts.MaxSteps = len(observations) / opts.MinOccurrences
	}

	var best candidate

	found := false

	for size := opts.MinSteps; size <= opts.MaxSteps; size++ {
		startsByKey := make(map[string][]int)
		stepsByKey := make(map[string][]string)

		for start := 0; start+size <= len(observations); start++ {
			steps := actionWindow(observations, start, size)
			key := strings.Join(steps, "\x00")

			startsByKey[key] = append(startsByKey[key], start)
			if _, ok := stepsByKey[key]; !ok {
				stepsByKey[key] = append([]string(nil), steps...)
			}
		}

		for key, starts := range startsByKey {
			occurrenceStarts := nonOverlappingStarts(starts, size)
			if len(occurrenceStarts) < opts.MinOccurrences {
				continue
			}

			cand := candidate{
				steps:            stepsByKey[key],
				occurrences:      len(occurrenceStarts),
				firstIndex:       starts[0],
				occurrenceStarts: occurrenceStarts,
			}
			if !found || better(cand, best) {
				best = cand
				found = true
			}
		}
	}

	if !found {
		return Suggestion{}, false
	}

	return buildSuggestion(best, observations), true
}

func normalizeOptions(opts Options) Options {
	if opts.MinSteps < defaultMinSteps {
		opts.MinSteps = defaultMinSteps
	}

	if opts.MaxSteps <= 0 {
		opts.MaxSteps = defaultMaxSteps
	}

	if opts.MaxSteps < opts.MinSteps {
		opts.MaxSteps = opts.MinSteps
	}

	if opts.MinOccurrences < defaultMinOccurrences {
		opts.MinOccurrences = defaultMinOccurrences
	}

	return opts
}

func normalizeObservations(observed []Observation) []normalizedObservation {
	observations := make([]normalizedObservation, 0, len(observed))
	for i := range observed {
		observation := &observed[i]

		normalized := normalizeStep(observation.Action)
		if normalized == "" {
			continue
		}

		action, parameters := parameterizeAction(normalized)
		observations = append(observations, normalizedObservation{
			action:       action,
			sourceAction: normalized,
			parameters:   parameters,
			observation:  normalizeObservation(*observation),
		})
	}

	return observations
}

func normalizeStep(action string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(action))), " ")
}

func normalizeObservation(observation Observation) Observation {
	return Observation{
		Action:               strings.TrimSpace(observation.Action),
		Prompt:               strings.TrimSpace(observation.Prompt),
		ToolClass:            strings.TrimSpace(observation.ToolClass),
		Inputs:               cleanStrings(observation.Inputs),
		Outputs:              cleanStrings(observation.Outputs),
		VerificationCommands: cleanStrings(observation.VerificationCommands),
		StopConditions:       cleanStrings(observation.StopConditions),
	}
}

func splitObservationSpec(raw string) []string {
	parts := make([]string, 0, 2)

	for part := range strings.SplitSeq(raw, "|") {
		part = strings.TrimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}

	return parts
}

func normalizeObservationKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "_", "-")

	switch key {
	case observationPrompt:
		return observationPrompt
	case "tool", "tool-class", "toolclass":
		return observationTool
	case "input", "in":
		return observationInput
	case "output", "out":
		return observationOutput
	case "verify", "verification", "verification-command":
		return observationVerify
	case "stop", "stop-condition":
		return observationStop
	default:
		return ""
	}
}

func applyObservationMetadata(observation *Observation, key, value string) bool {
	switch key {
	case observationPrompt:
		observation.Prompt = appendPrompt(observation.Prompt, value)
	case observationTool:
		observation.ToolClass = value
	case observationInput:
		observation.Inputs = append(observation.Inputs, value)
	case observationOutput:
		observation.Outputs = append(observation.Outputs, value)
	case observationVerify:
		observation.VerificationCommands = append(observation.VerificationCommands, value)
	case observationStop:
		observation.StopConditions = append(observation.StopConditions, value)
	default:
		return false
	}

	return true
}

func appendPrompt(existing, value string) string {
	existing = strings.TrimSpace(existing)
	value = strings.TrimSpace(value)

	if existing == "" {
		return value
	}

	return existing + "\n" + value
}

func cleanStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}

	return out
}

func actionWindow(observations []normalizedObservation, start, size int) []string {
	steps := make([]string, 0, size)

	for i := start; i < start+size; i++ {
		steps = append(steps, observations[i].action)
	}

	return steps
}

func nonOverlappingStarts(starts []int, size int) []int {
	out := make([]int, 0, len(starts))

	nextAllowed := 0
	for _, start := range starts {
		if start < nextAllowed {
			continue
		}

		out = append(out, start)
		nextAllowed = start + size
	}

	return out
}

func better(cand, best candidate) bool {
	candScore := len(cand.steps) * cand.occurrences

	bestScore := len(best.steps) * best.occurrences
	if candScore != bestScore {
		return candScore > bestScore
	}

	if len(cand.steps) != len(best.steps) {
		return len(cand.steps) > len(best.steps)
	}

	if cand.occurrences != best.occurrences {
		return cand.occurrences > best.occurrences
	}

	return cand.firstIndex < best.firstIndex
}

func buildSuggestion(c candidate, observations []normalizedObservation) Suggestion {
	slug := slugForSteps(c.steps)
	parameters := parametersForCandidate(c, observations)
	workflow := workflowForCandidate(c, observations)

	suggestion := Suggestion{
		Name:        nameFromSlug(slug),
		Slug:        slug,
		Steps:       append([]string(nil), c.steps...),
		Workflow:    workflow,
		Parameters:  parameters,
		Occurrences: c.occurrences,
		Rationale: fmt.Sprintf(
			"Observed the %d-step sequence %q repeat %d times, making it a good candidate for a reusable skill.",
			len(c.steps), strings.Join(c.steps, " → "), c.occurrences,
		),
	}
	suggestion.TriggerEvals = BuildTriggerEvals(suggestion)

	return suggestion
}

func slugForSteps(steps []string) string {
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		part := separators.ReplaceAllString(strings.ToLower(step), "-")

		part = strings.Trim(part, "-")
		if part != "" {
			parts = append(parts, part)
		}
	}

	return strings.Join(parts, "-")
}

func nameFromSlug(slug string) string {
	words := strings.Split(slug, "-")
	for i, word := range words {
		if word == "" {
			continue
		}

		letters := []rune(word)
		letters[0] = unicode.ToUpper(letters[0])
		words[i] = string(letters)
	}

	return strings.Join(words, " ") + " Skill"
}

func parameterizeAction(action string) (template string, parameters map[string][]string) {
	fields := strings.Fields(action)
	if len(fields) == 0 {
		return "", nil
	}

	parameters = make(map[string][]string)

	out := make([]string, 0, len(fields))
	for _, field := range fields {
		prefix, core, suffix := splitToken(field)

		name := parameterName(core)
		if name == "" {
			out = append(out, field)
			continue
		}

		placeholder := "{{" + name + "}}"
		out = append(out, prefix+placeholder+suffix)

		addMapUnique(parameters, name, core)
	}

	return strings.Join(out, " "), parameters
}

func splitToken(token string) (prefix, core, suffix string) {
	start := 0
	for start < len(token) && isTrimPunctuation(rune(token[start])) {
		start++
	}

	end := len(token)
	for end > start && isTrimPunctuation(rune(token[end-1])) {
		end--
	}

	return token[:start], token[start:end], token[end:]
}

func isTrimPunctuation(r rune) bool {
	switch r {
	case '"', '\'', '`', '(', ')', '[', ']', '{', '}', '<', '>', ',', ';', ':':
		return true
	default:
		return false
	}
}

func parameterName(token string) string {
	token = strings.ToLower(strings.TrimSpace(token))
	if token == "" {
		return ""
	}

	switch {
	case strings.HasPrefix(token, "http://") || strings.HasPrefix(token, "https://"):
		return parameterURL
	case issueRef.MatchString(token):
		return parameterIssue
	case strings.Contains(token, "/") || strings.Contains(token, `\`) || isKnownPath(token):
		return parameterPath
	case numberValue.MatchString(token):
		return parameterNumber
	case hexLikeValue.MatchString(token):
		return parameterID
	default:
		return ""
	}
}

func isKnownPath(token string) bool {
	switch strings.ToLower(filepath.Ext(token)) {
	case ".go", ".md", ".yaml", ".yml", ".json", ".js", ".jsx", ".ts", ".tsx", ".py", ".sh", ".txt":
		return true
	default:
		return false
	}
}

func parametersForCandidate(c candidate, observations []normalizedObservation) []Parameter {
	byName := make(map[string]*Parameter)

	for _, start := range c.occurrenceStarts {
		for offset := range c.steps {
			for name, examples := range observations[start+offset].parameters {
				parameter, ok := byName[name]
				if !ok {
					parameter = &Parameter{
						Name:        name,
						Placeholder: "{{" + name + "}}",
						Description: parameterDescription(name),
					}
					byName[name] = parameter
				}

				for _, example := range examples {
					parameter.Examples = appendUnique(parameter.Examples, example)
				}
			}
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}

	sort.Strings(names)

	parameters := make([]Parameter, 0, len(names))
	for _, name := range names {
		parameters = append(parameters, *byName[name])
	}

	return parameters
}

func parameterDescription(name string) string {
	switch name {
	case parameterIssue:
		return "Issue, ticket, pull request, or task reference supplied by the user."
	case parameterPath:
		return "Repository, file, directory, or package path supplied by the user."
	case parameterURL:
		return "External or local URL supplied by the user."
	case parameterNumber:
		return "Numeric value supplied by the user."
	case parameterID:
		return "Opaque identifier supplied by the user."
	default:
		return "Value supplied by the user."
	}
}

func workflowForCandidate(c candidate, observations []normalizedObservation) []WorkflowStep {
	workflow := make([]WorkflowStep, len(c.steps))
	for offset, step := range c.steps {
		workflow[offset] = WorkflowStep{Action: step}

		for _, start := range c.occurrenceStarts {
			observed := observations[start+offset]

			sourceAction := observed.sourceAction
			if sourceAction != "" && sourceAction != step {
				workflow[offset].SourceActions = appendUnique(workflow[offset].SourceActions, sourceAction)
			}

			observation := observed.observation
			workflow[offset].Prompts = appendUniqueTrimmed(workflow[offset].Prompts, observation.Prompt)
			workflow[offset].Inputs = appendUniqueAll(workflow[offset].Inputs, observation.Inputs)
			workflow[offset].Outputs = appendUniqueAll(workflow[offset].Outputs, observation.Outputs)
			workflow[offset].VerificationCommands = appendUniqueAll(workflow[offset].VerificationCommands, observation.VerificationCommands)
			workflow[offset].StopConditions = appendUniqueAll(workflow[offset].StopConditions, observation.StopConditions)

			toolClass := observation.ToolClass
			if strings.TrimSpace(toolClass) == "" {
				toolClass = inferToolClass(sourceAction)
			}

			workflow[offset].ToolClasses = appendUniqueTrimmed(workflow[offset].ToolClasses, toolClass)
		}
	}

	return workflow
}

func inferToolClass(action string) string {
	action = normalizeStep(action)
	switch {
	case containsAnySubstring(action, "test", "build", "shell", "command", "make ", "run "):
		return "shell"
	case containsAnySubstring(action, "issue", "pull request", "pr "):
		return "github"
	case containsAnySubstring(action, "edit", "patch", "write", "fix"):
		return "file-edit"
	case containsAnySubstring(action, "read", "inspect", "open"):
		return "file-read"
	case containsAnySubstring(action, "search", "grep", "find"):
		return "search"
	default:
		return "agent-guidance"
	}
}

func containsAnySubstring(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}

	return false
}

func addMapUnique(values map[string][]string, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	values[key] = appendUnique(values[key], value)
}

func appendUniqueAll(existing, values []string) []string {
	for _, value := range values {
		existing = appendUniqueTrimmed(existing, value)
	}

	return existing
}

func appendUniqueTrimmed(existing []string, value string) []string {
	return appendUnique(existing, strings.TrimSpace(value))
}

func appendUnique(existing []string, value string) []string {
	if value == "" {
		return existing
	}

	if slices.Contains(existing, value) {
		return existing
	}

	return append(existing, value)
}
