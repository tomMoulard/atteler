package vector

import (
	"errors"
	"reflect"
	"testing"
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
