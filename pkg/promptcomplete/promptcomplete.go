// Package promptcomplete provides deterministic prompt-line completion
// primitives for local candidates such as agents, resources, tools, and
// templates.
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
)

// Candidate is a local completion option.
//
// Text is the exact single-line text inserted into the prompt. Kind is a
// caller-defined category such as "agent", "resource", "tool", or "template".
//
//nolint:govet // Public field order follows API readability and output shape.
type Candidate struct {
	Text        string
	Kind        string
	Description string
	Tokens      []string
	Explanation string
}

// Context describes the current prompt input and known local completion
// sources.
//
// Cursor is a byte offset into Input. Zero means the start of Input.
//
//nolint:govet // Public field order keeps candidate sources grouped before cursor metadata.
type Context struct {
	Candidates []Candidate
	Agents     []Candidate
	Resources  []Candidate
	Tools      []Candidate
	Templates  []Candidate
	Input      string
	Cursor     int
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
	Score            int
	Explanation      string
}

type scoredCandidate struct {
	candidate Candidate
	score     int
	index     int
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
		if candidate.Text == "" || strings.ContainsAny(candidate.Text, "\r\n") {
			continue
		}
		score, matched := score(candidate, prefix, contextTokens, opts)
		if !matched {
			continue
		}
		scored = append(scored, scoredCandidate{candidate: candidate, score: score, index: i})
	}
	if len(scored) == 0 {
		return nil
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
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
	for _, item := range scored {
		out = append(out, buildSuggestion(item.candidate, prefix, start, end, item.score, opts))
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
	total := len(ctx.Candidates) + len(ctx.Agents) + len(ctx.Resources) + len(ctx.Tools) + len(ctx.Templates)
	out := make([]Candidate, 0, total)
	out = append(out, ctx.Candidates...)
	out = appendWithKind(out, "agent", ctx.Agents)
	out = appendWithKind(out, "resource", ctx.Resources)
	out = appendWithKind(out, "tool", ctx.Tools)
	out = appendWithKind(out, "template", ctx.Templates)
	return out
}

func appendWithKind(out []Candidate, kind string, candidates []Candidate) []Candidate {
	for _, candidate := range candidates {
		if candidate.Kind == "" {
			candidate.Kind = kind
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

func score(candidate Candidate, prefix string, contextTokens []string, opts Options) (int, bool) {
	text := candidate.Text
	needle := prefix
	if !opts.CaseSensitive {
		text = strings.ToLower(text)
		needle = strings.ToLower(needle)
	}
	if !strings.HasPrefix(text, needle) {
		return 0, false
	}

	score := 1000 + len(prefix)*10
	if len(candidate.Text) == len(prefix) {
		score += 50
	}
	score += contextScore(candidate, contextTokens, opts)
	score -= len(candidate.Text)
	return score, true
}

func contextScore(candidate Candidate, contextTokens []string, opts Options) int {
	if len(contextTokens) == 0 {
		return 0
	}
	candidateTokens := candidateContextTokens(candidate)
	if !opts.CaseSensitive {
		for i, token := range candidateTokens {
			candidateTokens[i] = strings.ToLower(token)
		}
	}

	score := 0
	for _, token := range contextTokens {
		if !opts.CaseSensitive {
			token = strings.ToLower(token)
		}
		if slices.Contains(candidateTokens, token) {
			score += 80
		}
	}
	return score
}

func candidateContextTokens(candidate Candidate) []string {
	tokens := make([]string, 0, 4+len(candidate.Tokens))
	tokens = append(tokens, candidate.Kind)
	tokens = append(tokens, candidate.Tokens...)
	tokens = append(tokens, splitTokens(candidate.Text)...)
	tokens = append(tokens, splitTokens(candidate.Description)...)
	return compactTokens(tokens)
}

func normalizeCandidate(candidate Candidate) Candidate {
	candidate.Text = strings.TrimSpace(candidate.Text)
	candidate.Kind = strings.TrimSpace(candidate.Kind)
	candidate.Description = strings.TrimSpace(candidate.Description)
	candidate.Explanation = strings.TrimSpace(candidate.Explanation)
	candidate.Tokens = compactTokens(candidate.Tokens)
	return candidate
}

func buildSuggestion(candidate Candidate, prefix string, start, end, score int, opts Options) Suggestion {
	suffixStart := len(prefix)
	text := candidate.Text
	if !opts.CaseSensitive {
		suffixStart = len(commonPrefixFold(text, prefix))
	}
	explanation := candidate.Explanation
	if explanation == "" {
		explanation = defaultExplanation(candidate, prefix)
	}
	return Suggestion{
		Text:             text,
		Suffix:           text[suffixStart:],
		ReplacementStart: start,
		ReplacementEnd:   end,
		Candidate:        candidate,
		Score:            score,
		Explanation:      explanation,
	}
}

func defaultExplanation(candidate Candidate, prefix string) string {
	if candidate.Kind == "" {
		return "Matches the current prompt-line prefix " + quote(prefix) + "."
	}
	return "Matches the current prompt-line prefix " + quote(prefix) + " as a local " + candidate.Kind + " candidate."
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

func quote(s string) string {
	return `"` + s + `"`
}

func isPromptTokenRune(r rune) bool {
	return isTokenLetter(r) || r == '@' || r == '$' || r == '/' || r == '.' || r == '-' || r == ':'
}

func isTokenLetter(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}
