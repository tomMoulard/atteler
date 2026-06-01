package vector

import (
	"fmt"
	"testing"
)

func BenchmarkSearchScale(b *testing.B) {
	const (
		dimensions = 128
		limit      = 8
	)

	for _, docCount := range []int{64, 512, 4096} {
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

		b.Run(fmt.Sprintf("ann/docs=%d", docCount), func(b *testing.B) {
			options := (ANNOptions{}).Normalize(docCount, limit)
			b.ReportMetric(float64(docCount), "docs")
			b.ReportMetric(float64(options.MinCandidates), "ann_candidates")
			b.ReportMetric(float64(DefaultANNExactSearchMaxDocuments), "exact_threshold")

			for b.Loop() {
				if _, err := ann.Search(query, limit); err != nil {
					b.Fatalf("ANNIndex.Search: %v", err)
				}
			}
		})
	}
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
