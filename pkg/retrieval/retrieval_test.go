package retrieval_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/memory"
	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/vector"
)

type fixtureFile struct {
	Documents []fixtureDocument `json:"documents"`
	Queries   []fixtureQuery    `json:"queries"`
}

type fixtureDocument struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type fixtureQuery struct {
	Text    string `json:"text"`
	WantTop string `json:"want_top"`
}

func TestSearch_ComparesLexicalHashedAndEmbeddingBackedFixtures(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t)
	query := fixture.Queries[0]

	lexical := memory.NewStore()
	for _, doc := range fixture.Documents {
		require.NoError(t, lexical.AddText(doc.ID, doc.Text))
	}

	hashedStore, hashedVectorizer := buildVectorFixture(t, fixture.Documents, mustTextVectorizer(t, 32))
	embeddingStore, embeddingVectorizer := buildVectorFixture(t, fixture.Documents, fakeEmbeddingVectorizer{})

	results, err := retrieval.Search(context.Background(), retrieval.Query{
		Text:    query.Text,
		Limit:   9,
		Explain: true,
	}, lexical,
		vector.Searcher{Store: hashedStore, Vectorizer: hashedVectorizer, Source: retrieval.Source{Type: retrieval.SourceVector, Name: "hashed-fixture"}},
		vector.Searcher{Store: embeddingStore, Vectorizer: embeddingVectorizer, Source: retrieval.Source{Type: retrieval.SourceVector, Name: "embedding-fixture"}, ScorerName: "embedding-cosine"},
	)
	require.NoError(t, err)
	require.NotEmpty(t, results)

	bySource := make(map[string]retrieval.Result)

	for i := range results {
		result := results[i]
		if _, ok := bySource[result.Source.Name]; !ok {
			bySource[result.Source.Name] = result
		}

		assert.NotEmpty(t, result.DocumentID)
		assert.NotEmpty(t, result.Chunk.ID)
		assert.NotEmpty(t, result.Scorer.Name)
		assert.NotEmpty(t, result.Scorer.Explanation)
		assert.True(t, result.Score >= 0 && result.Score <= 1)
	}

	assert.Equal(t, query.WantTop, bySource["hashed-fixture"].DocumentID)
	assert.Equal(t, query.WantTop, bySource["embedding-fixture"].DocumentID)
	assert.Equal(t, query.WantTop, firstSourceResult(results, retrieval.SourceMemory).DocumentID)

	filtered, err := retrieval.Search(context.Background(), retrieval.Query{
		Text:    query.Text,
		Limit:   3,
		Sources: []retrieval.SourceType{retrieval.SourceVector},
	}, lexical,
		vector.Searcher{Store: hashedStore, Vectorizer: hashedVectorizer, Source: retrieval.Source{Type: retrieval.SourceVector, Name: "hashed-fixture"}},
	)
	require.NoError(t, err)
	require.NotEmpty(t, filtered)

	for i := range filtered {
		result := filtered[i]
		assert.Equal(t, retrieval.SourceVector, result.Source.Type)
	}
}

func TestSearch_DeduplicatesAndFiltersUnsafeResults(t *testing.T) {
	t.Parallel()

	unsafe := staticSearcher{results: []retrieval.Result{{
		Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "fixture"},
		DocumentID: "secret",
		Chunk:      retrieval.Chunk{ID: "secret#1"},
		Score:      0.9,
		Safety:     retrieval.Safety{InjectAllowed: false, Sensitive: true},
	}}}
	safeDuplicate := staticSearcher{results: []retrieval.Result{
		{
			Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "fixture"},
			DocumentID: "doc",
			Chunk:      retrieval.Chunk{ID: "doc#1"},
			Score:      0.4,
			Scorer:     retrieval.Scorer{Explanation: []string{"low score"}},
			Safety:     retrieval.Safety{InjectAllowed: true},
		},
		{
			Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "fixture"},
			DocumentID: "doc",
			Chunk:      retrieval.Chunk{ID: "doc#1"},
			Score:      0.8,
			Scorer:     retrieval.Scorer{Explanation: []string{"high score"}},
			Safety:     retrieval.Safety{InjectAllowed: true},
		},
	}}

	results, err := retrieval.Search(context.Background(), retrieval.Query{Text: "doc"}, unsafe, safeDuplicate)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "doc", results[0].DocumentID)
	assert.InDelta(t, 0.8, results[0].Score, 0.000001)
	assert.ElementsMatch(t, []string{"low score", "high score"}, results[0].Scorer.Explanation)
}

func TestSearch_MergesDuplicateSafetyBeforeFiltering(t *testing.T) {
	t.Parallel()

	results := []retrieval.Result{
		{
			Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "fixture"},
			DocumentID: "doc",
			Chunk:      retrieval.Chunk{ID: "doc#1"},
			Score:      0.9,
			Snippet:    "OAuth api_key=super-secret-token",
			Metadata:   map[string]string{"api_key": "super-secret-token"},
			Safety:     retrieval.Safety{InjectAllowed: true},
		},
		{
			Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "fixture"},
			DocumentID: "doc",
			Chunk:      retrieval.Chunk{ID: "doc#1"},
			Score:      0.2,
			Snippet:    "OAuth api_key=[REDACTED]",
			Metadata:   map[string]string{"api_key": "[REDACTED]"},
			Safety: retrieval.Safety{
				InjectAllowed: false,
				Redacted:      true,
				Sensitive:     true,
				Reasons:       []string{"credential-shaped assignment"},
			},
		},
	}

	filtered, err := retrieval.Search(context.Background(), retrieval.Query{Text: "oauth"}, staticSearcher{results: results})
	require.NoError(t, err)
	assert.Empty(t, filtered, "unsafe duplicate evidence should remove the merged hit before prompt injection")

	included, err := retrieval.Search(context.Background(), retrieval.Query{Text: "oauth", IncludeUnsafe: true}, staticSearcher{results: results})
	require.NoError(t, err)
	require.Len(t, included, 1)
	assert.InDelta(t, 0.9, included[0].Score, 0.000001)
	assert.False(t, included[0].Safety.InjectAllowed)
	assert.True(t, included[0].Safety.Redacted)
	assert.NotContains(t, included[0].Snippet, "super-secret-token")
	assert.Contains(t, included[0].Snippet, "[REDACTED]")
	assert.Equal(t, "[REDACTED]", included[0].Metadata["api_key"])
	assert.Contains(t, included[0].Safety.Reasons, "credential-shaped assignment")
}

func TestSearch_AppliesLimitAfterSafetyFiltering(t *testing.T) {
	t.Parallel()

	searcher := observingSearcher{results: []retrieval.Result{
		{
			Source:     retrieval.Source{Type: retrieval.SourceMemory},
			DocumentID: "unsafe-top",
			Chunk:      retrieval.Chunk{ID: "unsafe-top#1"},
			Score:      0.99,
			Safety:     retrieval.Safety{InjectAllowed: false, Sensitive: true},
		},
		{
			Source:     retrieval.Source{Type: retrieval.SourceMemory},
			DocumentID: "safe-second",
			Chunk:      retrieval.Chunk{ID: "safe-second#1"},
			Score:      0.5,
			Safety:     retrieval.Safety{InjectAllowed: true},
		},
	}}

	results, err := retrieval.Search(context.Background(), retrieval.Query{Text: "safe", Limit: 1}, &searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "safe-second", results[0].DocumentID)
	assert.Equal(t, 0, searcher.seenLimit, "backend limit should be disabled before safety filtering")
}

func TestSearch_AppliesLimitAfterSourceFiltering(t *testing.T) {
	t.Parallel()

	searcher := observingSearcher{results: []retrieval.Result{
		{
			Source:     retrieval.Source{Type: retrieval.SourceSession},
			DocumentID: "session-top",
			Chunk:      retrieval.Chunk{ID: "session-top#1"},
			Score:      0.99,
			Safety:     retrieval.Safety{InjectAllowed: true},
		},
		{
			Source:     retrieval.Source{Type: retrieval.SourceFile},
			DocumentID: "file-second",
			Chunk:      retrieval.Chunk{ID: "file-second#1"},
			Score:      0.5,
			Safety:     retrieval.Safety{InjectAllowed: true},
		},
	}}

	results, err := retrieval.Search(context.Background(), retrieval.Query{
		Text:    "file",
		Limit:   1,
		Sources: []retrieval.SourceType{retrieval.SourceFile},
	}, &searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "file-second", results[0].DocumentID)
	assert.Equal(t, 0, searcher.seenLimit, "backend limit should be disabled before source filtering")
}

func TestSearch_AppliesContractFiltersBeforeLimit(t *testing.T) {
	t.Parallel()

	searcher := observingSearcher{results: []retrieval.Result{
		{
			Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "notes"},
			DocumentID: "wrong-agent",
			Chunk:      retrieval.Chunk{ID: "wrong-agent#1"},
			Score:      0.99,
			Metadata:   map[string]string{"agent": "writer", "kind": "note"},
			Safety:     retrieval.Safety{InjectAllowed: true},
		},
		{
			Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "notes"},
			DocumentID: "right-agent",
			Chunk:      retrieval.Chunk{ID: "right-agent#1"},
			Score:      0.5,
			Metadata:   map[string]string{"agent": "reviewer", "kind": "note"},
			Safety:     retrieval.Safety{InjectAllowed: true},
		},
	}}

	results, err := retrieval.Search(context.Background(), retrieval.Query{
		Text:    "note",
		Limit:   1,
		Filters: map[string]string{"agent": "reviewer", "source.name": "notes"},
	}, &searcher)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "right-agent", results[0].DocumentID)
	assert.Equal(t, 0, searcher.seenLimit, "backend limit should be disabled before contract filtering")
}

func TestSearch_BackfillsStableMetadataBeforeFiltering(t *testing.T) {
	t.Parallel()

	source := retrieval.Source{Type: retrieval.SourceVector, Name: "fixture", URI: "file://doc"}
	stableID := retrieval.StableDocumentID(source, "doc")

	results, err := retrieval.Search(context.Background(), retrieval.Query{
		Text:    "oauth",
		Filters: map[string]string{retrieval.MetadataStableID: stableID},
	}, staticSearcher{results: []retrieval.Result{{
		Source:     source,
		DocumentID: "doc",
		Chunk:      retrieval.Chunk{ID: "doc#chunk"},
		Score:      0.5,
		Snippet:    "OAuth callback notes",
		Safety:     retrieval.Safety{InjectAllowed: true},
	}}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, stableID, results[0].Metadata[retrieval.MetadataStableID])
	assert.Equal(t, retrieval.TextHash("OAuth callback notes"), results[0].Chunk.ContentHash)
	assert.Equal(t, results[0].Chunk.ContentHash, results[0].Metadata[retrieval.MetadataContentHash])
}

func TestNormalizeResult_BackfillsStableMetadataAndClampsScore(t *testing.T) {
	t.Parallel()

	source := retrieval.Source{Type: retrieval.SourceVector, Name: "fixture", URI: "file://doc"}

	result := retrieval.NormalizeResult(retrieval.Result{
		Source:     source,
		DocumentID: "doc",
		Chunk:      retrieval.Chunk{ID: "doc#chunk"},
		Score:      2.5,
		Snippet:    "OAuth callback notes",
		Safety:     retrieval.Safety{InjectAllowed: true},
	})

	assert.InDelta(t, 1.0, result.Score, 0.000001)
	assert.Equal(t, retrieval.StableDocumentID(source, "doc"), result.Metadata[retrieval.MetadataStableID])
	assert.Equal(t, retrieval.TextHash("OAuth callback notes"), result.Metadata[retrieval.MetadataContentHash])
	assert.Equal(t, result.Metadata[retrieval.MetadataContentHash], result.Chunk.ContentHash)
}

func TestNormalizeResultDoesNotMutateInputMetadata(t *testing.T) {
	t.Parallel()

	metadata := map[string]string{"kind": "note"}
	result := retrieval.NormalizeResult(retrieval.Result{
		Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "notes"},
		DocumentID: "doc-1",
		Metadata:   metadata,
		Snippet:    "OAuth callback notes",
	})

	assert.Equal(t, retrieval.StableDocumentID(retrieval.Source{Type: retrieval.SourceMemory, Name: "notes"}, "doc-1"), result.Metadata[retrieval.MetadataStableID])
	assert.Equal(t, retrieval.TextHash("OAuth callback notes"), result.Metadata[retrieval.MetadataContentHash])
	assert.Equal(t, map[string]string{"kind": "note"}, metadata)
	assert.NotContains(t, metadata, retrieval.MetadataStableID)
	assert.NotContains(t, metadata, retrieval.MetadataContentHash)
}

func TestSearch_TreatsMissingSafetyAsDefaultInjectable(t *testing.T) {
	t.Parallel()

	results, err := retrieval.Search(context.Background(), retrieval.Query{Text: "doc"}, staticSearcher{results: []retrieval.Result{{
		Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "legacy"},
		DocumentID: "legacy-doc",
		Chunk:      retrieval.Chunk{ID: "legacy-doc#1"},
		Score:      0.7,
		Snippet:    "legacy public result without persisted safety metadata",
	}}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].Safety.InjectAllowed)
	assert.Equal(t, "legacy-doc", results[0].DocumentID)
}

func TestSearch_ReconstructsSafetyFromMetadataWhenSafetyOmitted(t *testing.T) {
	t.Parallel()

	filtered, err := retrieval.Search(context.Background(), retrieval.Query{Text: "secret"}, staticSearcher{results: []retrieval.Result{{
		Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "legacy"},
		DocumentID: "legacy-secret",
		Chunk:      retrieval.Chunk{ID: "legacy-secret#1"},
		Score:      0.7,
		Snippet:    "legacy secret result",
		Metadata: map[string]string{
			retrieval.MetadataSafetyInjectAllowed: "false",
			retrieval.MetadataSafetySensitive:     "true",
			retrieval.MetadataSafetyReasons:       "legacy policy",
		},
	}}})
	require.NoError(t, err)
	assert.Empty(t, filtered)

	included, err := retrieval.Search(context.Background(), retrieval.Query{Text: "secret", IncludeUnsafe: true}, staticSearcher{results: []retrieval.Result{{
		Source:     retrieval.Source{Type: retrieval.SourceMemory, Name: "legacy"},
		DocumentID: "legacy-secret",
		Chunk:      retrieval.Chunk{ID: "legacy-secret#1"},
		Score:      0.7,
		Snippet:    "legacy secret result",
		Metadata: map[string]string{
			retrieval.MetadataSafetyInjectAllowed: "false",
			retrieval.MetadataSafetySensitive:     "true",
			retrieval.MetadataSafetyReasons:       "legacy policy",
		},
	}}})
	require.NoError(t, err)
	require.Len(t, included, 1)
	assert.False(t, included[0].Safety.InjectAllowed)
	assert.True(t, included[0].Safety.Sensitive)
	assert.Contains(t, included[0].Safety.Reasons, "legacy policy")
}

func TestNormalizeSafety_PreservesExplicitUnsafeSignals(t *testing.T) {
	t.Parallel()

	safety := retrieval.NormalizeSafety(retrieval.Safety{
		InjectAllowed: false,
		Sensitive:     true,
		Reasons:       []string{"credential-adjacent path"},
	})

	assert.False(t, safety.InjectAllowed)
	assert.True(t, safety.Sensitive)
	assert.Contains(t, safety.Reasons, "credential-adjacent path")
}

func TestSearch_SkipsNilSearchers(t *testing.T) {
	t.Parallel()

	results, err := retrieval.Search(context.Background(), retrieval.Query{Text: "doc"}, nil, staticSearcher{results: []retrieval.Result{{
		Source:     retrieval.Source{Type: retrieval.SourceMemory},
		DocumentID: "doc",
		Chunk:      retrieval.Chunk{ID: "doc#1"},
		Score:      0.8,
		Safety:     retrieval.Safety{InjectAllowed: true},
	}}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "doc", results[0].DocumentID)
}

func TestSearch_HonorsCanceledContextBeforeBackends(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := retrieval.Search(ctx, retrieval.Query{Text: "doc"}, panicSearcher{})
	require.ErrorIs(t, err, context.Canceled)
}

func BenchmarkRetrievalFixture_LexicalMemory(b *testing.B) {
	fixture := loadFixture(b)

	store := memory.NewStore()
	for _, doc := range fixture.Documents {
		require.NoError(b, store.AddText(doc.ID, doc.Text))
	}

	query := retrieval.Query{Text: fixture.Queries[0].Text, Limit: 3}

	b.ResetTimer()

	for range b.N {
		_, err := store.SearchRetrieval(context.Background(), query)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRetrievalFixture_HashedVector(b *testing.B) {
	fixture := loadFixture(b)
	store, vectorizer := buildVectorFixture(b, fixture.Documents, mustTextVectorizer(b, 32))
	searcher := vector.Searcher{Store: store, Vectorizer: vectorizer, Source: retrieval.Source{Type: retrieval.SourceVector, Name: "hashed-fixture"}}
	query := retrieval.Query{Text: fixture.Queries[0].Text, Limit: 3}

	b.ResetTimer()

	for range b.N {
		_, err := searcher.SearchRetrieval(context.Background(), query)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRetrievalFixture_EmbeddingBacked(b *testing.B) {
	fixture := loadFixture(b)
	store, vectorizer := buildVectorFixture(b, fixture.Documents, fakeEmbeddingVectorizer{})
	searcher := vector.Searcher{Store: store, Vectorizer: vectorizer, Source: retrieval.Source{Type: retrieval.SourceVector, Name: "embedding-fixture"}, ScorerName: "embedding-cosine"}
	query := retrieval.Query{Text: fixture.Queries[0].Text, Limit: 3}

	b.ResetTimer()

	for range b.N {
		_, err := searcher.SearchRetrieval(context.Background(), query)
		if err != nil {
			b.Fatal(err)
		}
	}
}

type staticSearcher struct {
	results []retrieval.Result
}

func (s staticSearcher) SearchRetrieval(context.Context, retrieval.Query) ([]retrieval.Result, error) {
	return append([]retrieval.Result(nil), s.results...), nil
}

type observingSearcher struct {
	results   []retrieval.Result
	seenLimit int
}

func (s *observingSearcher) SearchRetrieval(_ context.Context, query retrieval.Query) ([]retrieval.Result, error) {
	s.seenLimit = query.Limit
	return append([]retrieval.Result(nil), s.results...), nil
}

type panicSearcher struct{}

func (panicSearcher) SearchRetrieval(context.Context, retrieval.Query) ([]retrieval.Result, error) {
	panic("searcher should not be called")
}

type fakeEmbeddingVectorizer struct{}

func (fakeEmbeddingVectorizer) Vectorize(text string) (vector.Vector, error) {
	tokens := memory.Tokenize(text)
	out := vector.Vector{0, 0, 0, 0}

	for _, token := range tokens {
		switch token {
		case "oauth", "callback", "tokens", "state":
			out[0]++
		case "release", "documentation", "migration":
			out[1]++
		case "agent", "memory", "retrieval":
			out[2]++
		default:
			out[3]++
		}
	}

	return out, nil
}

func buildVectorFixture(tb testing.TB, docs []fixtureDocument, vectorizer vector.Vectorizer) (*vector.Store, vector.Vectorizer) {
	tb.Helper()

	store, err := vector.NewStore(0)
	if specer, ok := vectorizer.(interface{ Spec() vector.VectorizerSpec }); ok {
		store, err = vector.NewStoreWithVectorizer(specer.Spec())
	}

	require.NoError(tb, err)

	for _, doc := range docs {
		vec, vectorErr := vectorizer.Vectorize(doc.Text)
		require.NoError(tb, vectorErr)
		require.NoError(tb, store.Add(vector.Document{ID: doc.ID, Text: doc.Text, Vector: vec}))
	}

	return store, vectorizer
}

func mustTextVectorizer(tb testing.TB, dimensions int) *vector.TextVectorizer {
	tb.Helper()

	vectorizer, err := vector.NewTextVectorizer(dimensions)
	require.NoError(tb, err)

	return vectorizer
}

func firstSourceResult(results []retrieval.Result, source retrieval.SourceType) retrieval.Result {
	for i := range results {
		result := results[i]
		if result.Source.Type == source {
			return result
		}
	}

	return retrieval.Result{}
}

func loadFixture(tb testing.TB) fixtureFile {
	tb.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "fixtures.json"))
	require.NoError(tb, err)

	var fixture fixtureFile
	require.NoError(tb, json.Unmarshal(data, &fixture))
	require.NotEmpty(tb, fixture.Documents)
	require.NotEmpty(tb, fixture.Queries)

	return fixture
}
