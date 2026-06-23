package sdk_test

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/review"
	"github.com/tommoulard/atteler/pkg/sdk"
	"github.com/tommoulard/atteler/pkg/session"
)

//nolint:govet // Test fake prioritizes readable provider fields over packing.
type fakeProvider struct {
	models  []string
	name    string
	content string
}

func (p fakeProvider) Name() string {
	return p.name
}

func (p fakeProvider) Models() []string {
	return append([]string(nil), p.models...)
}

func (p fakeProvider) FetchModels(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return p.Models(), nil
}

func (p fakeProvider) HealthCheck(ctx context.Context) error {
	return ctx.Err()
}

func (p fakeProvider) Complete(ctx context.Context, params llm.CompleteParams) (*llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	content := p.content
	if content == "" {
		content = "ok"
	}

	for i := len(params.Messages) - 1; i >= 0; i-- {
		if params.Messages[i].Role == llm.RoleUser {
			content = "echo: " + params.Messages[i].Content
			break
		}
	}

	return &llm.Response{
		Content: content,
		Model:   params.Model,
	}, nil
}

func (p fakeProvider) ModelContextWindow(model string) int {
	if slices.Contains(p.models, model) {
		return 8192
	}

	return 0
}

func TestRunOneShotChat_PersistsSession(t *testing.T) {
	t.Parallel()

	registry, err := sdk.NewProviderRegistry(fakeProvider{name: "fake", models: []string{"fake-model"}})
	require.NoError(t, err)

	store := session.NewStore(t.TempDir())
	result, err := sdk.RunOneShotChat(t.Context(), sdk.OneShotChatOptions{
		Registry:    registry,
		Store:       store,
		Model:       "fake-model",
		Prompt:      "hello",
		SaveSession: true,
	})
	require.NoError(t, err)

	assert.Equal(t, "echo: hello", result.Response.Content)
	assert.NotEmpty(t, result.SessionPath)

	saved, err := store.Load(result.Session.ID)
	require.NoError(t, err)
	require.Len(t, saved.Messages, 2)
	assert.Equal(t, llm.RoleUser, saved.Messages[0].Role)
	assert.Equal(t, "hello", saved.Messages[0].Content)
	assert.Equal(t, llm.RoleAssistant, saved.Messages[1].Role)
	assert.Equal(t, "echo: hello", saved.Messages[1].Content)
}

func TestRunOneShotChat_PreservesUnsavedSessionHistory(t *testing.T) {
	t.Parallel()

	registry, err := sdk.NewProviderRegistry(fakeProvider{name: "fake", models: []string{"fake-model"}})
	require.NoError(t, err)

	messages := make([]llm.Message, 1, 3)
	messages[0] = llm.Message{Role: llm.RoleUser, Content: "prior"}

	input := session.Session{
		DefaultModel: "fake-model",
		Title:        "imported transcript",
		Messages:     messages,
	}

	result, err := sdk.RunOneShotChat(t.Context(), sdk.OneShotChatOptions{
		Registry: registry,
		Session:  input,
		Prompt:   "next",
	})
	require.NoError(t, err)

	require.NotEmpty(t, result.Session.ID)
	assert.Equal(t, "imported transcript", result.Session.Title)
	require.Len(t, result.Session.Messages, 3)
	assert.Equal(t, "prior", result.Session.Messages[0].Content)
	assert.Equal(t, "next", result.Session.Messages[1].Content)
	assert.Equal(t, "echo: next", result.Session.Messages[2].Content)

	callerBacking := input.Messages[:cap(input.Messages)]
	assert.Len(t, input.Messages, 1)
	assert.Empty(t, callerBacking[1].Content, "caller-owned session history should not be appended in place")
	assert.Empty(t, callerBacking[2].Content, "caller-owned session history should not be appended in place")
}

func TestNewProviderRegistry_RejectsNilProvider(t *testing.T) {
	t.Parallel()

	registry, err := sdk.NewProviderRegistry(nil)

	require.Error(t, err)
	assert.Nil(t, registry)
	assert.Contains(t, err.Error(), "provider 0 is nil")
}

func TestBuildMemoryIndex_SearchesDocuments(t *testing.T) {
	t.Parallel()

	store, err := sdk.BuildMemoryIndex(sdk.MemoryIndexOptions{
		Documents: []memory.Document{{
			ID:   "doc-1",
			Text: "Atteler sessions can be indexed for retrieval.",
		}},
	})
	require.NoError(t, err)

	results, err := sdk.SearchMemory(store, "sessions retrieval", 1)
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.Equal(t, "doc-1", results[0].Document.ID)
}

func TestNewReviewRun_UsesDefaultReviewContract(t *testing.T) {
	t.Parallel()

	run, err := sdk.NewReviewRun(sdk.ReviewRunOptions{})
	require.NoError(t, err)

	assert.Equal(t, []string{"."}, run.Plan.Paths())
	assert.Len(t, run.Plan.Reviewers(), 2)

	report := review.Report{
		Reviewer: "quality-reviewer",
		GateChecks: []review.GateCheck{
			{Name: "tests pass", Passed: true},
			{Name: "types pass", Passed: true},
			{Name: "lint pass", Passed: true},
			{Name: "no new flakes", Passed: true},
			{Name: "behavioral diff reviewed", Passed: true},
		},
	}

	require.NoError(t, run.ValidateReport(report))
}

func TestPackagesByStability_ReturnsCopy(t *testing.T) {
	t.Parallel()

	stable := sdk.PackagesByStability(sdk.StabilityStable)
	require.NotEmpty(t, stable)

	stable[0].ImportPath = "mutated"

	again := sdk.PackagesByStability(sdk.StabilityStable)
	assert.NotEqual(t, "mutated", again[0].ImportPath)
	assert.Contains(t, sdk.CompatibilityPolicy, "Stable SDK packages")
	assert.Contains(t, sdk.CompatibilityPolicy, "exported identifiers")
}
