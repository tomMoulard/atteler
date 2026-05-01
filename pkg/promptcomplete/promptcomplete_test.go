package promptcomplete

import (
	"reflect"
	"strings"
	"testing"
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
