// Package memory provides dependency-free local text indexing and lexical search
// primitives for small retrieval-augmented workflows.
//
//nolint:wsl_v5 // Memory privacy helpers use compact guard clauses around indexing policy.
package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tommoulard/atteler/pkg/retrieval"
)

const (
	defaultSnippetRunes = 160
	// StoreSchemaVersion is the current JSON persistence schema for lexical memory.
	StoreSchemaVersion = SchemaVersion
	// StoreTextNormalization identifies the token normalization used by lexical search.
	StoreTextNormalization = "unicode-letter-digit-lowercase-v1"
)

// SchemaVersion is the current persisted JSON memory-store schema version.
const SchemaVersion = 1

const (
	// ScopeManual is used when callers directly add documents without a broader corpus.
	ScopeManual = "manual"
	// ScopeFile is used for explicit file-indexed documents.
	ScopeFile = "file"
	// ScopeSession is used for a single saved session.
	ScopeSession = "session"
	// ScopeRepo is used for sessions scoped to one repository/worktree.
	ScopeRepo = "repo"
	// ScopeTags is used for sessions selected by tag.
	ScopeTags = "tags"
	// ScopeDateRange is used for sessions selected by created/updated date.
	ScopeDateRange = "date_range"
	// ScopeAgent is used for agent-specific session memory.
	ScopeAgent = "agent"
	// ScopeGlobal is used for opt-in all-session memory.
	ScopeGlobal = "global"
	// ScopeStore is used when callers intentionally search only a persisted store.
	ScopeStore = "store"
)

// CorpusMetadata describes the corpus represented by a memory store.
//
//nolint:govet // JSON/API field readability is preferred over pointer packing.
type CorpusMetadata struct {
	CreatedFrom   []string `json:"created_from,omitempty"`
	SessionIDs    []string `json:"session_ids,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Scope         string   `json:"scope,omitempty"`
	Description   string   `json:"description,omitempty"`
	RepoPath      string   `json:"repo_path,omitempty"`
	Agent         string   `json:"agent,omitempty"`
	DateStart     string   `json:"date_start,omitempty"`
	DateEnd       string   `json:"date_end,omitempty"`
	Retention     string   `json:"retention,omitempty"`
	DocumentCount int      `json:"document_count"`
	SessionCount  int      `json:"session_count"`
	FileCount     int      `json:"file_count"`
	Global        bool     `json:"global,omitempty"`
}

// Provenance records the source that produced an indexed document.
//
//nolint:govet // JSON/API field readability is preferred over pointer packing.
type Provenance struct {
	Tags       []string `json:"tags,omitempty"`
	SourceType string   `json:"source_type,omitempty"`
	SourceID   string   `json:"source_id,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Role       string   `json:"role,omitempty"`
	Agent      string   `json:"agent,omitempty"`
	RepoPath   string   `json:"repo_path,omitempty"`
	Path       string   `json:"path,omitempty"`
	CreatedAt  string   `json:"created_at,omitempty"`
	UpdatedAt  string   `json:"updated_at,omitempty"`
}

// PolicyDecision records privacy and retention decisions for an indexed document.
//
//nolint:govet // JSON/API field readability is preferred over pointer packing.
type PolicyDecision struct {
	RedactionRules []string `json:"redaction_rules,omitempty"`
	Scope          string   `json:"scope,omitempty"`
	Retention      string   `json:"retention,omitempty"`
	Redacted       bool     `json:"redacted"`
}

// Document is a text item indexed by Store.
type Document struct {
	Metadata   map[string]string `json:"metadata,omitempty"`
	Provenance *Provenance       `json:"provenance,omitempty"`
	Policy     *PolicyDecision   `json:"policy,omitempty"`
	ExpiresAt  *time.Time        `json:"expires_at,omitempty"`
	CreatedAt  time.Time         `json:"created_at,omitzero"`
	UpdatedAt  time.Time         `json:"updated_at,omitzero"`
	ID         string            `json:"id"`
	Path       string            `json:"path,omitempty"`
	Text       string            `json:"text"`
	SourceHash string            `json:"source_hash,omitempty"`
}

// Store is a JSON-serializable collection of text documents.
//
//nolint:govet // JSON/API field readability is preferred over pointer packing.
type Store struct {
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	Corpus        CorpusMetadata `json:"corpus"`
	Documents     []Document     `json:"documents"`
	Normalization string         `json:"normalization,omitempty"`
	SchemaVersion int            `json:"schema_version"`
	redactor      *Redactor      `json:"-"`
}

// LoadOptions controls persisted store loading.
type LoadOptions struct {
	// Migrate permits loading older stores by redacting and normalizing them
	// before returning. The current in-memory format already normalizes during
	// load, so this flag is accepted for compatibility with other stores.
	Migrate bool
}

// Result is a ranked lexical match returned by Search.
//
//nolint:govet // Layout prioritizes JSON/API readability over pointer-byte packing.
type Result struct {
	Score    float64  `json:"score"`
	Document Document `json:"document"`
	Matches  []string `json:"matches"`
	Snippet  string   `json:"snippet"`
}

// NewStore returns an empty in-memory document store.
func NewStore() *Store {
	now := time.Now().UTC()
	store := &Store{
		SchemaVersion: SchemaVersion,
		Normalization: StoreTextNormalization,
		CreatedAt:     now,
		UpdatedAt:     now,
		Corpus:        CorpusMetadata{Scope: ScopeManual},
	}
	store.ensureRedactor()

	return store
}

// Tokenize splits text into lowercase unicode letter/digit tokens.
func Tokenize(text string) []string {
	return tokenize(text)
}

// AddText indexes text under id. Existing documents with the same id are replaced.
func (s *Store) AddText(id, text string) error {
	return s.Add(Document{ID: id, Text: text})
}

// IndexText is an alias for AddText.
func (s *Store) IndexText(id, text string) error {
	return s.AddText(id, text)
}

// AddFile reads and indexes a UTF-8 text file. The filepath is used as both ID
// and Path so callers can round-trip search results back to the source file.
func (s *Store) AddFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read memory file %q: %w", path, err)
	}

	if !utf8.Valid(data) {
		return fmt.Errorf("read memory file %q: %w", path, ErrInvalidUTF8)
	}

	clean := filepath.Clean(path)

	return s.Add(Document{
		ID:       clean,
		Path:     clean,
		Text:     string(data),
		Metadata: map[string]string{"source_type": ScopeFile, "kind": ScopeFile, "path": clean},
		Provenance: &Provenance{
			SourceType: ScopeFile,
			SourceID:   clean,
			Kind:       ScopeFile,
			Path:       clean,
		},
		Policy: &PolicyDecision{Scope: ScopeFile},
	})
}

// AddFiles reads and indexes each path in order.
func (s *Store) AddFiles(paths ...string) error {
	for _, path := range paths {
		if err := s.AddFile(path); err != nil {
			return err
		}
	}

	return nil
}

// IndexFile is an alias for AddFile.
func (s *Store) IndexFile(path string) error {
	return s.AddFile(path)
}

// IndexFiles is an alias for AddFiles.
func (s *Store) IndexFiles(paths ...string) error {
	return s.AddFiles(paths...)
}

// Add indexes doc. Existing documents with the same id are replaced.
func (s *Store) Add(doc Document) error {
	s.ensureDefaults()

	doc.ID = strings.TrimSpace(doc.ID)
	if doc.ID == "" {
		return ErrMissingID
	}

	if !utf8.ValidString(doc.Text) {
		return ErrInvalidUTF8
	}

	doc = s.redactedDocument(doc)
	doc.ExpiresAt = cloneTimePtr(doc.ExpiresAt)

	now := time.Now().UTC()
	hasCreatedAt := !doc.CreatedAt.IsZero()
	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = now
	}
	doc.UpdatedAt = now
	doc.SourceHash = sourceHash(doc.Text)

	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now

	for i := range s.Documents {
		if s.Documents[i].ID != doc.ID {
			continue
		}

		if !hasCreatedAt {
			doc.CreatedAt = s.Documents[i].CreatedAt
		}
		s.Documents[i] = doc
		s.recountCorpus()
		s.normalizeCorpusMetadata()

		return nil
	}

	s.Documents = append(s.Documents, doc)
	s.recountCorpus()
	s.normalizeCorpusMetadata()

	return nil
}

// Index is an alias for Add.
func (s *Store) Index(doc Document) error {
	return s.Add(doc)
}

// Search ranks documents by lexical overlap with query and returns up to limit
// results. A limit less than one returns every matching document.
func (s *Store) Search(query string, limit int) ([]Result, error) {
	queryTerms := uniqueTokens(query)
	if len(queryTerms) == 0 {
		return nil, ErrEmptyQuery
	}
	redactor := s.currentRedactor()

	querySet := make(map[string]struct{}, len(queryTerms))
	for _, term := range queryTerms {
		querySet[term] = struct{}{}
	}

	results := make([]Result, 0, len(s.Documents))
	now := time.Now().UTC()
	for i := range s.Documents {
		doc := s.Documents[i]
		if isExpired(doc, now) {
			continue
		}

		safeDoc := redactedSearchDocument(doc, redactor, s.Corpus.Scope)
		tokens := tokenize(stripRedactionMarkers(safeDoc.Text))
		if len(tokens) == 0 {
			continue
		}

		counts := make(map[string]int)

		for _, token := range tokens {
			if _, ok := querySet[token]; ok {
				counts[token]++
			}
		}

		if len(counts) == 0 {
			continue
		}

		matches := sortedKeys(counts)
		snippetText := snippet(safeDoc.Text, matches, defaultSnippetRunes)
		snippetText, _ = redactor.Redact(snippetText)

		results = append(results, Result{
			Document: safeDoc,
			Score:    score(counts, len(queryTerms), len(tokens)),
			Snippet:  snippetText,
			Matches:  matches,
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

// SearchRetrieval returns lexical memory hits using the shared retrieval
// contract.
func (s *Store) SearchRetrieval(ctx context.Context, query retrieval.Query) ([]retrieval.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("memory retrieval: %w", err)
	}

	results, err := s.Search(query.Text, query.Limit)
	if err != nil {
		return nil, err
	}

	out := make([]retrieval.Result, 0, len(results))
	for i := range results {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("memory retrieval: %w", err)
		}

		out = append(out, retrievalResult(results[i], query))
	}

	return out, nil
}

// Save writes the store as pretty-printed JSON.
func (s *Store) Save(path string) error {
	s.ensureDefaults()
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	s.SchemaVersion = SchemaVersion
	s.Normalization = StoreTextNormalization
	s.redactAllDocuments()
	s.redactCorpus()
	s.recountCorpus()
	s.normalizeCorpusMetadata()

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal memory store: %w", err)
	}

	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create memory store dir: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write memory store %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod memory store %q: %w", path, err)
	}

	return nil
}

// Load reads a JSON store saved by Save.
func Load(path string) (*Store, error) {
	return LoadWithOptions(path, LoadOptions{})
}

// LoadWithOptions reads a JSON store saved by Save.
func LoadWithOptions(path string, _ LoadOptions) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read memory store %q: %w", path, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return NewStore(), nil
	}

	var store Store
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("decode memory store %q: %w", path, err)
	}

	store.ensureDefaults()
	store.redactAllDocuments()
	store.redactCorpus()
	store.recountCorpus()
	store.normalizeCorpusMetadata()

	return &store, nil
}

// Compact removes expired documents and returns the number of purged entries.
func (s *Store) Compact(now time.Time) int {
	s.ensureDefaults()
	now = now.UTC()

	kept := s.Documents[:0]
	removed := 0
	for i := range s.Documents {
		if isExpired(s.Documents[i], now) {
			removed++
			continue
		}
		kept = append(kept, s.Documents[i])
	}

	if removed > 0 {
		clear(s.Documents[len(kept):cap(s.Documents)])
		s.Documents = kept
		s.UpdatedAt = time.Now().UTC()
		s.recountCorpus()
		s.normalizeCorpusMetadata()
	}

	return removed
}

// SetRedactor replaces the store redactor and reapplies redaction to already
// loaded documents. A nil redactor restores defaults.
func (s *Store) SetRedactor(redactor *Redactor) {
	if redactor == nil {
		s.redactor = nil
		s.ensureRedactor()
	} else {
		s.redactor = redactor
	}

	s.ensureDefaults()
	s.redactAllDocuments()
	s.redactCorpus()
	s.recountCorpus()
	s.normalizeCorpusMetadata()
}

// SetCustomRedactionRules configures the built-in redaction rules plus custom regexps.
func (s *Store) SetCustomRedactionRules(patterns ...string) error {
	redactor, err := NewRedactor(patterns...)
	if err != nil {
		return err
	}

	s.SetRedactor(redactor)

	return nil
}

// PurgeFilter selects documents to remove from a store.
type PurgeFilter struct {
	SessionID string
	Tag       string
	RepoPath  string
	All       bool
}

// Purge removes memory-derived documents matching filter and returns the count removed.
func (s *Store) Purge(filter PurgeFilter) int {
	s.ensureDefaults()
	filter = s.redactedPurgeFilter(filter)

	if filter.All {
		removed := len(s.Documents)
		s.Documents = nil
		s.Corpus = CorpusMetadata{Scope: ScopeManual}
		s.Corpus.Description = corpusDescription(s.Corpus)

		return removed
	}

	// Purge selectors are redacted before matching, so normalize any legacy raw
	// documents with the active redactor as well. This keeps deletion controls
	// usable after built-in or custom redaction rules hide source identifiers.
	s.redactAllDocuments()
	s.redactCorpus()

	kept := s.Documents[:0]
	removed := 0
	for i := range s.Documents {
		doc := s.Documents[i]
		if purgeMatches(doc, filter) {
			removed++
			continue
		}

		kept = append(kept, doc)
	}

	s.Documents = kept
	if len(s.Documents) == 0 {
		s.Corpus = CorpusMetadata{Scope: ScopeManual}
	}
	s.recountCorpus()
	s.normalizeCorpusMetadata()

	return removed
}

// ApplyRetention removes documents with provenance timestamps older than cutoff.
// Documents without any indexed timestamp are kept because their age cannot be
// established safely.
func (s *Store) ApplyRetention(cutoff time.Time) int {
	s.ensureDefaults()
	if cutoff.IsZero() {
		return 0
	}

	kept := s.Documents[:0]
	removed := 0
	for i := range s.Documents {
		doc := s.Documents[i]
		activity, ok := documentActivity(doc)
		if ok && activity.Before(cutoff) {
			removed++
			continue
		}

		kept = append(kept, doc)
	}

	s.Documents = kept
	if removed > 0 && len(s.Documents) == 0 {
		s.Corpus = CorpusMetadata{
			Scope:     ScopeManual,
			Retention: strings.TrimSpace(s.Corpus.Retention),
		}
	}
	s.recountCorpus()
	s.normalizeCorpusMetadata()

	return removed
}

// ApplyPolicy records an indexing policy decision on all currently stored
// documents. Empty fields leave existing document policy values unchanged.
func (s *Store) ApplyPolicy(scope, retention string) {
	s.ensureDefaults()

	scope = strings.TrimSpace(scope)
	retention = strings.TrimSpace(retention)
	if scope == "" && retention == "" {
		return
	}

	for i := range s.Documents {
		if s.Documents[i].Policy == nil {
			s.Documents[i].Policy = &PolicyDecision{}
		}
		if scope != "" {
			s.Documents[i].Policy.Scope = scope
		}
		if retention != "" {
			s.Documents[i].Policy.Retention = retention
		}
	}
}

func (s *Store) redactedPurgeFilter(filter PurgeFilter) PurgeFilter {
	redactor := s.currentRedactor()
	if strings.TrimSpace(filter.SessionID) != "" {
		filter.SessionID, _ = redactIdentifier(redactor, filter.SessionID, nil)
	}
	if strings.TrimSpace(filter.Tag) != "" {
		filter.Tag, _ = redactIdentifier(redactor, filter.Tag, nil)
	}
	if strings.TrimSpace(filter.RepoPath) != "" {
		filter.RepoPath, _ = redactIdentifier(redactor, filter.RepoPath, nil)
	}

	return filter
}

var (
	// ErrMissingID is returned when indexing a document without an ID.
	ErrMissingID = errors.New("memory document id is required")
	// ErrEmptyQuery is returned when a search query has no tokens.
	ErrEmptyQuery = errors.New("memory search query is empty")
	// ErrInvalidUTF8 is returned when AddFile reads non-UTF-8 content.
	ErrInvalidUTF8 = errors.New("memory file is not valid UTF-8")
)

func (s *Store) ensureDefaults() {
	if s.SchemaVersion == 0 {
		s.SchemaVersion = SchemaVersion
	}
	if strings.TrimSpace(s.Normalization) == "" {
		s.Normalization = StoreTextNormalization
	}

	if strings.TrimSpace(s.Corpus.Scope) == "" {
		s.Corpus.Scope = ScopeManual
	}

	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = s.CreatedAt
	}

	s.Corpus.DocumentCount = len(s.Documents)
	s.ensureRedactor()
}

func (s *Store) recountCorpus() {
	if len(s.Documents) == 0 {
		s.Corpus.DocumentCount = 0
		s.Corpus.FileCount = 0
		s.Corpus.SessionIDs = cleanSortedUniqueStrings(s.Corpus.SessionIDs, false)
		s.Corpus.Tags = cleanSortedUniqueStrings(s.Corpus.Tags, true)
		s.Corpus.SessionCount = len(s.Corpus.SessionIDs)
		s.Corpus.CreatedFrom = corpusSources(s.Corpus.SessionCount, 0)
		s.Corpus.Description = corpusDescription(s.Corpus)

		return
	}

	sessionSeen := make(map[string]struct{})
	tagSeen := make(map[string]struct{})
	sessionIDs := make([]string, 0, len(s.Corpus.SessionIDs))
	tags := make([]string, 0, len(s.Corpus.Tags))
	fileCount := 0

	for i := range s.Documents {
		doc := s.Documents[i]
		if documentIsFile(doc) {
			fileCount++
		}
		for _, tag := range documentTags(doc) {
			key := strings.ToLower(strings.TrimSpace(tag))
			if key == "" {
				continue
			}
			if _, ok := tagSeen[key]; ok {
				continue
			}
			tagSeen[key] = struct{}{}
			tags = append(tags, strings.TrimSpace(tag))
		}

		sessionID := documentSessionID(doc)
		if sessionID == "" {
			continue
		}
		if _, ok := sessionSeen[sessionID]; ok {
			continue
		}

		sessionSeen[sessionID] = struct{}{}
		sessionIDs = append(sessionIDs, sessionID)
	}

	sort.Strings(sessionIDs)
	sort.Slice(tags, func(i, j int) bool {
		return strings.ToLower(tags[i]) < strings.ToLower(tags[j])
	})
	s.Corpus.DocumentCount = len(s.Documents)
	s.Corpus.FileCount = fileCount
	s.Corpus.SessionCount = len(sessionIDs)
	s.refreshCorpusScope(fileCount)
	if len(sessionIDs) > 0 || len(s.Corpus.SessionIDs) > 0 {
		s.Corpus.SessionIDs = sessionIDs
	}
	if len(tags) > 0 || len(s.Corpus.Tags) > 0 {
		s.Corpus.Tags = tags
	}
	s.Corpus.CreatedFrom = corpusSources(s.Corpus.SessionCount, s.Corpus.FileCount)
	s.Corpus.Description = corpusDescription(s.Corpus)
}

func (s *Store) refreshCorpusScope(fileCount int) {
	if len(s.Documents) == 0 {
		if s.Corpus.Scope == ScopeFile {
			s.Corpus.Scope = ScopeManual
		}

		return
	}

	if fileCount == len(s.Documents) && (strings.TrimSpace(s.Corpus.Scope) == "" || s.Corpus.Scope == ScopeManual) {
		s.Corpus.Scope = ScopeFile
	} else if fileCount != len(s.Documents) && s.Corpus.Scope == ScopeFile {
		s.Corpus.Scope = ScopeManual
	}
}

func corpusDescription(corpus CorpusMetadata) string {
	var parts []string
	if strings.TrimSpace(corpus.Scope) != "" {
		parts = append(parts, "scope="+strings.TrimSpace(corpus.Scope))
	}
	if corpus.Global {
		parts = append(parts, "global=opt-in")
	}
	if strings.TrimSpace(corpus.RepoPath) != "" {
		parts = append(parts, "repo="+strings.TrimSpace(corpus.RepoPath))
	}
	if strings.TrimSpace(corpus.Agent) != "" {
		parts = append(parts, "agent="+strings.TrimSpace(corpus.Agent))
	}
	if len(corpus.Tags) > 0 {
		parts = append(parts, "tags="+strings.Join(corpus.Tags, ","))
	}
	if strings.TrimSpace(corpus.DateStart) != "" || strings.TrimSpace(corpus.DateEnd) != "" {
		parts = append(parts, "date_range="+descriptionValue(corpus.DateStart, "*")+".."+descriptionValue(corpus.DateEnd, "*"))
	}
	if strings.TrimSpace(corpus.Retention) != "" {
		parts = append(parts, "retention="+strings.TrimSpace(corpus.Retention))
	}

	return strings.Join(parts, " ")
}

func descriptionValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return strings.TrimSpace(value)
}

func corpusSources(sessionCount, fileCount int) []string {
	sources := make([]string, 0, 2)
	if sessionCount > 0 {
		sources = append(sources, "sessions")
	}
	if fileCount > 0 {
		sources = append(sources, "files")
	}

	return sources
}

func cleanSortedUniqueStrings(values []string, fold bool) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		key := value
		if fold {
			key = strings.ToLower(value)
		}
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}
		out = append(out, value)
	}

	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})

	return out
}

func documentIsFile(doc Document) bool {
	if doc.Provenance != nil && doc.Provenance.SourceType == ScopeFile {
		return true
	}

	return doc.Metadata["source_type"] == ScopeFile || doc.Metadata["kind"] == ScopeFile
}

func documentSessionID(doc Document) string {
	if doc.Provenance != nil && strings.TrimSpace(doc.Provenance.SessionID) != "" {
		return strings.TrimSpace(doc.Provenance.SessionID)
	}

	if sessionID := strings.TrimSpace(doc.Metadata["session_id"]); sessionID != "" {
		return sessionID
	}

	return sessionIDFromDocumentID(doc.ID)
}

func sessionIDFromDocumentID(id string) string {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "session/") {
		parts := strings.Split(id, "/")
		if len(parts) >= 2 {
			return strings.TrimSpace(parts[1])
		}
	}

	return ""
}

func sessionKindFromDocumentID(id string) string {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "session/") {
		parts := strings.Split(id, "/")
		if len(parts) >= 3 {
			return strings.TrimSpace(parts[2])
		}
	}

	return ""
}

func documentTags(doc Document) []string {
	tags := make([]string, 0)
	if doc.Provenance != nil {
		tags = append(tags, doc.Provenance.Tags...)
	}
	tags = append(tags, splitMetadataList(doc.Metadata["tags"])...)

	return tags
}

func documentActivity(doc Document) (time.Time, bool) {
	candidates := make([]string, 0, 4)
	if doc.Provenance != nil {
		candidates = append(candidates, doc.Provenance.UpdatedAt, doc.Provenance.CreatedAt)
	}

	candidates = append(candidates, doc.Metadata["updated_at"], doc.Metadata["created_at"])
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}

		parsed, err := time.Parse(time.RFC3339, candidate)
		if err == nil {
			return parsed.UTC(), true
		}
	}

	return time.Time{}, false
}

func (s *Store) normalizeCorpusMetadata() {
	if len(s.Documents) == 0 {
		s.Corpus = emptyCorpusMetadata(s.Corpus)

		return
	}

	s.clearStaleCorpusSelectors()
	s.Corpus.Scope = s.normalizedCorpusScope()
	s.Corpus.Description = corpusDescription(s.Corpus)
}

func emptyCorpusMetadata(corpus CorpusMetadata) CorpusMetadata {
	empty := CorpusMetadata{
		Scope:     emptyCorpusScope(corpus),
		Retention: strings.TrimSpace(corpus.Retention),
	}
	if empty.Scope != ScopeManual {
		empty.RepoPath = strings.TrimSpace(corpus.RepoPath)
		empty.Agent = strings.TrimSpace(corpus.Agent)
		empty.DateStart = strings.TrimSpace(corpus.DateStart)
		empty.DateEnd = strings.TrimSpace(corpus.DateEnd)
		empty.Tags = cleanSortedUniqueStrings(corpus.Tags, true)
		empty.SessionIDs = cleanSortedUniqueStrings(corpus.SessionIDs, false)
		empty.SessionCount = len(empty.SessionIDs)
		empty.CreatedFrom = corpusSources(empty.SessionCount, 0)
		empty.Global = corpus.Scope == ScopeGlobal && corpus.Global
	} else if empty.Retention != "" {
		empty.DateStart = strings.TrimSpace(corpus.DateStart)
		empty.DateEnd = strings.TrimSpace(corpus.DateEnd)
	}
	empty.Description = corpusDescription(empty)

	return empty
}

func emptyCorpusScope(corpus CorpusMetadata) string {
	switch strings.TrimSpace(corpus.Scope) {
	case ScopeRepo:
		if strings.TrimSpace(corpus.RepoPath) != "" {
			return ScopeRepo
		}
	case ScopeSession:
		if len(cleanSortedUniqueStrings(corpus.SessionIDs, false)) > 0 {
			return ScopeSession
		}
	case ScopeTags:
		if len(cleanSortedUniqueStrings(corpus.Tags, true)) > 0 {
			return ScopeTags
		}
	case ScopeDateRange:
		if strings.TrimSpace(corpus.DateStart) != "" || strings.TrimSpace(corpus.DateEnd) != "" {
			return ScopeDateRange
		}
	case ScopeAgent:
		if strings.TrimSpace(corpus.Agent) != "" {
			return ScopeAgent
		}
	case ScopeGlobal:
		if corpus.Global {
			return ScopeGlobal
		}
	case ScopeStore:
		return ScopeStore
	}

	return ScopeManual
}

func (s *Store) clearStaleCorpusSelectors() {
	if s.Corpus.Scope != ScopeGlobal {
		s.Corpus.Global = false
	}
	if strings.TrimSpace(s.Corpus.RepoPath) != "" && !s.allDocumentsMatchRepo(s.Corpus.RepoPath) {
		s.Corpus.RepoPath = ""
	}
	if strings.TrimSpace(s.Corpus.Agent) != "" && !s.allDocumentsMatchAgent(s.Corpus.Agent) {
		s.Corpus.Agent = ""
	}
	if s.Corpus.Retention == "" &&
		(strings.TrimSpace(s.Corpus.DateStart) != "" || strings.TrimSpace(s.Corpus.DateEnd) != "") &&
		!s.allDocumentsMatchDateRange(s.Corpus.DateStart, s.Corpus.DateEnd) {
		s.Corpus.DateStart = ""
		s.Corpus.DateEnd = ""
	}
}

func (s *Store) normalizedCorpusScope() string {
	scope := strings.TrimSpace(s.Corpus.Scope)
	if scope == "" {
		return ScopeManual
	}
	if scope == ScopeManual || scope == ScopeFile {
		return scope
	}
	if !knownCorpusScope(scope) || !s.corpusScopeSelectorsMatch(scope) {
		return ScopeManual
	}

	return scope
}

func knownCorpusScope(scope string) bool {
	switch scope {
	case ScopeRepo, ScopeSession, ScopeTags, ScopeDateRange, ScopeAgent, ScopeGlobal, ScopeStore:
		return true
	default:
		return false
	}
}

func (s *Store) corpusScopeSelectorsMatch(scope string) bool {
	switch scope {
	case ScopeRepo:
		return strings.TrimSpace(s.Corpus.RepoPath) != ""
	case ScopeSession:
		return s.allDocumentsMatchSingleSession()
	case ScopeTags:
		return len(s.Corpus.Tags) > 0 && s.allDocumentsMatchAnyTag(s.Corpus.Tags)
	case ScopeDateRange:
		return s.corpusDateRangeMatches()
	case ScopeAgent:
		return strings.TrimSpace(s.Corpus.Agent) != ""
	case ScopeGlobal:
		return s.Corpus.Global
	case ScopeStore:
		return true
	default:
		return false
	}
}

func (s *Store) corpusDateRangeMatches() bool {
	if s.allDocumentsMatchDateRange(s.Corpus.DateStart, s.Corpus.DateEnd) {
		return true
	}

	s.Corpus.DateStart = ""
	s.Corpus.DateEnd = ""

	return false
}

func (s *Store) allDocumentsMatchRepo(repoPath string) bool {
	for i := range s.Documents {
		if !documentRepoMatches(s.Documents[i], repoPath) {
			return false
		}
	}

	return len(s.Documents) > 0
}

func (s *Store) allDocumentsMatchSingleSession() bool {
	var sessionID string
	for i := range s.Documents {
		doc := s.Documents[i]
		if !documentIsSessionLike(doc) {
			return false
		}

		docSessionID := documentSessionID(doc)
		if docSessionID == "" {
			return false
		}
		if sessionID == "" {
			sessionID = docSessionID

			continue
		}
		if docSessionID != sessionID {
			return false
		}
	}

	return sessionID != ""
}

func (s *Store) allDocumentsMatchAnyTag(tags []string) bool {
	want := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" {
			want[tag] = struct{}{}
		}
	}
	if len(want) == 0 {
		return false
	}

	for i := range s.Documents {
		doc := s.Documents[i]
		matched := false
		for _, tag := range documentTags(doc) {
			if _, ok := want[strings.ToLower(strings.TrimSpace(tag))]; ok {
				matched = true

				break
			}
		}
		if !matched {
			return false
		}
	}

	return len(s.Documents) > 0
}

func (s *Store) allDocumentsMatchDateRange(startRaw, endRaw string) bool {
	start, hasStart := parseCorpusTime(startRaw)
	end, hasEnd := parseCorpusTime(endRaw)
	if !hasStart && !hasEnd {
		return false
	}

	for i := range s.Documents {
		doc := s.Documents[i]
		activity, ok := documentActivity(doc)
		if !ok {
			return false
		}
		if hasStart && activity.Before(start) {
			return false
		}
		if hasEnd && activity.After(end) {
			return false
		}
	}

	return len(s.Documents) > 0
}

func parseCorpusTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}

	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}

	return parsed.UTC(), true
}

func (s *Store) allDocumentsMatchAgent(agent string) bool {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" {
		return false
	}

	for i := range s.Documents {
		if strings.ToLower(strings.TrimSpace(documentAgentFromDocument(s.Documents[i]))) != agent {
			return false
		}
	}

	return len(s.Documents) > 0
}

func documentAgentFromDocument(doc Document) string {
	if doc.Provenance != nil && strings.TrimSpace(doc.Provenance.Agent) != "" {
		return strings.TrimSpace(doc.Provenance.Agent)
	}

	return firstNonEmpty(doc.Metadata["agent"], doc.Metadata["source_agent"], doc.Metadata["default_agent"])
}

func (s *Store) ensureRedactor() {
	if s.redactor != nil {
		return
	}

	redactor, err := NewRedactor()
	if err != nil {
		// Built-in redaction rules are constants and should never fail to compile.
		panic(err)
	}

	s.redactor = redactor
}

func (s *Store) currentRedactor() *Redactor {
	if s.redactor != nil {
		return s.redactor
	}

	redactor, err := NewRedactor()
	if err != nil {
		// Built-in redaction rules are constants and should never fail to compile.
		panic(err)
	}

	return redactor
}

func (s *Store) redactedDocument(doc Document) Document {
	doc = ensureDocumentProvenance(doc)

	policy := doc.Policy
	if policy == nil {
		policy = &PolicyDecision{}
		doc.Policy = policy
	}
	if strings.TrimSpace(policy.Scope) == "" {
		policy.Scope = documentPolicyScope(doc, s.Corpus.Scope)
	}

	var applied []string
	doc.ID, applied = redactIdentifier(s.redactor, doc.ID, applied)
	doc.Text, applied = s.redactString(doc.Text, applied)
	doc.Path, applied = redactIdentifier(s.redactor, doc.Path, applied)
	doc.Metadata, applied = redactedMetadata(s.redactor, doc.Metadata, applied)
	doc.Provenance, applied = redactedProvenance(s.redactor, doc.Provenance, applied)
	doc.Policy, applied = redactedPolicy(s.redactor, doc.Policy, applied)

	if len(applied) > 0 {
		doc.Policy.Redacted = true
		doc.Policy.RedactionRules = mergeStrings(doc.Policy.RedactionRules, applied)
	}
	doc.SourceHash = sourceHash(doc.Text)

	return doc
}

func documentPolicyScope(doc Document, corpusScope string) string {
	sourceType := ScopeManual
	if doc.Provenance != nil && strings.TrimSpace(doc.Provenance.SourceType) != "" {
		sourceType = strings.TrimSpace(doc.Provenance.SourceType)
	}

	switch sourceType {
	case ScopeFile:
		return ScopeFile
	case sessionSourceType:
		if strings.TrimSpace(corpusScope) != "" && corpusScope != ScopeManual && corpusScope != ScopeFile {
			return strings.TrimSpace(corpusScope)
		}

		return ScopeSession
	default:
		return ScopeManual
	}
}

func ensureDocumentProvenance(doc Document) Document {
	var existing Provenance
	if doc.Provenance != nil {
		existing = *doc.Provenance
		existing.Tags = append([]string(nil), doc.Provenance.Tags...)
	}

	sourceType := firstNonEmpty(existing.SourceType, doc.Metadata["source_type"])
	if sourceType == "" {
		sourceType = ScopeManual
		if strings.TrimSpace(existing.SessionID) != "" ||
			strings.TrimSpace(doc.Metadata["session_id"]) != "" ||
			sessionIDFromDocumentID(doc.ID) != "" {
			sourceType = sessionSourceType
		} else if strings.TrimSpace(existing.Path) != "" ||
			strings.TrimSpace(doc.Path) != "" ||
			strings.TrimSpace(doc.Metadata["path"]) != "" {
			sourceType = ScopeFile
		}
	}

	kind := firstNonEmpty(existing.Kind, doc.Metadata["kind"], sessionKindFromDocumentID(doc.ID), sourceType)
	sessionID := firstNonEmpty(existing.SessionID, doc.Metadata["session_id"])
	if sessionID == "" && sourceType == sessionSourceType {
		sessionID = sessionIDFromDocumentID(doc.ID)
	}
	sourceID := firstNonEmpty(existing.SourceID, doc.Metadata["source_id"], doc.ID)
	if sourceType == sessionSourceType {
		sourceID = firstNonEmpty(existing.SourceID, doc.Metadata["source_id"], sessionID, doc.ID)
	}
	if sourceType == ScopeFile {
		sourceID = firstNonEmpty(existing.SourceID, doc.Metadata["source_id"], doc.Path, doc.Metadata["path"], doc.ID)
	}
	provenancePath := firstNonEmpty(existing.Path, doc.Path, doc.Metadata["path"], doc.Metadata["artifact_path"])
	if provenancePath == "" && sourceType == ScopeFile {
		provenancePath = sourceID
	}

	tags := mergeStrings(existing.Tags, splitMetadataList(doc.Metadata["tags"]))
	doc.Provenance = &Provenance{
		SourceType: sourceType,
		SourceID:   sourceID,
		SessionID:  sessionID,
		Kind:       kind,
		Role:       firstNonEmpty(existing.Role, doc.Metadata["role"]),
		Agent:      firstNonEmpty(existing.Agent, doc.Metadata["agent"], doc.Metadata["source_agent"], doc.Metadata["default_agent"]),
		RepoPath:   firstNonEmpty(existing.RepoPath, doc.Metadata["repo_path"], doc.Metadata["worktree_path"]),
		Path:       provenancePath,
		CreatedAt:  firstNonEmpty(existing.CreatedAt, doc.Metadata["created_at"]),
		UpdatedAt:  firstNonEmpty(existing.UpdatedAt, doc.Metadata["updated_at"]),
		Tags:       tags,
	}

	return doc
}

func splitMetadataList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	var values []string
	for value := range strings.SplitSeq(raw, ",") {
		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}

	return values
}

func isIdentifierMetadataKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "id", "source_id", "session_id", "repo_path", "worktree_path", "path", "artifact_path", "tags", "agent", "source_agent", "default_agent":
		return true
	default:
		return false
	}
}

func isSecretMetadataKey(key string) bool {
	key = strings.ToLower(strings.Trim(strings.TrimSpace(key), `"'`))
	key = strings.Trim(strings.ReplaceAll(key, "-", "_"), "_")
	switch key {
	case "api_key", "apikey", "access_token", "refresh_token", "token", "client_secret", "private_key", "auth", "authorization", "secret", "password":
		return true
	}

	for _, suffix := range []string{"_api_key", "_access_token", "_refresh_token", "_token", "_client_secret", "_private_key", "_auth", "_authorization", "_secret", "_password"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}

	return false
}

func redactedMetadata(redactor *Redactor, metadata map[string]string, applied []string) (redacted map[string]string, rules []string) {
	if metadata == nil {
		return nil, applied
	}

	redacted = make(map[string]string, len(metadata))
	for key, value := range metadata {
		if strings.TrimSpace(key) == "" {
			continue
		}
		redactedKey, keyRules := redactIdentifier(redactor, key, applied)
		applied = keyRules
		switch {
		case isIdentifierMetadataKey(key):
			redacted[redactedKey], applied = redactIdentifier(redactor, value, applied)
		case isSecretMetadataKey(key):
			redacted[redactedKey], applied = redactSecretMetadataValue(redactor, key, value, applied)
		default:
			redacted[redactedKey], applied = redactString(redactor, value, applied)
		}
	}
	if len(redacted) == 0 {
		return nil, applied
	}

	return redacted, applied
}

func redactSecretMetadataValue(redactor *Redactor, key, value string, applied []string) (redacted string, rules []string) {
	if strings.TrimSpace(value) == "" {
		return value, applied
	}

	_, decision := redactor.Redact(key + "=" + value)
	if decision.Redacted {
		return redactionReplacementPrefix + "secret_assignment]", mergeStrings(applied, decision.Rules)
	}

	return redactString(redactor, value, applied)
}

func redactedProvenance(redactor *Redactor, provenance *Provenance, applied []string) (redacted *Provenance, rules []string) {
	if provenance == nil {
		return nil, applied
	}

	copied := *provenance
	copied.Tags = append([]string(nil), provenance.Tags...)
	copied.SourceType, applied = redactString(redactor, copied.SourceType, applied)
	copied.SourceID, applied = redactIdentifier(redactor, copied.SourceID, applied)
	copied.SessionID, applied = redactIdentifier(redactor, copied.SessionID, applied)
	copied.Kind, applied = redactString(redactor, copied.Kind, applied)
	copied.Role, applied = redactString(redactor, copied.Role, applied)
	copied.Agent, applied = redactIdentifier(redactor, copied.Agent, applied)
	copied.RepoPath, applied = redactIdentifier(redactor, copied.RepoPath, applied)
	copied.Path, applied = redactIdentifier(redactor, copied.Path, applied)
	copied.CreatedAt, applied = redactString(redactor, copied.CreatedAt, applied)
	copied.UpdatedAt, applied = redactString(redactor, copied.UpdatedAt, applied)
	for i := range copied.Tags {
		copied.Tags[i], applied = redactIdentifier(redactor, copied.Tags[i], applied)
	}

	return &copied, applied
}

func redactedPolicy(redactor *Redactor, policy *PolicyDecision, applied []string) (redacted *PolicyDecision, rules []string) {
	if policy == nil {
		return nil, applied
	}

	copied := *policy
	copied.RedactionRules = append([]string(nil), policy.RedactionRules...)
	copied.Scope, applied = redactString(redactor, copied.Scope, applied)
	copied.Retention, applied = redactString(redactor, copied.Retention, applied)
	for i := range copied.RedactionRules {
		copied.RedactionRules[i], applied = redactString(redactor, copied.RedactionRules[i], applied)
	}
	copied.RedactionRules = mergeStrings(nil, copied.RedactionRules)

	return &copied, applied
}

func (s *Store) redactAllDocuments() {
	for i := range s.Documents {
		s.Documents[i] = s.redactedDocument(s.Documents[i])
	}
}

func (s *Store) redactCorpus() {
	var applied []string
	s.Corpus.Scope, applied = s.redactString(s.Corpus.Scope, applied)
	s.Corpus.Description, applied = s.redactString(s.Corpus.Description, applied)
	s.Corpus.RepoPath, applied = redactIdentifier(s.redactor, s.Corpus.RepoPath, applied)
	s.Corpus.Agent, applied = redactIdentifier(s.redactor, s.Corpus.Agent, applied)
	s.Corpus.DateStart, applied = s.redactString(s.Corpus.DateStart, applied)
	s.Corpus.DateEnd, applied = s.redactString(s.Corpus.DateEnd, applied)
	s.Corpus.Retention, applied = s.redactString(s.Corpus.Retention, applied)
	for i := range s.Corpus.CreatedFrom {
		s.Corpus.CreatedFrom[i], applied = s.redactString(s.Corpus.CreatedFrom[i], applied)
	}
	for i := range s.Corpus.SessionIDs {
		s.Corpus.SessionIDs[i], applied = redactIdentifier(s.redactor, s.Corpus.SessionIDs[i], applied)
	}
	for i := range s.Corpus.Tags {
		s.Corpus.Tags[i], applied = redactIdentifier(s.redactor, s.Corpus.Tags[i], applied)
	}
}

func redactedSearchDocument(doc Document, redactor *Redactor, scope string) Document {
	policy := &PolicyDecision{Scope: scope}
	if doc.Policy != nil {
		copied := *doc.Policy
		copied.RedactionRules = append([]string(nil), doc.Policy.RedactionRules...)
		policy = &copied
	}
	if strings.TrimSpace(policy.Scope) == "" {
		policy.Scope = scope
	}
	doc.Policy = policy
	doc = ensureDocumentProvenance(doc)

	var applied []string
	doc.ID, applied = redactIdentifier(redactor, doc.ID, applied)
	doc.Text, applied = redactString(redactor, doc.Text, applied)
	doc.Path, applied = redactIdentifier(redactor, doc.Path, applied)
	doc.Metadata, applied = redactedMetadata(redactor, doc.Metadata, applied)
	doc.Provenance, applied = redactedProvenance(redactor, doc.Provenance, applied)
	doc.Policy, applied = redactedPolicy(redactor, doc.Policy, applied)

	if len(applied) > 0 {
		doc.Policy.Redacted = true
		doc.Policy.RedactionRules = mergeStrings(doc.Policy.RedactionRules, applied)
	}

	return doc
}

func (s *Store) redactString(value string, applied []string) (redacted string, rules []string) {
	return redactString(s.redactor, value, applied)
}

func redactString(redactor *Redactor, value string, applied []string) (redacted string, rules []string) {
	value, decision := redactor.Redact(value)
	if decision.Redacted {
		applied = append(applied, decision.Rules...)
	}

	return value, applied
}

func redactIdentifier(redactor *Redactor, value string, applied []string) (redacted string, rules []string) {
	redacted, decision := redactor.RedactIdentifier(value)
	if decision.Redacted {
		applied = append(applied, decision.Rules...)
	}

	return redacted, applied
}

// RedactIdentifier redacts secret-like values while keeping distinct redacted
// identifiers separable with a short fingerprint for each matched secret.
func (r *Redactor) RedactIdentifier(value string) (string, RedactionDecision) {
	if r == nil || value == "" {
		return value, RedactionDecision{}
	}

	out := value
	applied := make(map[string]struct{})
	for _, rule := range r.rules {
		if !rule.re.MatchString(out) {
			continue
		}

		out = rule.re.ReplaceAllStringFunc(out, func(match string) string {
			return redactionReplacementPrefix + rule.name + "]#" + redactionFingerprint(match)
		})
		applied[rule.name] = struct{}{}
	}

	if len(applied) == 0 {
		return out, RedactionDecision{}
	}

	rules := make([]string, 0, len(applied))
	for name := range applied {
		rules = append(rules, name)
	}
	sort.Strings(rules)

	return out, RedactionDecision{Redacted: true, Rules: rules}
}

func redactionFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))

	return hex.EncodeToString(sum[:6])
}

func mergeStrings(existing, added []string) []string {
	if len(existing) == 0 && len(added) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(existing)+len(added))
	for _, value := range existing {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		seen[value] = struct{}{}
	}
	for _, value := range added {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		seen[value] = struct{}{}
	}

	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)

	return out
}

func purgeMatches(doc Document, filter PurgeFilter) bool {
	if strings.TrimSpace(filter.SessionID) != "" && !documentSessionMatches(doc, filter.SessionID) {
		return false
	}

	if strings.TrimSpace(filter.Tag) != "" && !documentTagMatches(doc, filter.Tag) {
		return false
	}

	if strings.TrimSpace(filter.RepoPath) != "" && !documentRepoMatches(doc, filter.RepoPath) {
		return false
	}

	return strings.TrimSpace(filter.SessionID) != "" ||
		strings.TrimSpace(filter.Tag) != "" ||
		strings.TrimSpace(filter.RepoPath) != ""
}

func documentSessionMatches(doc Document, sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}

	return documentSessionID(doc) == sessionID
}

func documentTagMatches(doc Document, tag string) bool {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" {
		return false
	}

	if doc.Provenance != nil {
		for _, candidate := range doc.Provenance.Tags {
			if strings.ToLower(strings.TrimSpace(candidate)) == tag {
				return true
			}
		}
	}

	for candidate := range strings.SplitSeq(doc.Metadata["tags"], ",") {
		if strings.ToLower(strings.TrimSpace(candidate)) == tag {
			return true
		}
	}

	return false
}

func documentRepoMatches(doc Document, repoPath string) bool {
	repoPath = cleanComparablePath(repoPath)
	if repoPath == "" {
		return false
	}

	candidates := []string{
		doc.Metadata["repo_path"],
		doc.Metadata["worktree_path"],
	}
	if doc.Provenance != nil {
		candidates = append(candidates, doc.Provenance.RepoPath)
	}
	for _, candidate := range candidates {
		if comparablePathExact(candidate, repoPath) {
			return true
		}
	}

	for _, candidate := range documentRepoPathCandidates(doc) {
		if comparablePathWithin(candidate, repoPath) {
			return true
		}
	}

	return false
}

func documentRepoPathCandidates(doc Document) []string {
	if documentIsSessionLike(doc) {
		return nil
	}

	candidates := []string{doc.Metadata["path"], doc.Path}
	candidates = appendDocumentProvenanceFilePaths(candidates, doc)
	if documentIsFile(doc) {
		candidates = append(candidates, doc.ID, doc.Metadata["source_id"])
	}

	return candidates
}

func appendDocumentProvenanceFilePaths(candidates []string, doc Document) []string {
	if doc.Provenance == nil {
		return candidates
	}

	candidates = append(candidates, doc.Provenance.Path)
	if doc.Provenance.SourceType == ScopeFile {
		candidates = append(candidates, doc.Provenance.SourceID)
	}

	return candidates
}

func comparablePathExact(path, root string) bool {
	path = cleanComparablePath(path)
	if path == "" {
		return false
	}

	return path == root
}

func documentIsSessionLike(doc Document) bool {
	if doc.Provenance != nil {
		switch doc.Provenance.SourceType {
		case ScopeFile:
			return false
		case sessionSourceType:
			return true
		}
	}
	switch doc.Metadata["source_type"] {
	case ScopeFile:
		return false
	case sessionSourceType:
		return true
	}

	return documentSessionID(doc) != ""
}

func comparablePathWithin(path, root string) bool {
	path = cleanComparablePath(path)
	if path == "" {
		return false
	}
	if path == root {
		return true
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func cleanComparablePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}

	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	return evalComparableSymlinks(path)
}

func evalComparableSymlinks(path string) string {
	path = filepath.Clean(path)
	if evaluated, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(evaluated)
	}

	parent := path
	var suffix []string
	for {
		next := filepath.Dir(parent)
		if next == parent {
			return path
		}

		suffix = append(suffix, filepath.Base(parent))
		parent = next
		if evaluated, err := filepath.EvalSymlinks(parent); err == nil {
			out := filepath.Clean(evaluated)
			for i := len(suffix) - 1; i >= 0; i-- {
				out = filepath.Join(out, suffix[i])
			}

			return filepath.Clean(out)
		}
	}
}

func stripRedactionMarkers(text string) string {
	if !strings.Contains(text, redactionReplacementPrefix) {
		return text
	}

	var b strings.Builder
	for {
		start := strings.Index(text, redactionReplacementPrefix)
		if start < 0 {
			b.WriteString(text)

			break
		}

		b.WriteString(text[:start])

		end := strings.Index(text[start:], "]")
		if end < 0 {
			b.WriteString(text[start:])

			break
		}

		b.WriteByte(' ')
		text = text[start+end+1:]
		text = stripRedactionFingerprintSuffix(text)
	}

	return b.String()
}

func stripRedactionFingerprintSuffix(text string) string {
	if !strings.HasPrefix(text, "#") {
		return text
	}

	end := 1
	for end < len(text) && isASCIIHex(text[end]) {
		end++
	}
	if end == 1 {
		return text
	}

	return text[end:]
}

func isASCIIHex(b byte) bool {
	return (b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'f') ||
		(b >= 'A' && b <= 'F')
}

func retrievalResult(result Result, query retrieval.Query) retrieval.Result {
	doc := result.Document
	source := retrieval.Source{Type: retrieval.SourceMemory, Name: firstNonEmpty(doc.Metadata["kind"], doc.ID)}
	if doc.Provenance != nil {
		switch doc.Provenance.SourceType {
		case ScopeFile:
			source = retrieval.Source{Type: retrieval.SourceFile, Name: firstNonEmpty(doc.Provenance.Path, doc.Path, doc.ID), URI: firstNonEmpty(doc.Provenance.Path, doc.Path)}
		case sessionSourceType:
			source = retrieval.Source{Type: retrieval.SourceSession, Name: firstNonEmpty(doc.Provenance.SessionID, doc.ID), URI: firstNonEmpty(doc.Provenance.Path, doc.Path)}
		}
	}

	chunk := retrieval.BestChunkForTerms(doc.ID, doc.Text, result.Matches, retrieval.ChunkOptions{})
	scorer := retrieval.Scorer{
		Name: "lexical-token-overlap",
		Raw:  result.Score,
		Details: map[string]float64{
			"matches":     float64(len(result.Matches)),
			"query_terms": float64(len(uniqueTokens(query.Text))),
		},
	}
	if query.Explain {
		scorer.Explanation = []string{
			"ranked by query-token coverage plus in-document match density",
			"matched terms: " + strings.Join(result.Matches, ", "),
		}
	}

	metadata := cloneMetadata(doc.Metadata)
	if doc.Policy != nil && doc.Policy.Redacted {
		metadata = ensureMetadata(metadata)
		metadata[retrieval.MetadataSafetyRedacted] = "true"
	}

	return retrieval.NormalizeResult(retrieval.Result{
		Source:     source,
		DocumentID: doc.ID,
		Chunk:      chunk.Chunk,
		Score:      retrieval.NormalizeRawScore(result.Score),
		Scorer:     scorer,
		Snippet:    firstNonEmpty(result.Snippet, retrieval.Snippet(chunk.Text, defaultSnippetRunes)),
		Metadata:   metadata,
		Freshness:  retrieval.FreshnessFromMetadata(metadata),
		Safety:     retrieval.SafetyFromMetadata(metadata),
	})
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	out := make(map[string]string, len(metadata))
	maps.Copy(out, metadata)

	return out
}

func ensureMetadata(metadata map[string]string) map[string]string {
	if metadata != nil {
		return metadata
	}

	return make(map[string]string)
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}

	copied := value.UTC()

	return &copied
}

func isExpired(doc Document, now time.Time) bool {
	return doc.ExpiresAt != nil && !doc.ExpiresAt.IsZero() && !doc.ExpiresAt.After(now.UTC())
}

func sourceHash(text string) string {
	sum := sha256.Sum256([]byte(text))

	return "sha256:" + hex.EncodeToString(sum[:])
}

func tokenize(text string) []string {
	var (
		tokens []string
		b      strings.Builder
	)

	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			continue
		}

		if b.Len() > 0 {
			tokens = append(tokens, b.String())
			b.Reset()
		}
	}

	if b.Len() > 0 {
		tokens = append(tokens, b.String())
	}

	return tokens
}

func uniqueTokens(text string) []string {
	seen := make(map[string]struct{})

	var terms []string

	for _, token := range tokenize(text) {
		if _, ok := seen[token]; ok {
			continue
		}

		seen[token] = struct{}{}
		terms = append(terms, token)
	}

	return terms
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

func score(counts map[string]int, queryTerms, documentTokens int) float64 {
	var frequency int
	for _, count := range counts {
		frequency += count
	}

	coverage := float64(len(counts)) / float64(queryTerms)
	density := float64(frequency) / float64(documentTokens)

	return coverage + density
}

func snippet(text string, terms []string, maxRunes int) string {
	clean := strings.Join(strings.Fields(text), " ")
	if clean == "" || maxRunes < 1 {
		return ""
	}

	firstRune := -1

	for _, term := range terms {
		idx := runeIndexFold(clean, term)
		if idx >= 0 && (firstRune < 0 || idx < firstRune) {
			firstRune = idx
		}
	}

	start := 0
	if firstRune >= 0 {
		start = max(0, firstRune-maxRunes/3)
	}

	runes := []rune(clean)
	if start > len(runes) {
		start = 0
	}

	end := min(start+maxRunes, len(runes))

	out := strings.TrimSpace(string(runes[start:end]))
	if start > 0 {
		out = "…" + out
	}

	if end < len(runes) {
		out += "…"
	}

	return out
}

func runeIndexFold(text, term string) int {
	textRunes := []rune(strings.ToLower(text))

	termRunes := []rune(strings.ToLower(term))
	if len(termRunes) == 0 || len(termRunes) > len(textRunes) {
		return -1
	}

	for i := 0; i <= len(textRunes)-len(termRunes); i++ {
		match := true

		for j, termRune := range termRunes {
			if textRunes[i+j] != termRune {
				match = false
				break
			}
		}

		if match {
			return i
		}
	}

	return -1
}
