package promptcomplete

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSuggest_AgentSuffixAndReplacementRange(t *testing.T) {
	t.Parallel()

	got, ok := Suggest(Context{
		Input:  "ask res",
		Cursor: len("ask res"),
		Agents: CandidatesFromNames("agent", "researcher", "reviewer"),
	}, Options{})
	if !ok {
		t.Fatal("Suggest returned ok=false, want a suggestion")
	}

	if got.Text != "researcher" {
		t.Fatalf("Text = %q, want researcher", got.Text)
	}

	if got.Suffix != "earcher" {
		t.Fatalf("Suffix = %q, want earcher", got.Suffix)
	}

	if got.ReplacementStart != len("ask ") || got.ReplacementEnd != len("ask res") {
		t.Fatalf("replacement = [%d:%d], want [%d:%d]", got.ReplacementStart, got.ReplacementEnd, len("ask "), len("ask res"))
	}

	if !strings.Contains(got.Explanation, "agent") || !strings.Contains(got.Explanation, `"res"`) {
		t.Fatalf("Explanation = %q, want agent prefix context", got.Explanation)
	}
}

func TestSuggest_AgentAtMentionPreservesSigil(t *testing.T) {
	t.Parallel()

	got, ok := Suggest(Context{
		Input:  "ask @res",
		Cursor: len("ask @res"),
		Agents: CandidatesFromNames("agent", "researcher"),
	}, Options{})

	require.True(t, ok)
	assert.Equal(t, "@researcher", got.Text)
	assert.Equal(t, "earcher", got.Suffix)
	assert.Equal(t, len("ask "), got.ReplacementStart)
	assert.Equal(t, len("ask @res"), got.ReplacementEnd)
	assert.Equal(t, "researcher", got.Candidate.Text)
	assert.Contains(t, got.Explanation, `"@researcher"`)
}

func TestSuggest_AtMentionPathCanReplaceBasenamePrefix(t *testing.T) {
	t.Parallel()

	got, ok := Suggest(Context{
		Input:  "open @promptc",
		Cursor: len("open @promptc"),
		RecentFiles: []Candidate{{
			Text:        "pkg/promptcomplete/promptcomplete.go",
			Description: "recently touched prompt completion source",
		}},
	}, Options{})

	require.True(t, ok)
	assert.Equal(t, "@pkg/promptcomplete/promptcomplete.go", got.Text)
	assert.Equal(t, got.Text, got.Suffix)
	assert.Equal(t, len("open "), got.ReplacementStart)
	assert.Equal(t, len("open @promptc"), got.ReplacementEnd)
	assert.True(t, hasRankSignal(got, "segment-prefix"), "signals = %#v", got.RankSignals)
}

func TestSuggestAll_RanksPrefixThenContextTokens(t *testing.T) {
	t.Parallel()

	got := SuggestAll(Context{
		Input:  "use tool f",
		Cursor: len("use tool f"),
		Resources: []Candidate{
			{Text: "fixtures", Description: "local test files"},
		},
		Tools: []Candidate{
			{Text: "format", Description: "go formatting"},
		},
	}, Options{})

	if len(got) < 2 {
		t.Fatalf("SuggestAll returned %d suggestions, want at least 2: %#v", len(got), got)
	}

	if got[0].Text != "format" {
		t.Fatalf("first Text = %q, want format; all = %#v", got[0].Text, got)
	}

	if got[0].Score <= got[1].Score {
		t.Fatalf("scores = %d, %d; want context-ranked tool first", got[0].Score, got[1].Score)
	}
}

func TestSuggestAll_UsesCandidateTokensForContext(t *testing.T) {
	t.Parallel()

	got := SuggestAll(Context{
		Input:  "route fast ex",
		Cursor: len("route fast ex"),
		Candidates: []Candidate{
			{Text: "executor", Kind: "agent", Tokens: []string{"standard"}},
			{Text: "explore", Kind: "agent", Tokens: []string{"fast"}},
		},
	}, Options{})

	if len(got) != 2 {
		t.Fatalf("SuggestAll returned %d suggestions, want 2: %#v", len(got), got)
	}

	if got[0].Text != "explore" {
		t.Fatalf("first Text = %q, want explore; all = %#v", got[0].Text, got)
	}
}

func TestSuggestAll_UsesLiveContextSourcesAndTelemetry(t *testing.T) {
	t.Parallel()

	got := SuggestAll(Context{
		Input:  "fix symbol Su",
		Cursor: len("fix symbol Su"),
		ProjectSymbols: []Candidate{{
			Text:        "SuggestAll",
			Description: "function in pkg/promptcomplete/promptcomplete.go",
			Tokens:      []string{"func", "promptcomplete"},
		}},
		Templates: []Candidate{{
			Text:        "summary",
			Description: "summarize the current session",
		}},
	}, Options{})

	require.Len(t, got, 2)
	assert.Equal(t, "SuggestAll", got[0].Text)
	assert.Equal(t, "project-symbol", got[0].Candidate.Kind)
	assert.Equal(t, "project symbol index", got[0].Source)
	assert.Contains(t, got[0].Explanation, "project-symbol")
	assert.Contains(t, got[0].Explanation, "project symbol index")
	assert.True(t, hasRankSignal(got[0], "source-cue"), "signals = %#v", got[0].RankSignals)
	assert.Greater(t, got[0].Score, got[1].Score)
}

func TestSuggestAll_MatchesPathBasenameAndReportsReplacement(t *testing.T) {
	t.Parallel()

	got, ok := Suggest(Context{
		Input:  "open promptc",
		Cursor: len("open promptc"),
		RecentFiles: []Candidate{{
			Text:        "pkg/promptcomplete/promptcomplete.go",
			Description: "modified Go source",
		}},
	}, Options{})

	require.True(t, ok)
	assert.Equal(t, "pkg/promptcomplete/promptcomplete.go", got.Text)
	assert.Equal(t, len("open "), got.ReplacementStart)
	assert.Equal(t, len("open promptc"), got.ReplacementEnd)
	assert.Equal(t, got.Text, got.Suffix)
	assert.Equal(t, "recent-file", got.Candidate.Kind)
	assert.Contains(t, got.Explanation, "recently touched files")
	assert.True(t, hasRankSignal(got, "segment-prefix"), "signals = %#v", got.RankSignals)
}

func TestSuggestAll_ReturnsSeparatePathAmbiguityCandidates(t *testing.T) {
	t.Parallel()

	got := SuggestAll(Context{
		Input:  "open config",
		Cursor: len("open config"),
		RecentFiles: []Candidate{
			{Text: "cmd/atteler/config.go", Description: "CLI config file"},
			{Text: "pkg/config/config.go", Description: "package config file"},
		},
	}, Options{})

	require.Len(t, got, 2)
	assert.ElementsMatch(t, []string{"cmd/atteler/config.go", "pkg/config/config.go"}, []string{got[0].Text, got[1].Text})

	for _, suggestion := range got {
		assert.Equal(t, len("open "), suggestion.ReplacementStart)
		assert.Equal(t, len("open config"), suggestion.ReplacementEnd)
		assert.Equal(t, suggestion.Text, suggestion.Suffix)
		assert.True(t, hasRankSignal(suggestion, "segment-prefix"), "signals = %#v", suggestion.RankSignals)
	}
}

func TestSuggestAll_SkipsHiddenCandidates(t *testing.T) {
	t.Parallel()

	got := SuggestAll(Context{
		Input:  "ask int",
		Cursor: len("ask int"),
		Agents: []Candidate{
			{Text: "internal", Hidden: true},
			{Text: "integrator"},
		},
	}, Options{})

	require.Len(t, got, 1)
	assert.Equal(t, "integrator", got[0].Text)
}

func TestSuggestAll_IssueReferenceUsesHashToken(t *testing.T) {
	t.Parallel()

	got, ok := Suggest(Context{
		Input:  "related #2",
		Cursor: len("related #2"),
		Issues: []Candidate{{
			Text:        "#27",
			Description: "Make prompt completion context-aware",
		}},
	}, Options{})

	require.True(t, ok)
	assert.Equal(t, "#27", got.Text)
	assert.Equal(t, len("related "), got.ReplacementStart)
	assert.Equal(t, "issue", got.Candidate.Kind)
	assert.True(t, hasRankSignal(got, "source-cue"), "signals = %#v", got.RankSignals)
}

func TestSuggestAll_TemplateCompletionAndCustomExplanation(t *testing.T) {
	t.Parallel()

	got, ok := Suggest(Context{
		Input:  "please re",
		Cursor: len("please re"),
		Templates: []Candidate{
			{
				Text:        "review this change for regressions",
				Description: "code review prompt template",
				Explanation: "Completes a local code review prompt template.",
			},
		},
	}, Options{})
	if !ok {
		t.Fatal("Suggest returned ok=false, want a suggestion")
	}

	if got.Suffix != "view this change for regressions" {
		t.Fatalf("Suffix = %q, want template suffix", got.Suffix)
	}

	if got.Candidate.Kind != "template" {
		t.Fatalf("Candidate.Kind = %q, want template", got.Candidate.Kind)
	}

	if got.Explanation != "Completes a local code review prompt template." {
		t.Fatalf("Explanation = %q, want custom explanation", got.Explanation)
	}
}

func TestSuggestAll_AvoidsMultilineAmbiguity(t *testing.T) {
	t.Parallel()

	//nolint:govet // Test table order follows readability.
	tests := []struct {
		name       string
		input      string
		cursor     int
		candidates []Candidate
	}{
		{
			name:       "candidate with newline",
			input:      "use r",
			cursor:     len("use r"),
			candidates: []Candidate{{Text: "researcher\nwith more"}},
		},
		{
			name:       "candidate with carriage return",
			input:      "use r",
			cursor:     len("use r"),
			candidates: []Candidate{{Text: "researcher\rwith more"}},
		},
		{
			name:       "non whitespace after cursor on same line",
			input:      "use r now",
			cursor:     len("use r"),
			candidates: CandidatesFromNames("agent", "researcher"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := Context{
				Input:  tt.input,
				Cursor: tt.cursor,
				Agents: tt.candidates,
			}
			if got := SuggestAll(ctx, Options{}); len(got) != 0 {
				t.Fatalf("SuggestAll returned %#v, want no multiline-ambiguous suggestions", got)
			}
		})
	}
}

func TestSuggestAll_CompletesCurrentLineInMultilineInput(t *testing.T) {
	t.Parallel()

	input := "first line\nuse res"

	got, ok := Suggest(Context{
		Input:  input,
		Cursor: len(input),
		Agents: CandidatesFromNames("agent", "researcher"),
	}, Options{})
	if !ok {
		t.Fatal("Suggest returned ok=false, want a suggestion")
	}

	if got.ReplacementStart != len("first line\nuse ") {
		t.Fatalf("ReplacementStart = %d, want %d", got.ReplacementStart, len("first line\nuse "))
	}

	if got.Suffix != "earcher" {
		t.Fatalf("Suffix = %q, want earcher", got.Suffix)
	}
}

func TestSuggestAll_OptionsLimitMinPrefixAndCaseSensitivity(t *testing.T) {
	t.Parallel()

	ctx := Context{
		Input:  "use R",
		Cursor: len("use R"),
		Agents: CandidatesFromNames("agent", "researcher", "reviewer"),
	}
	if got := SuggestAll(ctx, Options{MinPrefix: 2}); len(got) != 0 {
		t.Fatalf("SuggestAll with MinPrefix returned %#v, want none", got)
	}

	if got := SuggestAll(ctx, Options{Limit: 1}); len(got) != 1 {
		t.Fatalf("SuggestAll with Limit returned %d suggestions, want 1", len(got))
	}

	if got := SuggestAll(ctx, Options{CaseSensitive: true}); len(got) != 0 {
		t.Fatalf("SuggestAll with CaseSensitive returned %#v, want none", got)
	}
}

func TestCandidatesFromNames_TrimsAndSkipsEmptyNames(t *testing.T) {
	t.Parallel()

	got := CandidatesFromNames("tool", " format ", "", "test")

	want := []Candidate{
		{Text: "format", Kind: "tool"},
		{Text: "test", Kind: "tool"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CandidatesFromNames = %#v, want %#v", got, want)
	}
}

func TestSuggest_InvalidCursorAndEmptyPrefix(t *testing.T) {
	t.Parallel()

	ctx := Context{Input: "use ", Cursor: len("use "), Agents: CandidatesFromNames("agent", "researcher")}
	if got, ok := Suggest(ctx, Options{}); ok {
		t.Fatalf("Suggest returned %#v, true; want no suggestion for empty prefix", got)
	}

	ctx.Cursor = len(ctx.Input) + 1
	if got, ok := Suggest(ctx, Options{}); ok {
		t.Fatalf("Suggest returned %#v, true; want no suggestion for invalid cursor", got)
	}
}

func hasRankSignal(suggestion Suggestion, name string) bool {
	for _, signal := range suggestion.RankSignals {
		if signal.Name == name {
			return true
		}
	}

	return false
}
