package vector

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkSearchScale(b *testing.B) {
	const (
		dimensions = 128
		limit      = 8
	)

	for _, docCount := range []int{64, 65, 512, 4096} {
		docs := benchmarkDocuments(docCount, dimensions)
		query := docs[docCount/2].Vector

		store, err := NewStore(dimensions)
		if err != nil {
			b.Fatalf("NewStore(%d): %v", dimensions, err)
		}

		for i := range docs {
			if addErr := store.Add(docs[i]); addErr != nil {
				b.Fatalf("Store.Add(%s): %v", docs[i].ID, addErr)
			}
		}

		ann, err := NewANNIndex(docs, dimensions, ANNOptions{})
		if err != nil {
			b.Fatalf("NewANNIndex(%d): %v", docCount, err)
		}

		b.Run(fmt.Sprintf("bruteforce/docs=%d", docCount), func(b *testing.B) {
			b.ReportMetric(float64(docCount), "docs")
			b.ReportMetric(float64(DefaultANNExactSearchMaxDocuments), "exact_threshold")

			for b.Loop() {
				if _, err := store.Search(query, limit); err != nil {
					b.Fatalf("Store.Search: %v", err)
				}
			}
		})

		mode := "ann-approx"
		if (ANNOptions{}).UsesExactSearch(docCount, limit) {
			mode = "ann-exact"
		}

		b.Run(fmt.Sprintf("%s/docs=%d", mode, docCount), func(b *testing.B) {
			options := (ANNOptions{}).Normalize(docCount, limit)
			b.ReportMetric(float64(docCount), "docs")
			b.ReportMetric(float64(options.MinCandidates), "ann_candidates")
			b.ReportMetric(float64(DefaultANNExactSearchMaxDocuments), "exact_threshold")
			b.ReportMetric(float64(boolMetric((ANNOptions{}).UsesExactSearch(docCount, limit))), "exact_scan")

			for b.Loop() {
				if _, err := ann.Search(query, limit); err != nil {
					b.Fatalf("ANNIndex.Search: %v", err)
				}
			}
		})
	}
}

func BenchmarkIndexANNLifecycle(b *testing.B) {
	const (
		dimensions = 128
		docCount   = 4096
		limit      = 8
	)

	docs := benchmarkDocuments(docCount, dimensions)
	query := docs[docCount/2].Vector
	idx := benchmarkIndex(docs, dimensions)

	ann, err := NewANNIndex(docs, dimensions, ANNOptions{})
	if err != nil {
		b.Fatalf("NewANNIndex(%d): %v", docCount, err)
	}

	b.Run("transient-ann/docs=4096", func(b *testing.B) {
		b.ReportMetric(float64(docCount), "docs")
		b.ReportMetric(0, "prebuilt_ann")

		for b.Loop() {
			if _, err := idx.SearchANN(query, limit, ANNOptions{}); err != nil {
				b.Fatalf("Index.SearchANN: %v", err)
			}
		}
	})

	b.Run("prebuilt-ann/docs=4096", func(b *testing.B) {
		b.ReportMetric(float64(docCount), "docs")
		b.ReportMetric(1, "prebuilt_ann")

		for b.Loop() {
			if _, err := ann.Search(query, limit); err != nil {
				b.Fatalf("ANNIndex.Search: %v", err)
			}
		}
	})
}

func boolMetric(value bool) int {
	if value {
		return 1
	}

	return 0
}

func benchmarkDocuments(count, dimensions int) []Document {
	docs := make([]Document, 0, count)
	for i := range count {
		vec := make(Vector, dimensions)
		vec[i%dimensions] = 1
		vec[(i*17+3)%dimensions] += 0.5
		vec[(i*31+7)%dimensions] += 0.25

		text := fmt.Sprintf("benchmark local rag document %04d", i)
		docs = append(docs, Document{
			ID:         fmt.Sprintf("doc-%04d", i),
			Text:       text,
			SourceHash: sourceHash(text),
			Vector:     vec,
			Provenance: ensureProvenance(map[string]string{provenanceSourceTypeKey: "benchmark"}, "benchmark"),
		})
	}

	return docs
}

func benchmarkIndex(docs []Document, dimensions int) *Index {
	timestamp := time.Unix(1, 0).UTC()

	return &Index{
		Version:   IndexVersion,
		CreatedAt: timestamp,
		UpdatedAt: timestamp,
		Vectorizer: VectorizerMetadata{
			Kind:       VectorizerKindEmbedding,
			Provider:   "benchmark",
			Model:      "synthetic",
			Dimensions: dimensions,
		},
		Chunk:      ChunkOptions{MaxRunes: 1024, OverlapRunes: 128},
		Dimensions: dimensions,
		Documents:  cloneDocuments(docs),
	}
}
