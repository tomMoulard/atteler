package vector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_SearchCosineRanking(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	for _, doc := range []Document{
		{ID: "mixed", Text: "partly related", Vector: Vector{0.5, 0.5}},
		{ID: "exact", Text: "directly related", Vector: Vector{1, 0}},
		{ID: "orthogonal", Text: "unrelated", Vector: Vector{0, 1}},
	} {
		addErr := store.Add(doc)
		if addErr != nil {
			t.Fatalf("Add(%q) error = %v", doc.ID, addErr)
		}
	}

	results, err := store.Search(Vector{1, 0}, 0)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	got := resultIDs(results)

	want := []string{"exact", "mixed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Search() IDs = %v, want %v", got, want)
	}

	if results[0].Score <= results[1].Score {
		t.Fatalf("Search() scores = %v, want descending cosine ranking", scores(results))
	}
}

func TestStore_SearchTieBreaksByID(t *testing.T) {
	t.Parallel()

	store, err := NewStore(2)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	for _, doc := range []Document{
		{ID: "b", Vector: Vector{1, 0}},
		{ID: "a", Vector: Vector{1, 0}},
	} {
		addErr := store.Add(doc)
		if addErr != nil {
			t.Fatalf("Add(%q) error = %v", doc.ID, addErr)
		}
	}

	results, err := store.Search(Vector{1, 0}, 0)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	got := resultIDs(results)

	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Search() IDs = %v, want %v", got, want)
	}
}

func TestStore_DimensionErrors(t *testing.T) {
	t.Parallel()

	store, err := NewStore(3)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	addErr := store.Add(Document{ID: "bad", Vector: Vector{1, 0}})
	if !errors.Is(addErr, ErrDimensionMismatch) {
		t.Fatalf("Add(dimension mismatch) error = %v, want ErrDimensionMismatch", addErr)
	}

	_, searchErr := store.Search(Vector{1, 0}, 0)
	if !errors.Is(searchErr, ErrDimensionMismatch) {
		t.Fatalf("Search(dimension mismatch) error = %v, want ErrDimensionMismatch", searchErr)
	}

	adoptingStore, err := NewStore(0)
	if err != nil {
		t.Fatalf("NewStore(0) error = %v", err)
	}

	addErr = adoptingStore.Add(Document{ID: "ok", Vector: Vector{1, 0}})
	if addErr != nil {
		t.Fatalf("Add(first vector) error = %v", addErr)
	}

	if adoptingStore.Dimensions != 2 {
		t.Fatalf("Dimensions = %d, want adopted dimension 2", adoptingStore.Dimensions)
	}

	addErr = adoptingStore.Add(Document{ID: "bad", Vector: Vector{1, 0, 0}})
	if !errors.Is(addErr, ErrDimensionMismatch) {
		t.Fatalf("Add(second dimension mismatch) error = %v, want ErrDimensionMismatch", addErr)
	}

	emptyStore, err := NewStore(0)
	if err != nil {
		t.Fatalf("NewStore(0) error = %v", err)
	}

	_, searchErr = emptyStore.Search(Vector{1, 0}, 0)
	if searchErr != nil {
		t.Fatalf("Search(empty adopting store) error = %v", searchErr)
	}

	if emptyStore.Dimensions != 0 {
		t.Fatalf("Dimensions after empty Search = %d, want 0", emptyStore.Dimensions)
	}
}

func TestTextVectorizer_Stability(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(16)
	if err != nil {
		t.Fatalf("NewTextVectorizer() error = %v", err)
	}

	first, err := vectorizer.Vectorize("ADR: Prefer local retrieval over remote indexing.")
	if err != nil {
		t.Fatalf("Vectorize(first) error = %v", err)
	}

	second, err := vectorizer.Vectorize("adr prefer local retrieval over remote indexing")
	if err != nil {
		t.Fatalf("Vectorize(second) error = %v", err)
	}

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Vectorize() = %v and %v, want stable lexical vector", first, second)
	}

	changed, err := vectorizer.Vectorize("ADR: Prefer encrypted archival notes.")
	if err != nil {
		t.Fatalf("Vectorize(changed) error = %v", err)
	}

	if reflect.DeepEqual(first, changed) {
		t.Fatalf("Vectorize(changed) = %v, want different vector for different text", changed)
	}
}

func resultIDs(results []Result) []string {
	ids := make([]string, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.Document.ID)
	}

	return ids
}

func scores(results []Result) []float64 {
	scores := make([]float64, 0, len(results))
	for _, result := range results {
		scores = append(scores, result.Score)
	}

	return scores
}

// ---------------------------------------------------------------------------
// Vectorizer interface conformance
// ---------------------------------------------------------------------------

func TestTextVectorizer_ImplementsVectorizer(t *testing.T) {
	t.Parallel()

	tv, err := NewTextVectorizer(0)
	require.NoError(t, err)

	var _ Vectorizer = tv
}

func TestEmbeddingVectorizer_ImplementsVectorizer(t *testing.T) {
	t.Parallel()

	var _ Vectorizer = NewEmbeddingVectorizer()

	var _ VectorizerContext = NewEmbeddingVectorizer()
}

// ---------------------------------------------------------------------------
// EmbeddingVectorizer tests
// ---------------------------------------------------------------------------

func TestEmbeddingVectorizer_CallsAPI(t *testing.T) {
	t.Parallel()

	embedding := []float64{0.1, 0.2, 0.3, 0.4}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/embed", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var req ollamaEmbedRequest

		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "nomic-embed-text", req.Model)
		assert.Equal(t, "hello world", req.Input)

		w.Header().Set("Content-Type", "application/json")

		resp := ollamaEmbedResponse{Embeddings: [][]float64{embedding}}

		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer server.Close()

	v := NewEmbeddingVectorizer(
		WithEmbeddingBaseURL(server.URL),
	)

	vec, err := v.VectorizeContext(context.Background(), "hello world")
	require.NoError(t, err)
	assert.Equal(t, Vector(embedding), vec)
}

func TestEmbeddingVectorizer_EmptyTextReturnsError(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer()

	_, err := v.VectorizeContext(context.Background(), "  ")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmptyText)
}

func TestEmbeddingVectorizer_VectorizeRequiresContext(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer()

	_, err := v.Vectorize("hello world")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)

	_, err = v.VectorizeContext(nil, "hello world") //nolint:staticcheck // Verify nil contexts are rejected instead of panicking.
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextRequired)
}

func TestEmbeddingVectorizer_VectorizeContextHonorsCancellation(t *testing.T) {
	t.Parallel()

	requestStarted := make(chan struct{})
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)

		select {
		case <-r.Context().Done():
			return
		case <-release:
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: [][]float64{{1.0}}}))
		}
	}))
	defer server.Close()
	defer close(release)

	v := NewEmbeddingVectorizer(WithEmbeddingBaseURL(server.URL))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		_, err := v.VectorizeContext(ctx, "hello world")
		errCh <- err
	}()

	<-requestStarted
	cancel()

	err := <-errCh
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestEmbeddingVectorizer_VectorizeContextRejectsAlreadyCanceledContext(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := v.VectorizeContext(ctx, "  ")
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrEmptyText)
}

func TestEmbeddingVectorizer_CustomModel(t *testing.T) {
	t.Parallel()

	var receivedModel string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaEmbedRequest

		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		receivedModel = req.Model

		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: [][]float64{{1.0}}}))
	}))
	defer server.Close()

	v := NewEmbeddingVectorizer(
		WithEmbeddingBaseURL(server.URL),
		WithEmbeddingModel("mxbai-embed-large"),
	)

	_, err := v.VectorizeContext(context.Background(), "test")
	require.NoError(t, err)
	assert.Equal(t, "mxbai-embed-large", receivedModel)
}

func TestEmbeddingVectorizer_ServerError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	v := NewEmbeddingVectorizer(WithEmbeddingBaseURL(server.URL))

	_, err := v.VectorizeContext(context.Background(), "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status 500")
}

func TestEmbeddingVectorizer_EmptyEmbeddingsResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: [][]float64{}}))
	}))
	defer server.Close()

	v := NewEmbeddingVectorizer(WithEmbeddingBaseURL(server.URL))

	_, err := v.VectorizeContext(context.Background(), "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty response")
}

func TestEmbeddingVectorizer_WithCustomHTTPClient(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(ollamaEmbedResponse{Embeddings: [][]float64{{0.5, 0.5}}}))
	}))
	defer server.Close()

	customClient := &http.Client{}
	v := NewEmbeddingVectorizer(
		WithEmbeddingBaseURL(server.URL),
		WithEmbeddingHTTPClient(customClient),
	)

	vec, err := v.VectorizeContext(context.Background(), "test")
	require.NoError(t, err)
	assert.Equal(t, Vector{0.5, 0.5}, vec)
}

func TestNewEmbeddingVectorizer_Defaults(t *testing.T) {
	t.Parallel()

	v := NewEmbeddingVectorizer()
	assert.Equal(t, defaultEmbeddingBaseURL, v.baseURL)
	assert.Equal(t, defaultEmbeddingModel, v.model)
	assert.NotNil(t, v.client)
}
