package vector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/retrieval"
)

func TestIndexSearcher_SearchRetrievalRedactsQueryBeforeVectorizing(t *testing.T) {
	t.Parallel()

	vectorizer, err := NewTextVectorizer(16)
	require.NoError(t, err)

	idx, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/auth.md", Text: "OAuth callback state validation"}},
		vectorizer,
		vectorizer.Metadata(),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)

	queryVectorizer := &recordingTextVectorizer{inner: vectorizer}
	rawQuery := "OAuth callback api_key=super-secret-token"

	_, err = IndexSearcher{Index: idx, Vectorizer: queryVectorizer}.SearchRetrieval(
		context.TODO(),
		retrieval.Query{Text: rawQuery, IncludeUnsafe: true},
	)
	require.NoError(t, err)
	require.Len(t, queryVectorizer.texts, 1)
	assert.Equal(t, []string{"OAuth callback api_key=[REDACTED]"}, queryVectorizer.texts)
	assert.NotContains(t, queryVectorizer.texts[0], "super-secret-token")
}

func TestIndexSearcher_SearchRetrievalValidatesIndexBeforeVectorizingQuery(t *testing.T) {
	t.Parallel()

	queryVectorizer := &recordingVectorizer{vector: Vector{1, 0}}
	searcher := IndexSearcher{
		Index: &Index{
			Version:    0,
			Dimensions: 2,
			Vectorizer: NewLexicalMetadata(2),
		},
		Vectorizer: queryVectorizer,
	}

	_, err := searcher.SearchRetrieval(context.TODO(), retrieval.Query{Text: "private workspace query"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate index")
	assert.Empty(t, queryVectorizer.texts)
}

func TestIndexSearcher_SearchRetrievalRejectsVectorizerMetadataMismatchBeforeVectorizingQuery(t *testing.T) {
	t.Parallel()

	indexVectorizer := &metadataRecordingVectorizer{
		recordingVectorizer: &recordingVectorizer{vector: Vector{1, 0}},
		metadata:            NewEmbeddingMetadata("test-provider", "index-embed", "http://127.0.0.1:11434", 0),
		spec: VectorizerSpec{
			ID:            "test-embedding",
			Model:         "index-embed",
			Normalization: "test-v1",
			Version:       vectorizerSpecVersion,
		},
	}

	idx, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/auth.md", Text: "OAuth callback state validation"}},
		indexVectorizer,
		VectorizerMetadata{},
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)

	queryVectorizer := &metadataRecordingVectorizer{
		recordingVectorizer: &recordingVectorizer{vector: Vector{1, 0}},
		metadata:            NewEmbeddingMetadata("test-provider", "query-embed", "http://127.0.0.1:11434", 0),
		spec: VectorizerSpec{
			ID:            "test-embedding",
			Model:         "query-embed",
			Normalization: "test-v1",
			Version:       vectorizerSpecVersion,
		},
	}

	_, err = IndexSearcher{Index: idx, Vectorizer: queryVectorizer}.SearchRetrieval(
		context.TODO(),
		retrieval.Query{Text: "OAuth state"},
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrMetadataMismatch)
	assert.Contains(t, err.Error(), "vectorizer metadata")
	assert.Empty(t, queryVectorizer.texts)
}

type recordingTextVectorizer struct {
	inner *TextVectorizer
	texts []string
}

func (v *recordingTextVectorizer) Vectorize(text string) (Vector, error) {
	v.texts = append(v.texts, text)

	return v.inner.Vectorize(text)
}

func (v *recordingTextVectorizer) Metadata() VectorizerMetadata {
	return v.inner.Metadata()
}

func (v *recordingTextVectorizer) Spec() VectorizerSpec {
	return v.inner.Spec()
}
