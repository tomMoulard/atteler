package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const (
	defaultChunkRunes   = 800
	defaultOverlapRunes = 80
)

// ChunkOptions configures deterministic text chunking.
type ChunkOptions struct {
	MaxRunes     int
	OverlapRunes int
}

// ChunkedText pairs source text with its chunk citation metadata.
//
//nolint:govet // Layout prioritizes JSON/API readability over pointer-byte packing.
type ChunkedText struct {
	Chunk Chunk
	Text  string
}

// ChunkText splits text into stable rune-offset chunks. Empty text returns no
// chunks. The final chunk may be shorter than MaxRunes.
func ChunkText(documentID, text string, opts ChunkOptions) []ChunkedText {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}

	maxRunes, overlap := normalizeChunkOptions(opts)
	chunks := make([]ChunkedText, 0, (len(runes)/maxRunes)+1)

	for start, index := 0, 0; start < len(runes); index++ {
		end := min(start+maxRunes, len(runes))
		chunkText := string(runes[start:end])
		chunks = append(chunks, ChunkedText{
			Text: chunkText,
			Chunk: Chunk{
				ID:          StableChunkID(documentID, index, start, end, chunkText),
				Index:       index,
				Range:       Range{Unit: RangeUnitRuneOffset, Start: start, End: end},
				ContentHash: TextHash(chunkText),
			},
		})

		if end == len(runes) {
			break
		}

		start = max(0, end-overlap)
	}

	return chunks
}

// BestChunkForTerms returns the chunk with the highest lexical overlap with
// terms. It falls back to the first chunk so every retrieval result has a
// citable range.
func BestChunkForTerms(documentID, text string, terms []string, opts ChunkOptions) ChunkedText {
	chunks := ChunkText(documentID, text, opts)
	if len(chunks) == 0 {
		return ChunkedText{Chunk: Chunk{ID: StableChunkID(documentID, 0, 0, 0, ""), Range: Range{Unit: RangeUnitRuneOffset}}}
	}

	normalizedTerms := make([]string, 0, len(terms))

	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term != "" {
			normalizedTerms = append(normalizedTerms, term)
		}
	}

	best := chunks[0]
	bestScore := -1

	for _, chunk := range chunks {
		score := chunkTermScore(chunk.Text, normalizedTerms)
		if score > bestScore {
			best = chunk
			bestScore = score
		}
	}

	return best
}

// Snippet returns compact text from a chunk, capped at maxRunes runes.
func Snippet(text string, maxRunes int) string {
	clean := strings.Join(strings.Fields(text), " ")
	if maxRunes <= 0 || len([]rune(clean)) <= maxRunes {
		return clean
	}

	runes := []rune(clean)

	return strings.TrimSpace(string(runes[:maxRunes])) + "…"
}

// TextHash returns a short stable hash for text content.
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}

// StableDocumentID derives a deterministic ID from a source and external ID.
func StableDocumentID(source Source, externalID string) string {
	normalized := strings.Join([]string{string(source.Type), source.Name, source.URI, filepath.ToSlash(strings.TrimSpace(externalID))}, "\x00")

	label := slug(externalID)
	if label == "" {
		label = slug(string(source.Type))
	}

	if len(label) > 48 {
		label = label[:48]
	}

	sum := sha256.Sum256([]byte(normalized))

	return label + "-" + hex.EncodeToString(sum[:])[:12]
}

// StableChunkID derives a deterministic chunk ID from document/range/content.
func StableChunkID(documentID string, index, start, end int, text string) string {
	base := strings.Join([]string{documentID, strconv.Itoa(index), strconv.Itoa(start), strconv.Itoa(end), TextHash(text)}, "\x00")
	sum := sha256.Sum256([]byte(base))

	return documentID + "#chunk-" + hex.EncodeToString(sum[:])[:12]
}

func normalizeChunkOptions(opts ChunkOptions) (maxRunes, overlap int) {
	maxRunes = opts.MaxRunes
	if maxRunes <= 0 {
		maxRunes = defaultChunkRunes
	}

	overlap = max(0, opts.OverlapRunes)

	if overlap == 0 && maxRunes > defaultOverlapRunes {
		overlap = defaultOverlapRunes
	}

	if overlap >= maxRunes {
		overlap = maxRunes / 4
	}

	return maxRunes, overlap
}

func chunkTermScore(text string, terms []string) int {
	if len(terms) == 0 {
		return 0
	}

	normalized := strings.ToLower(text)

	var score int

	for _, term := range terms {
		if strings.Contains(normalized, term) {
			score++
		}
	}

	return score
}

var slugSeparator = regexp.MustCompile(`[-_./:\\]+`)

func slug(value string) string {
	value = strings.ToLower(filepath.ToSlash(strings.TrimSpace(value)))
	value = slugSeparator.ReplaceAllString(value, "-")

	var b strings.Builder

	lastDash := false

	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)

			lastDash = false
		case !lastDash:
			b.WriteRune('-')

			lastDash = true
		}
	}

	return strings.Trim(b.String(), "-")
}
