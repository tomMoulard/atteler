// Package promptcomplete provides deterministic prompt-line completion
// primitives for local context such as agents, resources, tools, templates,
// slash commands, project symbols, session state, and recently touched files.
package promptcomplete

import (
	"slices"
	"sort"
	"strings"
	"unicode"
)

const (
	defaultLimit     = 5
	defaultMinPrefix = 1

	kindSlashCommand = "slash-command"
)

// Candidate is a local completion option.
//
// Text is the exact single-line text inserted into the prompt. Kind is a
// caller-defined category such as "agent", "resource", "tool", "template",
// "project-symbol", or "recent-file". Source describes where the candidate
// came from so callers can explain relevance and avoid treating completions as
// anonymous ranked words.
type Candidate struct {
	Text        string
	Kind        string
	Description string
	Source      string
	Explanation string
	Tokens      []string
	Hidden      bool
}

// Context describes the current prompt input and known local completion
// sources.
//
// Cursor is a byte offset into Input. Zero means the start of Input.
//
//nolint:govet // Public field order keeps candidate sources grouped before cursor metadata.
type Context struct {
	Candidates     []Candidate
	Agents         []Candidate
	Resources      []Candidate
	Tools          []Candidate
	Templates      []Candidate
	SlashCommands  []Candidate
	ProjectSymbols []Candidate
	RecentFiles    []Candidate
	Tasks          []Candidate
	Issues         []Candidate
	Permissions    []Candidate
	Input          string
	Cursor         int
}

// Options controls prompt-line suggestion behavior.
type Options struct {
	// Limit is the maximum number of suggestions returned by SuggestAll. Values
	// below 1 use a small default.
	Limit int
	// MinPrefix is the shortest token prefix that can produce suggestions.
	// Values below 1 use a small default.
	MinPrefix int
	// CaseSensitive makes prefix and context-token matching case-sensitive.
	CaseSensitive bool
}

// Suggestion is one deterministic completion for the current prompt line.
//
// Suffix is the text that should be appended at Cursor to accept the
// completion. ReplacementStart and ReplacementEnd identify the current token
// span that would become Text if a caller prefers replace-range semantics.
//
//nolint:govet // Public field order follows API readability and output shape.
type Suggestion struct {
	Text             string
	Suffix           string
	ReplacementStart int
	ReplacementEnd   int
	Candidate        Candidate
	RankSignals      []RankSignal
	Score            int
	Source           string
	Explanation      string
}

// RankSignal explains one deterministic ranking contribution.
//
// It is intentionally compact for CLI/TUI telemetry: Name is a stable category,
// Score is the signed contribution, and Detail is human-readable context.
type RankSignal struct {
	Name   string
	Detail string
	Score  int
}

type scoredCandidate struct {
	candidate Candidate
	result    scoreResult
	index     int
}

type scoreResult struct {
	text        string
	signals     []RankSignal
	score       int
	suffixStart int
}

// Suggest returns the highest-ranked prompt-line suggestion for ctx.
func Suggest(ctx Context, opts Options) (Suggestion, bool) {
	suggestions := SuggestAll(ctx, Options{
		Limit:         1,
		MinPrefix:     opts.MinPrefix,
		CaseSensitive: opts.CaseSensitive,
	})
	if len(suggestions) == 0 {
		return Suggestion{}, false
	}

	return suggestions[0], true
}

// SuggestAll ranks local candidates for the token at Context.Cursor. It does
// not suggest when the current line is ambiguous, the cursor is invalid, the
// candidate would introduce a newline, or the text after the cursor is not
// whitespace.
func SuggestAll(ctx Context, opts Options) []Suggestion {
	opts = normalizeOptions(opts)

	before, prefix, start, end, ok := currentLine(ctx.Input, ctx.Cursor)
	if !ok || len(prefix) < opts.MinPrefix {
		return nil
	}

	contextTokens := tokensBefore(before)
	candidates := collectCandidates(ctx)

	scored := make([]scoredCandidate, 0, len(candidates))
	for i, candidate := range candidates {
		candidate = normalizeCandidate(candidate)
		if candidate.Hidden || candidate.Text == "" || strings.ContainsAny(candidate.Text, "\r\n") {
			continue
		}

		result, matched := score(candidate, prefix, contextTokens, opts)
		if !matched {
			continue
		}

		scored = append(scored, scoredCandidate{candidate: candidate, result: result, index: i})
	}

	if len(scored) == 0 {
		return nil
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].result.score != scored[j].result.score {
			return scored[i].result.score > scored[j].result.score
		}

		if scored[i].candidate.Kind != scored[j].candidate.Kind {
			return scored[i].candidate.Kind < scored[j].candidate.Kind
		}

		if scored[i].candidate.Text != scored[j].candidate.Text {
			return scored[i].candidate.Text < scored[j].candidate.Text
		}

		return scored[i].index < scored[j].index
	})

	if len(scored) > opts.Limit {
		scored = scored[:opts.Limit]
	}

	out := make([]Suggestion, 0, len(scored))
	for i := range scored {
		item := &scored[i]
		out = append(out, buildSuggestion(item.candidate, prefix, start, end, item.result))
	}

	return out
}

// CandidatesFromNames builds same-kind candidates from local names.
func CandidatesFromNames(kind string, names ...string) []Candidate {
	out := make([]Candidate, 0, len(names))
	for _, name := range names {
		text := strings.TrimSpace(name)
		if text == "" {
			continue
		}

		out = append(out, Candidate{Text: text, Kind: kind})
	}

	return out
}

func normalizeOptions(opts Options) Options {
	if opts.Limit <= 0 {
		opts.Limit = defaultLimit
	}

	if opts.MinPrefix <= 0 {
		opts.MinPrefix = defaultMinPrefix
	}

	return opts
}

func collectCandidates(ctx Context) []Candidate {
	total := len(ctx.Candidates) +
		len(ctx.Agents) +
		len(ctx.Resources) +
		len(ctx.Tools) +
		len(ctx.Templates) +
		len(ctx.SlashCommands) +
		len(ctx.ProjectSymbols) +
		len(ctx.RecentFiles) +
		len(ctx.Tasks) +
		len(ctx.Issues) +
		len(ctx.Permissions)
	out := make([]Candidate, 0, total)
	out = append(out, ctx.Candidates...)
	out = appendWithKind(out, "agent", "configured agents", ctx.Agents)
	out = appendWithKind(out, "resource", "context resources", ctx.Resources)
	out = appendWithKind(out, "tool", "local tools", ctx.Tools)
	out = appendWithKind(out, "template", "prompt templates", ctx.Templates)
	out = appendWithKind(out, kindSlashCommand, "interactive slash commands", ctx.SlashCommands)
	out = appendWithKind(out, "project-symbol", "project symbol index", ctx.ProjectSymbols)
	out = appendWithKind(out, "recent-file", "recently touched files", ctx.RecentFiles)
	out = appendWithKind(out, "task", "task list", ctx.Tasks)
	out = appendWithKind(out, "issue", "issue context", ctx.Issues)
	out = appendWithKind(out, "permission", "active permissions", ctx.Permissions)

	return out
}

func appendWithKind(out []Candidate, kind, source string, candidates []Candidate) []Candidate {
	for _, candidate := range candidates {
		if candidate.Kind == "" {
			candidate.Kind = kind
		}

		if candidate.Source == "" {
			candidate.Source = source
		}

		out = append(out, candidate)
	}

	return out
}

func currentLine(input string, cursor int) (before, prefix string, start, end int, ok bool) {
	if cursor < 0 || cursor > len(input) {
		return "", "", 0, 0, false
	}

	lineStart := strings.LastIndex(input[:cursor], "\n") + 1
	nextNewline := strings.Index(input[cursor:], "\n")

	lineEnd := len(input)
	if nextNewline >= 0 {
		lineEnd = cursor + nextNewline
	}

	if strings.TrimSpace(input[cursor:lineEnd]) != "" {
		return "", "", 0, 0, false
	}

	start = cursor
	for start > lineStart && isPromptTokenRune(rune(input[start-1])) {
		start--
	}

	if start > lineStart && input[start-1] == '\\' {
		return "", "", 0, 0, false
	}

	prefix = input[start:cursor]
	before = input[lineStart:start]

	return before, prefix, start, cursor, true
}

func score(candidate Candidate, prefix string, contextTokens []string, opts Options) (scoreResult, bool) {
	insertText := insertionText(candidate, prefix)
	text := normalizeMatchText(insertText, opts)
	needle := normalizeMatchText(prefix, opts)

	result := scoreResult{text: insertText, suffixStart: len(prefix)}
	if candidate.Kind == kindSlashCommand && !strings.HasPrefix(prefix, "/") {
		return scoreResult{}, false
	}

	switch {
	case strings.HasPrefix(text, needle):
		points := 1000 + len(prefix)*10
		result.score += points
		result.signals = append(result.signals, RankSignal{
			Name:   "prefix",
			Detail: "text starts with " + quote(prefix),
			Score:  points,
		})

		if !opts.CaseSensitive {
			result.suffixStart = len(commonPrefixFold(insertText, prefix))
		}
	case segmentPrefixMatch(insertText, prefix, opts):
		points := 820 + len(prefix)*8
		result.score += points
		result.suffixStart = 0
		result.signals = append(result.signals, RankSignal{
			Name:   "segment-prefix",
			Detail: "path/name segment starts with " + quote(prefix),
			Score:  points,
		})
	case len(prefix) >= 3 && strings.Contains(text, needle):
		points := 620 + len(prefix)*4
		result.score += points
		result.suffixStart = 0
		result.signals = append(result.signals, RankSignal{
			Name:   "contains",
			Detail: "text contains " + quote(prefix),
			Score:  points,
		})
	default:
		return scoreResult{}, false
	}

	if len(insertText) == len(prefix) {
		const exactLengthBonus = 50

		result.score += exactLengthBonus
		result.signals = append(result.signals, RankSignal{
			Name:   "exact-length",
			Detail: "candidate exactly completes the token",
			Score:  exactLengthBonus,
		})
	}

	contextPoints, contextSignals := contextScore(candidate, contextTokens, opts)
	result.score += contextPoints
	result.signals = append(result.signals, contextSignals...)

	sourcePoints, sourceSignals := sourceRelevanceScore(candidate, prefix, contextTokens, opts)
	result.score += sourcePoints
	result.signals = append(result.signals, sourceSignals...)

	lengthPenalty := -len(insertText)
	result.score += lengthPenalty
	result.signals = append(result.signals, RankSignal{
		Name:   "length",
		Detail: "prefer shorter unambiguous insertions",
		Score:  lengthPenalty,
	})

	return result, true
}

func insertionText(candidate Candidate, prefix string) string {
	if strings.HasPrefix(prefix, "@") &&
		!strings.HasPrefix(candidate.Text, "@") &&
		atMentionCandidate(candidate.Kind) {
		return "@" + candidate.Text
	}

	return candidate.Text
}

func atMentionCandidate(kind string) bool {
	return kind == "agent" || kind == "resource" || kind == "recent-file"
}

func contextScore(candidate Candidate, contextTokens []string, opts Options) (int, []RankSignal) {
	if len(contextTokens) == 0 {
		return 0, nil
	}

	candidateTokens := candidateContextTokens(candidate)
	if !opts.CaseSensitive {
		for i, token := range candidateTokens {
			candidateTokens[i] = strings.ToLower(token)
		}
	}

	var (
		signals []RankSignal
		score   int
	)

	for _, token := range contextTokens {
		original := token
		if !opts.CaseSensitive {
			token = strings.ToLower(token)
		}

		if slices.Contains(candidateTokens, token) {
			const points = 80

			score += points
			signals = append(signals, RankSignal{
				Name:   "context-token",
				Detail: "prompt context mentions " + quote(original),
				Score:  points,
			})
		}
	}

	return score, signals
}

func sourceRelevanceScore(candidate Candidate, prefix string, contextTokens []string, opts Options) (int, []RankSignal) {
	var (
		signals []RankSignal
		score   int
	)

	if strings.HasPrefix(prefix, "/") && candidate.Kind == kindSlashCommand {
		const points = 220

		score += points
		signals = append(signals, RankSignal{Name: "source-cue", Detail: "slash-prefixed token asks for a slash command", Score: points})
	}

	if strings.HasPrefix(prefix, "#") && candidate.Kind == "issue" {
		const points = 220

		score += points
		signals = append(signals, RankSignal{Name: "source-cue", Detail: "hash-prefixed token asks for an issue reference", Score: points})
	}

	aliases := kindAliases(candidate.Kind)
	if len(aliases) == 0 || len(contextTokens) == 0 {
		return score, signals
	}

	aliasSet := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		aliasSet[normalizeMatchText(alias, opts)] = struct{}{}
	}

	for _, token := range contextTokens {
		normalized := normalizeMatchText(token, opts)
		if _, ok := aliasSet[normalized]; !ok {
			continue
		}

		const points = 140

		score += points
		signals = append(signals, RankSignal{
			Name:   "source-cue",
			Detail: "prompt asks for " + quote(token) + " context",
			Score:  points,
		})
	}

	return score, signals
}

func kindAliases(kind string) []string {
	switch kind {
	case "agent":
		return []string{"agent", "profile", "persona"}
	case "resource":
		return []string{"resource", "context", "reference"}
	case "tool":
		return []string{"tool", "command"}
	case "template":
		return []string{"template", "prompt"}
	case kindSlashCommand:
		return []string{"slash", "command", "cmd"}
	case "project-symbol":
		return []string{"symbol", "function", "func", "method", "type", "const", "var"}
	case "recent-file":
		return []string{"file", "path", "package", "diff", "changed", "touched"}
	case "task":
		return []string{"task", "todo", "work"}
	case "issue":
		return []string{"issue", "bug", "ticket", "gh"}
	case "permission":
		return []string{"permission", "permissions", "allowed", "tool"}
	default:
		return nil
	}
}

func candidateContextTokens(candidate Candidate) []string {
	tokens := make([]string, 0, 4+len(candidate.Tokens))
	tokens = append(tokens, candidate.Kind, candidate.Source)
	tokens = append(tokens, candidate.Tokens...)
	tokens = append(tokens, splitTokens(candidate.Text)...)
	tokens = append(tokens, splitTokens(candidate.Description)...)

	return compactTokens(tokens)
}

func normalizeCandidate(candidate Candidate) Candidate {
	candidate.Text = strings.TrimSpace(candidate.Text)
	candidate.Kind = strings.TrimSpace(candidate.Kind)
	candidate.Description = strings.TrimSpace(candidate.Description)
	candidate.Source = strings.TrimSpace(candidate.Source)
	candidate.Explanation = strings.TrimSpace(candidate.Explanation)
	candidate.Tokens = compactTokens(candidate.Tokens)

	return candidate
}

func buildSuggestion(candidate Candidate, prefix string, start, end int, result scoreResult) Suggestion {
	suffixStart := result.suffixStart

	text := result.text
	if text == "" {
		text = candidate.Text
	}

	if suffixStart < 0 || suffixStart > len(text) {
		suffixStart = 0
	}

	explanation := candidate.Explanation
	if explanation == "" {
		explanation = defaultExplanation(candidate, prefix, text)
	}

	return Suggestion{
		Text:             text,
		Suffix:           text[suffixStart:],
		ReplacementStart: start,
		ReplacementEnd:   end,
		Candidate:        candidate,
		RankSignals:      append([]RankSignal(nil), result.signals...),
		Score:            result.score,
		Source:           candidate.Source,
		Explanation:      explanation,
	}
}

func defaultExplanation(candidate Candidate, prefix, insertion string) string {
	source := candidate.Source
	if source == "" {
		source = "local completion context"
	}

	if candidate.Kind == "" {
		return "Local candidate from " + source + " matches " + quote(prefix) +
			"; accepting replaces the current token with " + quote(insertion) + "."
	}

	return "Local " + candidate.Kind + " from " + source + " matches " + quote(prefix) +
		"; accepting replaces the current token with " + quote(insertion) + "."
}

func tokensBefore(line string) []string {
	return splitTokens(line)
}

func splitTokens(s string) []string {
	var out []string

	start := -1

	for i, r := range s {
		if isTokenLetter(r) {
			if start == -1 {
				start = i
			}

			continue
		}

		if start != -1 {
			out = append(out, s[start:i])
			start = -1
		}
	}

	if start != -1 {
		out = append(out, s[start:])
	}

	return compactTokens(out)
}

func compactTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))

	seen := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		if seen[token] {
			continue
		}

		seen[token] = true
		out = append(out, token)
	}

	return out
}

func commonPrefixFold(text, prefix string) string {
	if len(prefix) > len(text) {
		return text
	}

	for i := range len(prefix) {
		if unicode.ToLower(rune(text[i])) != unicode.ToLower(rune(prefix[i])) {
			return text[:i]
		}
	}

	return text[:len(prefix)]
}

func segmentPrefixMatch(text, prefix string, opts Options) bool {
	if strings.HasPrefix(prefix, "@") && strings.HasPrefix(text, "@") {
		prefix = strings.TrimPrefix(prefix, "@")
	}

	needle := normalizeMatchText(prefix, opts)
	if needle == "" {
		return false
	}

	for _, segment := range completionSegments(text) {
		if strings.HasPrefix(normalizeMatchText(segment, opts), needle) {
			return true
		}
	}

	return false
}

func completionSegments(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !isTokenLetter(r)
	})

	return compactTokens(fields)
}

func normalizeMatchText(s string, opts Options) string {
	if opts.CaseSensitive {
		return s
	}

	return strings.ToLower(s)
}

func quote(s string) string {
	return `"` + s + `"`
}

func isPromptTokenRune(r rune) bool {
	return isTokenLetter(r) || r == '@' || r == '$' || r == '/' || r == '\\' || r == '.' || r == '-' || r == ':' || r == '#'
}

func isTokenLetter(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}
