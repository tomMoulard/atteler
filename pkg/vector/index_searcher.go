package vector

import (
	"context"
	"fmt"

	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
)

// IndexSearcher adapts a persisted Index plus vectorizer to the shared
// retrieval contract. It searches through the local ANN layer instead of
// requiring callers to materialize a linear Store first.
type IndexSearcher struct {
	Index      *Index
	Vectorizer Vectorizer
	// IndexANN is an optional prebuilt ANN layer derived from Index.Documents.
	// Supplying it avoids rebuilding buckets for every retrieval call while
	// keeping Index as the persisted datastore of record.
	IndexANN   *ANNIndex
	Source     retrieval.Source
	ScorerName string
	ANN        ANNOptions
}

// SearchRetrieval vectorizes query text and returns ANN-ranked results using
// the shared retrieval contract.
func (s IndexSearcher) SearchRetrieval(ctx context.Context, query retrieval.Query) ([]retrieval.Result, error) {
	if ctx == nil {
		return nil, ErrContextRequired
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("vector index retrieval: %w", err)
	}

	if s.Index == nil {
		return nil, nil
	}

	if err := s.Index.Validate(); err != nil {
		return nil, fmt.Errorf("vector index retrieval: validate index: %w", err)
	}

	if s.Vectorizer == nil {
		if s.Index.Vectorizer.Normalize().Kind != VectorizerKindLexical {
			return nil, fmt.Errorf("%w: vectorizer is required for %s index retrieval", ErrMetadataMismatch, s.Index.Vectorizer.Label())
		}

		vectorizer, err := NewTextVectorizer(s.Index.Dimensions)
		if err != nil {
			return nil, fmt.Errorf("vector index retrieval: create vectorizer: %w", err)
		}

		s.Vectorizer = vectorizer
	}

	if err := validateIndexSearcherVectorizer(s.Index, s.Vectorizer); err != nil {
		return nil, fmt.Errorf("vector index retrieval: vectorizer metadata: %w", err)
	}

	if err := validateIndexSearcherANN(s.Index, s.IndexANN); err != nil {
		return nil, fmt.Errorf("vector index retrieval: ANN index: %w", err)
	}

	queryVector, err := vectorizeContext(ctx, s.Vectorizer, privacy.RedactText(query.Text))
	if err != nil {
		return nil, fmt.Errorf("vector index retrieval: vectorize query: %w", err)
	}

	results, err := s.searchANN(queryVector, query.Limit)
	if err != nil {
		return nil, fmt.Errorf("vector index retrieval: search ANN index: %w", err)
	}

	adapter := Searcher{
		Vectorizer: s.Vectorizer,
		Source:     s.Source,
		ScorerName: s.ScorerName,
	}

	out := make([]retrieval.Result, 0, len(results))
	for i := range results {
		out = append(out, adapter.retrievalResult(results[i], query))
	}

	return out, nil
}

func (s IndexSearcher) searchANN(queryVector Vector, limit int) ([]Result, error) {
	if s.IndexANN != nil {
		return s.IndexANN.Search(queryVector, limit)
	}

	return s.Index.SearchANN(queryVector, limit, s.ANN)
}

func validateIndexSearcherVectorizer(idx *Index, vectorizer Vectorizer) error {
	metadata := normalizeVectorizerMetadataForIndex(vectorizer, VectorizerMetadata{})
	if metadata.Kind == "" {
		return fmt.Errorf("%w: vectorizer metadata is required for %s index retrieval", ErrMetadataMismatch, idx.Vectorizer.Label())
	}

	return idx.Vectorizer.CompatibleWith(metadata)
}

func validateIndexSearcherANN(idx *Index, ann *ANNIndex) error {
	if ann == nil || idx == nil {
		return nil
	}

	if ann.dimensions != idx.Dimensions {
		return fmt.Errorf("%w: got %d, want %d", ErrDimensionMismatch, ann.dimensions, idx.Dimensions)
	}

	if len(ann.documents) != len(idx.Documents) {
		return fmt.Errorf("%w: ANN document count got %d, want %d", ErrSourceStale, len(ann.documents), len(idx.Documents))
	}

	for i := range idx.Documents {
		if ann.documents[i].ID != idx.Documents[i].ID {
			return fmt.Errorf("%w: ANN document %d got %q, want %q", ErrSourceStale, i, ann.documents[i].ID, idx.Documents[i].ID)
		}

		if ann.documents[i].SourceHash != idx.Documents[i].SourceHash {
			return fmt.Errorf("%w: ANN document %q source hash changed", ErrSourceStale, idx.Documents[i].ID)
		}

		if !vectorsEqual(ann.documents[i].Vector, idx.Documents[i].Vector) {
			return fmt.Errorf("%w: ANN document %q vector changed", ErrVectorMismatch, idx.Documents[i].ID)
		}
	}

	return nil
}
