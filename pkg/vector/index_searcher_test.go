package vector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/retrieval"
)

func TestIndexSearcher_RetrievalQualityFixture(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "retrieval_quality.json"))
	require.NoError(t, err)

	var fixture struct {
		Documents []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"documents"`
		Cases []struct {
			Query   string `json:"query"`
			WantTop string `json:"want_top"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(data, &fixture))

	vectorizer, err := NewTextVectorizer(128)
	require.NoError(t, err)

	sources := make([]Source, 0, len(fixture.Documents))
	for _, doc := range fixture.Documents {
		sources = append(sources, Source{
			Kind: SourceKindFile,
			Path: filepath.ToSlash(filepath.Join("fixture", doc.ID+".md")),
			Text: doc.Text,
		})
	}

	idx, err := BuildIndex(
		context.TODO(),
		sources,
		vectorizer,
		vectorizer.Metadata(),
		ChunkOptions{MaxRunes: 400, OverlapRunes: 40},
		time.Unix(1, 0),
	)
	require.NoError(t, err)

	ann, err := NewANNIndex(idx.Documents, idx.Dimensions, ANNOptions{})
	require.NoError(t, err)

	searcher := IndexSearcher{
		Index:      idx,
		Vectorizer: vectorizer,
		IndexANN:   ann,
		Source: retrieval.Source{
			Type: retrieval.SourceVector,
			Name: "quality-fixture",
			URI:  filepath.ToSlash(filepath.Join("testdata", "retrieval_quality.json")),
		},
		ScorerName: "lexical-fixture-ann",
	}

	for _, tc := range fixture.Cases {
		results, searchErr := retrieval.Search(context.TODO(), retrieval.Query{
			Text:          tc.Query,
			Limit:         1,
			IncludeUnsafe: true,
		}, searcher)
		require.NoError(t, searchErr, tc.Query)

		if assert.Len(t, results, 1, tc.Query) {
			wantID := filepath.ToSlash(filepath.Join("fixture", tc.WantTop+".md")) + "#chunk=0000"
			assert.Equal(t, wantID, results[0].DocumentID, tc.Query)
			assert.Equal(t, "lexical-fixture-ann", results[0].Scorer.Name, tc.Query)
		}
	}
}

func TestIndexSearcher_RejectsMismatchedPrebuiltANN(t *testing.T) {
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

	other, err := BuildIndex(
		context.TODO(),
		[]Source{{Path: "docs/shell.md", Text: "Shell command output capture"}},
		vectorizer,
		vectorizer.Metadata(),
		ChunkOptions{MaxRunes: 100},
		time.Unix(1, 0),
	)
	require.NoError(t, err)

	ann, err := NewANNIndex(other.Documents, other.Dimensions, ANNOptions{})
	require.NoError(t, err)

	_, err = IndexSearcher{Index: idx, Vectorizer: vectorizer, IndexANN: ann}.SearchRetrieval(
		context.TODO(),
		retrieval.Query{Text: "OAuth state", Limit: 1},
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSourceStale)
	assert.Contains(t, err.Error(), "ANN index")
}

func TestIndexSearcher_RejectsStalePrebuiltANNForSameDocumentID(t *testing.T) {
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

	ann, err := NewANNIndex(idx.Documents, idx.Dimensions, ANNOptions{})
	require.NoError(t, err)
	require.Len(t, ann.documents, 1)

	ann.documents[0].Vector[0]++

	_, err = IndexSearcher{Index: idx, Vectorizer: vectorizer, IndexANN: ann}.SearchRetrieval(
		context.TODO(),
		retrieval.Query{Text: "OAuth state", Limit: 1},
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrVectorMismatch)
	assert.Contains(t, err.Error(), "ANN index")
}

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
