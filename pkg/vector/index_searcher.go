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

	queryVector, err := vectorizeContext(ctx, s.Vectorizer, privacy.RedactText(query.Text))
	if err != nil {
		return nil, fmt.Errorf("vector index retrieval: vectorize query: %w", err)
	}

	results, err := s.Index.SearchANN(queryVector, query.Limit, s.ANN)
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

func validateIndexSearcherVectorizer(idx *Index, vectorizer Vectorizer) error {
	metadata := normalizeVectorizerMetadataForIndex(vectorizer, VectorizerMetadata{})
	if metadata.Kind == "" {
		return fmt.Errorf("%w: vectorizer metadata is required for %s index retrieval", ErrMetadataMismatch, idx.Vectorizer.Label())
	}

	return idx.Vectorizer.CompatibleWith(metadata)
}
