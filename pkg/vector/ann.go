package vector

import (
	"fmt"
	"math/bits"
	"sort"
)

const (
	defaultANNBucketBits          = 12
	defaultANNProbes              = 16
	defaultANNMinCandidates       = 64
	defaultANNCandidateMultiplier = 8

	// DefaultANNExactSearchMaxDocuments is the default index size at or below
	// which ANNOptions intentionally falls back to exact brute-force ranking.
	// Above this size, limited searches use ANN candidates unless callers raise
	// MinCandidates.
	DefaultANNExactSearchMaxDocuments = defaultANNMinCandidates
)

// ANNOptions controls the local approximate-nearest-neighbor search layer.
//
// The implementation uses stable random-projection LSH buckets to choose a
// candidate set, then applies exact cosine ranking inside that candidate set.
// That keeps persistence dependency-free while avoiding an unconditional
// full-index scan for larger workspaces.
type ANNOptions struct {
	BucketBits          int
	Probes              int
	MinCandidates       int
	CandidateMultiplier int
}

// Normalize fills conservative defaults for an ANN search over documentCount
// vectors. Small indexes intentionally search all documents because the exact
// path is faster and avoids approximate-result surprises.
func (o ANNOptions) Normalize(documentCount, limit int) ANNOptions {
	if o.BucketBits <= 0 {
		o.BucketBits = defaultANNBucketBits
	}

	if o.BucketBits > 63 {
		o.BucketBits = 63
	}

	if o.Probes <= 0 {
		o.Probes = defaultANNProbes
	}

	if o.MinCandidates <= 0 {
		o.MinCandidates = defaultANNMinCandidates
	}

	if o.CandidateMultiplier <= 0 {
		o.CandidateMultiplier = defaultANNCandidateMultiplier
	}

	if documentCount <= o.MinCandidates {
		o.MinCandidates = documentCount
	}

	if limit > 0 {
		target := annCandidateTarget(documentCount, limit, o.CandidateMultiplier)
		if target > o.MinCandidates {
			o.MinCandidates = target
		}
	}

	if o.MinCandidates > documentCount {
		o.MinCandidates = documentCount
	}

	return o
}

// UsesExactSearch reports whether the normalized options search every
// document. Exact search is intentional for small indexes and unbounded result
// sets; larger limited searches use ANN candidates.
func (o ANNOptions) UsesExactSearch(documentCount, limit int) bool {
	options := o.Normalize(documentCount, limit)

	return annUsesExactSearch(options, documentCount, limit)
}

func annUsesExactSearch(options ANNOptions, documentCount, limit int) bool {
	return documentCount <= 0 || limit <= 0 || options.MinCandidates >= documentCount
}

func annCandidateTarget(documentCount, limit, multiplier int) int {
	if documentCount <= 0 || limit <= 0 {
		return 0
	}

	if multiplier <= 1 {
		return min(limit, documentCount)
	}

	if limit > documentCount/multiplier {
		return documentCount
	}

	return limit * multiplier
}

// ANNIndex is an in-memory local ANN structure built from persisted vector
// documents. It is cheap to rebuild from Index.Documents when loading a
// workspace index and does not require an external service or dependency.
//
//nolint:govet // Keep derived ANN fields grouped by search behavior.
type ANNIndex struct {
	documents  []Document
	buckets    map[uint64][]int
	options    ANNOptions
	dimensions int
}

type annBucket struct {
	signature uint64
	distance  int
}

// NewANNIndex builds a local ANN index over docs.
func NewANNIndex(docs []Document, dimensions int, options ANNOptions) (*ANNIndex, error) {
	if dimensions <= 0 {
		return nil, ErrInvalidDimensions
	}

	options = options.Normalize(len(docs), 0)
	ann := &ANNIndex{
		documents:  cloneDocuments(docs),
		dimensions: dimensions,
		options:    options,
		buckets:    make(map[uint64][]int, len(docs)),
	}

	for i := range ann.documents {
		doc := ann.documents[i]
		if err := validateANNDocument(doc, dimensions); err != nil {
			return nil, fmt.Errorf("validate ANN document %q: %w", doc.ID, err)
		}

		signature := annSignature(doc.Vector, options.BucketBits)
		ann.buckets[signature] = append(ann.buckets[signature], i)
	}

	return ann, nil
}

func validateANNDocument(doc Document, dimensions int) error {
	if len(doc.Vector) != dimensions {
		return fmt.Errorf("%w: document has %d, want %d", ErrDimensionMismatch, len(doc.Vector), dimensions)
	}

	if err := validateVector(doc.Vector); err != nil {
		return err
	}

	if err := checkDocumentPrivacy(doc); err != nil {
		return err
	}

	if err := checkDocumentSourceHash(doc); err != nil {
		return err
	}

	if err := checkDocumentProvenance(doc); err != nil {
		return err
	}

	if err := checkDocumentTextHashVector(doc); err != nil {
		return err
	}

	return nil
}

// Search returns cosine-ranked results from an approximate candidate set. A
// limit less than one returns every non-zero candidate result.
func (ann *ANNIndex) Search(query Vector, limit int) ([]Result, error) {
	if ann == nil {
		return nil, nil
	}

	if err := validateVector(query); err != nil {
		return nil, err
	}

	if len(query) != ann.dimensions {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrDimensionMismatch, len(query), ann.dimensions)
	}

	candidates := ann.candidateIndexes(query, limit)
	results := make([]Result, 0, len(candidates))

	for _, index := range candidates {
		doc := ann.documents[index]

		score, err := CosineSimilarity(query, doc.Vector)
		if err != nil {
			return nil, err
		}

		if score == 0 {
			continue
		}

		results = append(results, Result{
			Document: cloneDocument(doc),
			Score:    score,
		})
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}

		return results[i].Document.ID < results[j].Document.ID
	})

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

func (ann *ANNIndex) candidateIndexes(query Vector, limit int) []int {
	docCount := len(ann.documents)
	if docCount == 0 {
		return nil
	}

	if limit <= 0 {
		return allDocumentIndexes(docCount)
	}

	options := ann.options.Normalize(docCount, limit)
	if annUsesExactSearch(options, docCount, limit) {
		return allDocumentIndexes(docCount)
	}

	querySignature := annSignature(query, options.BucketBits)

	buckets := make([]annBucket, 0, len(ann.buckets))
	for signature := range ann.buckets {
		buckets = append(buckets, annBucket{
			signature: signature,
			distance:  bits.OnesCount64((querySignature ^ signature) & annMask(options.BucketBits)),
		})
	}

	sort.SliceStable(buckets, func(i, j int) bool {
		if buckets[i].distance != buckets[j].distance {
			return buckets[i].distance < buckets[j].distance
		}

		return buckets[i].signature < buckets[j].signature
	})

	seen := make(map[int]struct{}, options.MinCandidates)
	candidates := make([]int, 0, options.MinCandidates)

	for i := range buckets {
		for _, docIndex := range ann.buckets[buckets[i].signature] {
			if _, ok := seen[docIndex]; ok {
				continue
			}

			seen[docIndex] = struct{}{}
			candidates = append(candidates, docIndex)
		}

		if i+1 >= options.Probes && len(candidates) >= options.MinCandidates {
			break
		}
	}

	if len(candidates) == 0 {
		return allDocumentIndexes(docCount)
	}

	sort.Ints(candidates)

	return candidates
}

// SearchANN builds a transient ANN layer over idx and searches it. The
// persisted Index remains the datastore of record; the ANN buckets are derived
// and intentionally cheap to rebuild on load.
func (idx *Index) SearchANN(query Vector, limit int, options ANNOptions) ([]Result, error) {
	if err := idx.Validate(); err != nil {
		return nil, err
	}

	ann, err := NewANNIndex(idx.Documents, idx.Dimensions, options)
	if err != nil {
		return nil, err
	}

	return ann.Search(query, limit)
}

func allDocumentIndexes(count int) []int {
	out := make([]int, count)
	for i := range out {
		out[i] = i
	}

	return out
}

func annSignature(vec Vector, bitCount int) uint64 {
	if bitCount <= 0 {
		bitCount = defaultANNBucketBits
	}

	if bitCount > 63 {
		bitCount = 63
	}

	var signature uint64

	for bit := range bitCount {
		var projection float64
		for dim, value := range vec {
			projection += value * annProjectionSign(bit, dim)
		}

		if projection >= 0 {
			signature |= 1 << bit
		}
	}

	return signature
}

func annProjectionSign(bit, dim int) float64 {
	//nolint:gosec // bit and dim are bounded internal projection indexes, not untrusted sizes.
	x := uint64(bit+1)*0x9e3779b97f4a7c15 ^ uint64(dim+1)*0xbf58476d1ce4e5b9
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31

	if x&1 == 0 {
		return -1
	}

	return 1
}

func annMask(bitCount int) uint64 {
	if bitCount >= 64 {
		return ^uint64(0)
	}

	return (uint64(1) << bitCount) - 1
}
