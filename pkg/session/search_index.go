package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
)

const (
	searchIndexFileName      = ".session-search-index"
	searchIndexLockFileName  = ".session-search-index.lock"
	searchIndexVersion       = 2
	searchIndexSchemaVersion = 12
	sessionSchemaVersion     = 1

	defaultTranscriptRetention = 180 * 24 * time.Hour
)

var errSearchIndexStale = errors.New("session: search index stale")

// SearchIndexPolicy controls what durable session content is indexed for
// retrieval. Transcript content older than MaxTranscriptAge is omitted unless
// MaxTranscriptAge is negative. If date metadata is excluded while transcript
// retention is finite, transcripts are omitted because expiry cannot be
// enforced without retaining derived dates in the index.
type SearchIndexPolicy struct {
	// ExcludedFields omits entire field families from the durable search index.
	ExcludedFields []SearchField
	// SensitiveFields extends the default redaction patterns with field names
	// whose values should be redacted before indexing.
	SensitiveFields []string
	// MaxTranscriptAge limits transcript indexing by latest session activity.
	// Zero uses the safe default retention; negative disables transcript expiry.
	MaxTranscriptAge time.Duration
}

type normalizedSearchIndexPolicy struct {
	excludedFields   map[SearchField]struct{}
	sensitiveFields  []string
	maxTranscriptAge time.Duration
}

type searchIndexPolicyFingerprint struct {
	ExcludedFields        []SearchField `json:"excluded_fields,omitempty"`
	SensitiveFields       []string      `json:"sensitive_fields,omitempty"`
	MaxTranscriptAgeNanos int64         `json:"max_transcript_age_nanos"`
}

//nolint:govet // JSON field order keeps index metadata readable; this file is small CLI state.
type sessionSearchIndex struct {
	Terms                map[string][]string          `json:"terms"`
	Sessions             []indexedSession             `json:"sessions"`
	Policy               searchIndexPolicyFingerprint `json:"policy"`
	Files                []indexedSessionFile         `json:"files"`
	Integrity            string                       `json:"integrity"`
	BuiltAt              time.Time                    `json:"built_at,omitzero"`
	NextTranscriptExpiry time.Time                    `json:"next_transcript_expiry,omitzero"`
	Version              int                          `json:"version"`
	SchemaVersion        int                          `json:"schema_version"`
	SessionSchema        int                          `json:"session_schema_version"`
}

type indexedSessionFile struct {
	Name            string `json:"-"`
	Key             string `json:"key"`
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano,omitempty"`
}

type indexedSession struct {
	Fields              []indexedField `json:"fields"`
	Agents              []string       `json:"agents,omitempty"`
	Models              []string       `json:"models,omitempty"`
	Repositories        []string       `json:"repositories,omitempty"`
	Tags                []string       `json:"tags,omitempty"`
	FileName            string         `json:"file_name,omitempty"`
	Key                 string         `json:"key"`
	TranscriptExpiresAt time.Time      `json:"transcript_expires_at,omitzero"`
	Summary             Summary        `json:"summary"`
}

type indexedField struct {
	CreatedAt time.Time   `json:"created_at,omitzero"`
	Role      llm.Role    `json:"role,omitempty"`
	Field     SearchField `json:"field"`
	Label     string      `json:"label"`
	Value     string      `json:"value"`
}

type searchIndexDocumentIntegrity struct {
	keys                 map[string]struct{}
	nextTranscriptExpiry time.Time
	hasSearchableField   bool
}

func (s *Store) searchIndexPath() string {
	return filepath.Join(s.dir, searchIndexFileName)
}

func (s *Store) searchIndexLockPath() string {
	return filepath.Join(s.dir, searchIndexLockFileName)
}

func (s *Store) ensureSearchIndex() (sessionSearchIndex, error) {
	index, err := s.readSearchIndex()
	if err == nil {
		return index, nil
	}

	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(s.dir); errors.Is(statErr, os.ErrNotExist) {
			index := newSearchIndex(normalizeSearchIndexPolicy(s.indexPolicy))
			index.finish()

			return index, nil
		}
	}

	return s.rebuildSearchIndex()
}

func (s *Store) readSearchIndex() (sessionSearchIndex, error) {
	return s.readSearchIndexWithValidation(true)
}

func (s *Store) readSearchIndexWithValidation(validateFiles bool) (sessionSearchIndex, error) {
	path := s.searchIndexPath()

	data, err := os.ReadFile(path)
	if err != nil {
		return sessionSearchIndex{}, fmt.Errorf("session: read search index %s: %w", path, err)
	}

	var index sessionSearchIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return sessionSearchIndex{}, fmt.Errorf("session: parse search index %s: %w", path, err)
	}

	if index.Version != searchIndexVersion ||
		index.SchemaVersion != searchIndexSchemaVersion ||
		index.SessionSchema != sessionSchemaVersion {
		return sessionSearchIndex{}, errSearchIndexStale
	}

	policy := normalizeSearchIndexPolicy(s.indexPolicy)
	if !sameSearchIndexPolicy(index.Policy, policy.fingerprint()) {
		return sessionSearchIndex{}, errSearchIndexStale
	}

	if index.Terms == nil {
		return sessionSearchIndex{}, errSearchIndexStale
	}

	if index.Integrity == "" || index.Integrity != searchIndexIntegrityDigest(index) {
		return sessionSearchIndex{}, errSearchIndexStale
	}

	if !validSearchIndexIntegrity(index) {
		return sessionSearchIndex{}, errSearchIndexStale
	}

	if searchIndexRetentionExpired(index, time.Now().UTC()) {
		return sessionSearchIndex{}, errSearchIndexStale
	}

	if validateFiles {
		files, err := currentSessionFiles(s.dir)
		if err != nil {
			return sessionSearchIndex{}, err
		}

		files = indexedSessionFilesForPolicy(files, policy)
		if !sameSessionFiles(index.Files, files) {
			return sessionSearchIndex{}, errSearchIndexStale
		}
	}

	return index, nil
}

func (s *Store) rebuildSearchIndex() (sessionSearchIndex, error) {
	var index sessionSearchIndex

	err := s.withSearchIndexLock(func() error {
		rebuilt, err := s.rebuildSearchIndexUnlocked()
		if err != nil {
			return err
		}

		index = rebuilt

		return nil
	})

	return index, err
}

func (s *Store) rebuildSearchIndexUnlocked() (sessionSearchIndex, error) {
	policy := normalizeSearchIndexPolicy(s.indexPolicy)
	index := newSearchIndex(policy)

	files, err := currentSessionFiles(s.dir)
	if err != nil {
		return sessionSearchIndex{}, err
	}

	index.Files = indexedSessionFilesForPolicy(files, policy)

	for _, file := range files {
		path := filepath.Join(s.dir, file.Name)

		session, err := s.Load(path)
		if err != nil {
			index.addUnindexedSessionFile(file.Name)

			continue
		}

		index.addSession(path, session, policy)
	}

	index.finish()

	if err := s.writeSearchIndex(index); err != nil {
		return sessionSearchIndex{}, err
	}

	return index, nil
}

func (s *Store) indexSavedSession(path string) error {
	return s.withSearchIndexLock(func() error {
		return s.indexSavedSessionUnlocked(path)
	})
}

func (s *Store) indexSavedSessionUnlocked(path string) error {
	policy := normalizeSearchIndexPolicy(s.indexPolicy)

	session, err := s.Load(path)
	if err != nil {
		return fmt.Errorf("session: load saved session for search index: %w", err)
	}

	index, err := s.readSearchIndexWithValidation(false)
	if err != nil {
		_, rebuildErr := s.rebuildSearchIndexUnlocked()

		return rebuildErr
	}

	files, err := currentSessionFiles(s.dir)
	if err != nil {
		return err
	}

	files = indexedSessionFilesForPolicy(files, policy)
	if !sameSessionFilesExcept(index.Files, files, filepath.Base(path)) {
		_, rebuildErr := s.rebuildSearchIndexUnlocked()

		return rebuildErr
	}

	index.BuiltAt = searchIndexBuildTime(policy)
	index.Files = files
	index.removeSession(session.ID)
	index.removeSessionFile(filepath.Base(path))
	index.addSession(path, session, policy)
	index.finish()

	return s.writeSearchIndex(index)
}

func (s *Store) withSearchIndexLock(fn func() error) (lockErr error) {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("session: create dir: %w", err)
	}

	file, err := os.OpenFile(s.searchIndexLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("session: open search index lock: %w", err)
	}
	defer file.Close()

	if err := lockSessionFile(file, "search index lock"); err != nil {
		return err
	}

	defer func() {
		if unlockErr := unlockSessionFile(file, "search index lock"); lockErr == nil && unlockErr != nil {
			lockErr = unlockErr
		}
	}()

	return fn()
}

func newSearchIndex(policy normalizedSearchIndexPolicy) sessionSearchIndex {
	return sessionSearchIndex{
		Version:       searchIndexVersion,
		SchemaVersion: searchIndexSchemaVersion,
		SessionSchema: sessionSchemaVersion,
		BuiltAt:       searchIndexBuildTime(policy),
		Policy:        policy.fingerprint(),
		Terms:         make(map[string][]string),
	}
}

func searchIndexBuildTime(policy normalizedSearchIndexPolicy) time.Time {
	if policy.excludes(SearchFieldDate) {
		return time.Time{}
	}

	return time.Now().UTC()
}

func currentSessionFiles(dir string) ([]indexedSessionFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("session: list indexed session files %s: %w", dir, err)
	}

	files := make([]indexedSessionFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isDurableSessionFileName(entry.Name()) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("session: stat indexed session file %s: %w", entry.Name(), err)
		}

		files = append(files, indexedSessionFile{
			Name:            entry.Name(),
			Key:             searchIndexFileKey(entry.Name()),
			Size:            info.Size(),
			ModTimeUnixNano: info.ModTime().UnixNano(),
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Name < files[j].Name
	})

	return files, nil
}

func indexedSessionFilesForPolicy(files []indexedSessionFile, policy normalizedSearchIndexPolicy) []indexedSessionFile {
	files = append([]indexedSessionFile(nil), files...)
	if !policy.excludes(SearchFieldDate) {
		return files
	}

	for i := range files {
		files[i].ModTimeUnixNano = 0
	}

	return files
}

func isDurableSessionFileName(name string) bool {
	return filepath.Ext(name) == sessionFileExt && !strings.HasPrefix(name, ".session-")
}

func sameSessionFiles(left, right []indexedSessionFile) bool {
	if len(left) != len(right) {
		return false
	}

	for i := range left {
		if left[i].Key != right[i].Key ||
			left[i].Size != right[i].Size ||
			left[i].ModTimeUnixNano != right[i].ModTimeUnixNano {
			return false
		}
	}

	return true
}

func sameSessionFilesExcept(left, right []indexedSessionFile, ignoredName string) bool {
	return sameSessionFiles(withoutSessionFile(left, searchIndexFileKey(ignoredName)), withoutSessionFile(right, searchIndexFileKey(ignoredName)))
}

func validSearchIndexIntegrity(index sessionSearchIndex) bool {
	fileKeys, ok := searchIndexFileKeys(index.Files)
	if !ok {
		return false
	}

	documentIntegrity, ok := searchIndexDocumentKeys(index.Sessions)
	if !ok {
		return false
	}

	if !sameSearchIndexKeySet(fileKeys, documentIntegrity.keys) {
		return false
	}

	if documentIntegrity.hasSearchableField != (len(index.Terms) > 0) {
		return false
	}

	if !validSearchIndexTerms(index.Terms, documentIntegrity.keys) {
		return false
	}

	expectedTerms, expectedNextExpiry := searchIndexTermsAndNextExpiry(index.Sessions)
	if !sameSearchIndexTerms(index.Terms, expectedTerms) {
		return false
	}

	return index.NextTranscriptExpiry.Equal(documentIntegrity.nextTranscriptExpiry) &&
		index.NextTranscriptExpiry.Equal(expectedNextExpiry)
}

func searchIndexFileKeys(files []indexedSessionFile) (map[string]struct{}, bool) {
	keys := make(map[string]struct{}, len(files))
	for _, file := range files {
		if file.Key == "" {
			return nil, false
		}

		if _, ok := keys[file.Key]; ok {
			return nil, false
		}

		keys[file.Key] = struct{}{}
	}

	return keys, true
}

func searchIndexDocumentKeys(sessions []indexedSession) (searchIndexDocumentIntegrity, bool) {
	integrity := searchIndexDocumentIntegrity{keys: make(map[string]struct{}, len(sessions))}

	for i := range sessions {
		document := sessions[i]
		if document.Key == "" {
			return searchIndexDocumentIntegrity{}, false
		}

		if _, ok := integrity.keys[document.Key]; ok {
			return searchIndexDocumentIntegrity{}, false
		}

		integrity.keys[document.Key] = struct{}{}

		if !document.TranscriptExpiresAt.IsZero() &&
			(integrity.nextTranscriptExpiry.IsZero() ||
				document.TranscriptExpiresAt.Before(integrity.nextTranscriptExpiry)) {
			integrity.nextTranscriptExpiry = document.TranscriptExpiresAt
		}

		if !integrity.hasSearchableField && indexedFieldsHaveSearchToken(document.Fields) {
			integrity.hasSearchableField = true
		}
	}

	return integrity, true
}

func sameSearchIndexKeySet(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}

	for key := range left {
		if _, ok := right[key]; !ok {
			return false
		}
	}

	return true
}

func validSearchIndexTerms(terms map[string][]string, documentKeys map[string]struct{}) bool {
	for term, keys := range terms {
		if term == "" || len(keys) == 0 {
			return false
		}

		for i, key := range keys {
			if _, ok := documentKeys[key]; !ok {
				return false
			}

			if i > 0 && keys[i-1] >= key {
				return false
			}
		}
	}

	return true
}

func sameSearchIndexTerms(left, right map[string][]string) bool {
	if len(left) != len(right) {
		return false
	}

	for term, leftKeys := range left {
		rightKeys, ok := right[term]
		if !ok || !slices.Equal(leftKeys, rightKeys) {
			return false
		}
	}

	return true
}

func indexedFieldsHaveSearchToken(fields []indexedField) bool {
	for _, field := range fields {
		if hasSearchToken(field.Value) {
			return true
		}
	}

	return false
}

func hasSearchToken(value string) bool {
	for _, r := range value {
		if isSearchTokenRune(r) {
			return true
		}
	}

	return false
}

func searchIndexTermsAndNextExpiry(sessions []indexedSession) (map[string][]string, time.Time) {
	termSessions := make(map[string]map[string]struct{})
	nextTranscriptExpiry := time.Time{}

	for i := range sessions {
		document := sessions[i]
		if !document.TranscriptExpiresAt.IsZero() &&
			(nextTranscriptExpiry.IsZero() || document.TranscriptExpiresAt.Before(nextTranscriptExpiry)) {
			nextTranscriptExpiry = document.TranscriptExpiresAt
		}

		for _, field := range document.Fields {
			for _, token := range tokenizeSearchText(field.Value) {
				if _, ok := termSessions[token]; !ok {
					termSessions[token] = make(map[string]struct{})
				}

				termSessions[token][document.Key] = struct{}{}
			}
		}
	}

	terms := make(map[string][]string, len(termSessions))
	for token, sessionIDs := range termSessions {
		ids := make([]string, 0, len(sessionIDs))
		for id := range sessionIDs {
			ids = append(ids, id)
		}

		sort.Strings(ids)
		terms[token] = ids
	}

	return terms, nextTranscriptExpiry
}

func withoutSessionFile(files []indexedSessionFile, ignoredKey string) []indexedSessionFile {
	filtered := make([]indexedSessionFile, 0, len(files))
	for _, file := range files {
		if file.Key == ignoredKey {
			continue
		}

		filtered = append(filtered, file)
	}

	return filtered
}

func (idx *sessionSearchIndex) addSession(path string, session Session, policy normalizedSearchIndexPolicy) {
	document := buildIndexedSession(path, session, policy, idx.BuiltAt)
	idx.Sessions = append(idx.Sessions, document)
}

func (idx *sessionSearchIndex) addUnindexedSessionFile(name string) {
	key := searchIndexFileKey(name)
	if key == "" {
		return
	}

	idx.Sessions = append(idx.Sessions, indexedSession{Key: key})
}

func searchIndexFileKey(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(name))

	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Store) indexedSessionPath(document *indexedSession) string {
	if document == nil {
		return ""
	}

	if document.FileName != "" {
		return filepath.Join(s.dir, document.FileName)
	}

	if document.Summary.Path != "" {
		return document.Summary.Path
	}

	if document.Summary.ID != "" {
		return s.path(document.Summary.ID)
	}

	return document.Summary.Path
}

func (idx *sessionSearchIndex) removeSession(sessionID string) {
	if sessionID == "" {
		return
	}

	filtered := idx.Sessions[:0]
	for i := range idx.Sessions {
		document := idx.Sessions[i]
		if document.Summary.ID == sessionID {
			continue
		}

		filtered = append(filtered, document)
	}

	idx.Sessions = filtered
	idx.Terms = make(map[string][]string)
}

func (idx *sessionSearchIndex) removeSessionFile(fileName string) {
	fileKey := searchIndexFileKey(fileName)
	if fileKey == "" {
		return
	}

	filtered := idx.Sessions[:0]
	for i := range idx.Sessions {
		document := idx.Sessions[i]
		if document.Key == fileKey {
			continue
		}

		filtered = append(filtered, document)
	}

	idx.Sessions = filtered
	idx.Terms = make(map[string][]string)
}

func (idx *sessionSearchIndex) finish() {
	sort.Slice(idx.Sessions, func(i, j int) bool {
		if idx.Sessions[i].Summary.ID != idx.Sessions[j].Summary.ID {
			return idx.Sessions[i].Summary.ID < idx.Sessions[j].Summary.ID
		}

		return idx.Sessions[i].Key < idx.Sessions[j].Key
	})

	idx.Terms, idx.NextTranscriptExpiry = searchIndexTermsAndNextExpiry(idx.Sessions)
	idx.Integrity = searchIndexIntegrityDigest(*idx)
}

func searchIndexIntegrityDigest(index sessionSearchIndex) string {
	index.Integrity = ""

	data, err := json.Marshal(index)
	if err != nil {
		data = []byte(fmt.Sprintf("%#v", index))
	}

	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Store) writeSearchIndex(index sessionSearchIndex) error {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("session: create dir: %w", err)
	}

	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal search index: %w", err)
	}

	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.dir, ".session-search-index-*")
	if err != nil {
		return fmt.Errorf("session: create search index temp: %w", err)
	}

	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("session: write search index temp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("session: close search index temp: %w", err)
	}

	if err := os.Rename(tmpPath, s.searchIndexPath()); err != nil {
		return fmt.Errorf("session: replace search index: %w", err)
	}

	return nil
}

func buildIndexedSession(
	path string,
	session Session,
	policy normalizedSearchIndexPolicy,
	now time.Time,
) indexedSession {
	fileName := filepath.Base(path)
	document := indexedSession{
		Key:                 searchIndexFileKey(fileName),
		TranscriptExpiresAt: indexedTranscriptExpiry(session, policy, now),
		Summary:             indexedSummary(path, session, policy),
	}

	if !policy.excludes(SearchFieldSession) && sanitizeIndexFieldString("session.file_name", fileName, policy) == fileName {
		document.FileName = fileName
	}

	if !policy.excludes(SearchFieldTags) {
		document.Tags = normalizedUniqueIndexed("session.tags", session.Tags, policy)
	}

	if !policy.excludes(SearchFieldAgent) {
		document.Agents = normalizedUniqueIndexed("session.default_agent", []string{session.DefaultAgent}, policy)
	}

	if !policy.excludes(SearchFieldModel) {
		document.Models = normalizedUniqueIndexed("session.default_model", []string{session.DefaultModel}, policy)
	}

	if !policy.excludes(SearchFieldRepo) {
		document.Repositories = normalizedUniqueIndexed("session.worktree_path", []string{
			indexRepoPathValue("session.worktree_path", session.WorktreePath, policy),
		}, policy)
	}

	addSessionMetadataFields(&document, session, policy)
	addTranscriptFields(&document, session, policy, now)
	addFailureFields(&document, session, policy)
	addEvaluationFields(&document, session, policy)
	addArtifactFields(&document, session, policy)
	addMultiAgentRunFields(&document, session, policy)

	return document
}

func indexedSummary(path string, session Session, policy normalizedSearchIndexPolicy) Summary {
	summary := summarize(path, session)
	summary.Path = sanitizeIndexFieldString("session.path", summary.Path, policy)

	if policy.excludes(SearchFieldSession) {
		summary.ID = ""
		summary.Path = ""
	} else {
		summary.ID = sanitizeIndexFieldString("session.id", summary.ID, policy)
	}

	if policy.excludes(SearchFieldTitle) {
		summary.Title = ""
	} else {
		summary.Title = sanitizeIndexFieldString("session.title", summary.Title, policy)
	}

	if policy.excludes(SearchFieldDate) {
		summary.CreatedAt = time.Time{}
		summary.UpdatedAt = time.Time{}
	}

	if policy.excludes(SearchFieldAgent) {
		summary.DefaultAgent = ""
	} else {
		summary.DefaultAgent = sanitizeIndexFieldString("session.default_agent", summary.DefaultAgent, policy)
	}

	if policy.excludes(SearchFieldModel) {
		summary.DefaultModel = ""
	} else {
		summary.DefaultModel = sanitizeIndexFieldString("session.default_model", summary.DefaultModel, policy)
	}

	if policy.excludes(SearchFieldRepo) {
		summary.WorktreePath = ""
		summary.WorktreeBranch = ""
		summary.WorktreeBase = ""
	} else {
		summary.WorktreePath = indexRepoPathValue("session.worktree_path", summary.WorktreePath, policy)
		summary.WorktreeBranch = sanitizeIndexFieldString("session.worktree_branch", summary.WorktreeBranch, policy)
		summary.WorktreeBase = sanitizeIndexFieldString("session.worktree_base", summary.WorktreeBase, policy)
	}

	if policy.excludes(SearchFieldTags) {
		summary.Tags = nil
	} else {
		summary.Tags = sanitizeIndexStrings("session.tags", summary.Tags, policy)
	}

	return summary
}

func addSessionMetadataFields(document *indexedSession, session Session, policy normalizedSearchIndexPolicy) {
	addIndexedField(document, policy, SearchFieldSession, "session.id", "", session.ID, time.Time{})
	addIndexedField(document, policy, SearchFieldTitle, "session.title", "", session.Title, time.Time{})
	addIndexedField(document, policy, SearchFieldAgent, "session.default_agent", "", session.DefaultAgent, time.Time{})
	addIndexedField(document, policy, SearchFieldModel, "session.default_model", "", session.DefaultModel, time.Time{})

	addIndexedField(document, policy, SearchFieldRepo, "session.worktree_path", "", indexRepoPathValue("session.worktree_path", session.WorktreePath, policy), time.Time{})
	addIndexedField(document, policy, SearchFieldRepo, "session.worktree_branch", "", session.WorktreeBranch, time.Time{})
	addIndexedField(document, policy, SearchFieldRepo, "session.worktree_base", "", session.WorktreeBase, time.Time{})

	for index, tag := range session.Tags {
		addIndexedField(document, policy, SearchFieldTags, fmt.Sprintf("session.tags[%d]", index+1), "", tag, time.Time{})
	}

	if !session.CreatedAt.IsZero() {
		addIndexedField(document, policy, SearchFieldDate, "session.created_at", "", session.CreatedAt.UTC().Format(time.RFC3339), session.CreatedAt)
	}

	if !session.UpdatedAt.IsZero() {
		addIndexedField(document, policy, SearchFieldDate, "session.updated_at", "", session.UpdatedAt.UTC().Format(time.RFC3339), session.UpdatedAt)
	}
}

func addTranscriptFields(document *indexedSession, session Session, policy normalizedSearchIndexPolicy, now time.Time) {
	if policy.excludes(SearchFieldTranscript) || transcriptExpired(session, policy, now) {
		return
	}

	for index, message := range session.Messages {
		label := fmt.Sprintf("messages[%d]", index+1)
		addIndexedField(document, policy, SearchFieldTranscript, label+".role", message.Role, string(message.Role), time.Time{})
		addIndexedField(document, policy, SearchFieldTranscript, label+".content", message.Role, message.Content, time.Time{})
	}
}

func addFailureFields(document *indexedSession, session Session, policy normalizedSearchIndexPolicy) {
	if policy.excludes(SearchFieldFailures) {
		return
	}

	for index, entry := range session.NegativeKnowledge {
		label := fmt.Sprintf("negative_knowledge[%d]", index+1)
		if !policy.excludes(SearchFieldAgent) {
			document.Agents = appendNormalizedUniqueIndexed(document.Agents, label+".agent", entry.Agent, policy)
		}

		addIndexedField(document, policy, SearchFieldAgent, label+".agent", llm.Role("negative_knowledge"), entry.Agent, entry.CreatedAt)
		addIndexedField(document, policy, SearchFieldFailures, label, llm.Role("negative_knowledge"), indexedNegativeKnowledgeSearchText(label, entry, policy), entry.CreatedAt)
	}
}

func addEvaluationFields(document *indexedSession, session Session, policy normalizedSearchIndexPolicy) {
	if policy.excludes(SearchFieldEvaluations) {
		return
	}

	for index := range session.Evaluations {
		entry := &session.Evaluations[index]

		label := fmt.Sprintf("evaluations[%d]", index+1)
		if !policy.excludes(SearchFieldAgent) {
			document.Agents = appendNormalizedUniqueIndexed(document.Agents, label+".agent", entry.Agent, policy)
		}

		if !policy.excludes(SearchFieldModel) {
			document.Models = appendNormalizedUniqueIndexed(document.Models, label+".model", entry.Model, policy)
		}

		addIndexedField(document, policy, SearchFieldAgent, label+".agent", llm.Role("evaluation"), entry.Agent, entry.CreatedAt)
		addIndexedField(document, policy, SearchFieldModel, label+".model", llm.Role("evaluation"), entry.Model, entry.CreatedAt)
		addIndexedField(document, policy, SearchFieldEvaluations, label, llm.Role("evaluation"), indexedEvaluationSearchText(label, entry, policy), entry.CreatedAt)
	}
}

func addArtifactFields(document *indexedSession, session Session, policy normalizedSearchIndexPolicy) {
	if policy.excludes(SearchFieldArtifacts) {
		return
	}

	for index := range session.Artifacts {
		entry := &session.Artifacts[index]

		label := fmt.Sprintf("artifacts[%d]", index+1)
		if !policy.excludes(SearchFieldAgent) {
			document.Agents = appendNormalizedUniqueIndexed(document.Agents, label+".source_agent", entry.SourceAgent, policy)
		}

		if !policy.excludes(SearchFieldRepo) {
			document.Repositories = appendNormalizedUniqueIndexed(document.Repositories, label+".worktree_path", indexRepoPathValue(label+".worktree_path", entry.WorktreePath, policy), policy)
		}

		addIndexedField(document, policy, SearchFieldAgent, label+".source_agent", llm.Role("artifact"), entry.SourceAgent, entry.CreatedAt)
		addIndexedField(document, policy, SearchFieldArtifacts, label, llm.Role("artifact"), indexedArtifactSearchText(label, entry, policy), entry.CreatedAt)
	}
}

func addMultiAgentRunFields(document *indexedSession, session Session, policy normalizedSearchIndexPolicy) {
	if policy.excludes(SearchFieldMultiAgent) {
		return
	}

	for index := range session.MultiAgentRuns {
		run := &session.MultiAgentRuns[index]
		label := fmt.Sprintf("multi_agent_runs[%d]", index+1)

		if !policy.excludes(SearchFieldAgent) {
			for _, agent := range multiAgentRunAgentValues(run) {
				document.Agents = appendNormalizedUniqueIndexed(document.Agents, label+".agent", agent, policy)
			}
		}

		if !policy.excludes(SearchFieldModel) {
			for _, model := range multiAgentRunModelValues(run) {
				document.Models = appendNormalizedUniqueIndexed(document.Models, label+".model", model, policy)
			}
		}

		addIndexedField(document, policy, SearchFieldMultiAgent, label, llm.Role("multi_agent_run"), indexedMultiAgentRunSearchText(label, run, policy), run.StartedAt)
	}
}

func addIndexedField(
	document *indexedSession,
	policy normalizedSearchIndexPolicy,
	field SearchField,
	label string,
	role llm.Role,
	value string,
	createdAt time.Time,
) {
	if policy.excludes(field) {
		return
	}

	value = sanitizeIndexFieldString(label, value, policy)
	if value == "" {
		return
	}

	if policy.excludes(SearchFieldDate) {
		createdAt = time.Time{}
	}

	document.Fields = append(document.Fields, indexedField{
		Field:     field,
		Label:     label,
		Role:      role,
		Value:     value,
		CreatedAt: createdAt,
	})
}

func transcriptExpired(session Session, policy normalizedSearchIndexPolicy, now time.Time) bool {
	if policy.maxTranscriptAge < 0 {
		return false
	}

	if policy.excludes(SearchFieldDate) {
		return true
	}

	activity := fallbackActivity(session.UpdatedAt, session.CreatedAt)
	if activity.IsZero() {
		return true
	}

	return now.Sub(activity) > policy.maxTranscriptAge
}

func indexedTranscriptExpiry(session Session, policy normalizedSearchIndexPolicy, now time.Time) time.Time {
	if len(session.Messages) == 0 ||
		policy.excludes(SearchFieldTranscript) ||
		transcriptExpired(session, policy, now) ||
		policy.maxTranscriptAge < 0 {
		return time.Time{}
	}

	activity := fallbackActivity(session.UpdatedAt, session.CreatedAt)
	if activity.IsZero() {
		return time.Time{}
	}

	return activity.Add(policy.maxTranscriptAge).UTC()
}

func searchIndexRetentionExpired(index sessionSearchIndex, now time.Time) bool {
	if index.NextTranscriptExpiry.IsZero() {
		return false
	}

	return !now.Before(index.NextTranscriptExpiry)
}

func normalizeSearchIndexPolicy(policy SearchIndexPolicy) normalizedSearchIndexPolicy {
	maxAge := policy.MaxTranscriptAge
	if maxAge == 0 {
		maxAge = defaultTranscriptRetention
	}

	normalized := normalizedSearchIndexPolicy{
		excludedFields:   make(map[SearchField]struct{}),
		sensitiveFields:  normalizeStringList(policy.SensitiveFields),
		maxTranscriptAge: maxAge,
	}

	for _, field := range policy.ExcludedFields {
		if field == "" {
			continue
		}

		normalized.excludedFields[field] = struct{}{}
	}

	return normalized
}

func (policy normalizedSearchIndexPolicy) excludes(field SearchField) bool {
	_, ok := policy.excludedFields[field]

	return ok
}

func (policy normalizedSearchIndexPolicy) fingerprint() searchIndexPolicyFingerprint {
	fields := make([]SearchField, 0, len(policy.excludedFields))
	for field := range policy.excludedFields {
		fields = append(fields, field)
	}

	slices.Sort(fields)

	return searchIndexPolicyFingerprint{
		ExcludedFields:        fields,
		SensitiveFields:       append([]string(nil), policy.sensitiveFields...),
		MaxTranscriptAgeNanos: int64(policy.maxTranscriptAge),
	}
}

func sameSearchIndexPolicy(left, right searchIndexPolicyFingerprint) bool {
	if left.MaxTranscriptAgeNanos != right.MaxTranscriptAgeNanos {
		return false
	}

	if len(left.ExcludedFields) != len(right.ExcludedFields) || len(left.SensitiveFields) != len(right.SensitiveFields) {
		return false
	}

	for i := range left.ExcludedFields {
		if left.ExcludedFields[i] != right.ExcludedFields[i] {
			return false
		}
	}

	for i := range left.SensitiveFields {
		if left.SensitiveFields[i] != right.SensitiveFields[i] {
			return false
		}
	}

	return true
}

func normalizedUniqueIndexed(label string, values []string, policy normalizedSearchIndexPolicy) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = appendNormalizedUniqueIndexed(out, label, value, policy)
	}

	return out
}

func appendNormalizedUniqueIndexed(values []string, label, value string, policy normalizedSearchIndexPolicy) []string {
	value = sanitizeIndexFieldString(label, value, policy)
	if value == "" {
		return values
	}

	key := normalizeFilterValue(value)
	for _, existing := range values {
		if normalizeFilterValue(existing) == key {
			return values
		}
	}

	return append(values, value)
}

func sanitizeIndexStrings(label string, values []string, policy normalizedSearchIndexPolicy) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = sanitizeIndexFieldString(label, value, policy)
		if value != "" {
			out = append(out, value)
		}
	}

	return out
}

func sanitizeIndexFieldString(label, value string, policy normalizedSearchIndexPolicy) string {
	return strings.TrimSpace(redactSensitiveField(label, value, policy.sensitiveFields))
}

func indexRepoPathValue(label, value string, policy normalizedSearchIndexPolicy) string {
	value = strings.TrimSpace(value)
	if strings.ContainsAny(value, `/\`) {
		value = pathBase(value)
	}

	return sanitizeIndexFieldString(label, value, policy)
}

func pathBase(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	value = strings.TrimRight(strings.ReplaceAll(value, "\\", "/"), "/")
	if value == "" || value == "." {
		return ""
	}

	base := filepath.Base(value)
	if base == "." || base == string(filepath.Separator) {
		return ""
	}

	return base
}

func indexedNegativeKnowledgeSearchText(label string, entry NegativeKnowledge, policy normalizedSearchIndexPolicy) string {
	parts := []string{
		"Failed attempt: " + sanitizeIndexFieldString(label+".approach", entry.Approach, policy),
		"Reason: " + sanitizeIndexFieldString(label+".reason", entry.Reason, policy),
	}
	if entry.Commit != "" {
		parts = append(parts, "Commit: "+sanitizeIndexFieldString(label+".commit", entry.Commit, policy))
	}

	if entry.Agent != "" && !policy.excludes(SearchFieldAgent) {
		parts = append(parts, "Agent: "+sanitizeIndexFieldString(label+".agent", entry.Agent, policy))
	}

	if entry.TaskType != "" {
		parts = append(parts, "Task Type: "+sanitizeIndexFieldString(label+".task_type", entry.TaskType, policy))
	}

	if entry.Severity != "" {
		parts = append(parts, "Severity: "+sanitizeIndexFieldString(label+".severity", entry.Severity, policy))
	}

	return strings.Join(parts, " | ")
}

func indexedEvaluationSearchText(label string, entry *AgentEvaluation, policy normalizedSearchIndexPolicy) string {
	parts := []string{"Evaluation"}
	if entry.Agent != "" && !policy.excludes(SearchFieldAgent) {
		parts[0] = "Evaluation: " + sanitizeIndexFieldString(label+".agent", entry.Agent, policy)
	}

	parts = append(parts, "Outcome: "+sanitizeIndexFieldString(label+".outcome", entry.Outcome, policy))
	if entry.Score != 0 {
		parts = append(parts, fmt.Sprintf("Score: %d", entry.Score))
	}

	if entry.Reference != "" {
		parts = append(parts, "Reference: "+sanitizeIndexFieldString(label+".reference", entry.Reference, policy))
	}

	evaluationTextFields := []struct {
		label string
		name  string
		value string
	}{
		{label: "Source", name: ".source", value: entry.Source},
		{label: "Evaluator", name: ".evaluator", value: entry.Evaluator},
		{label: "Rubric Version", name: ".rubric_version", value: entry.RubricVersion},
		{label: "Task Type", name: ".task_type", value: entry.TaskType},
		{label: "Difficulty", name: ".difficulty", value: entry.Difficulty},
		{label: "Expected Outcome", name: ".expected_outcome", value: entry.ExpectedOutcome},
		{label: "Provider", name: ".provider", value: entry.Provider},
		{label: "Fixture Version", name: ".fixture_version", value: entry.FixtureVersion},
		{label: "Agent Version", name: ".agent_version", value: entry.AgentVersion},
	}

	if entry.Model != "" && !policy.excludes(SearchFieldModel) {
		parts = append(parts, "Model: "+sanitizeIndexFieldString(label+".model", entry.Model, policy))
	}

	for _, field := range evaluationTextFields {
		if field.value != "" {
			parts = append(parts, field.label+": "+sanitizeIndexFieldString(label+field.name, field.value, policy))
		}
	}

	if entry.SchemaVersion != 0 {
		parts = append(parts, fmt.Sprintf("Schema Version: %d", entry.SchemaVersion))
	}

	parts = appendEvaluationMetricParts(parts, entry)

	if entry.Notes != "" {
		parts = append(parts, "Notes: "+sanitizeIndexFieldString(label+".notes", entry.Notes, policy))
	}

	return strings.Join(parts, " | ")
}

func appendEvaluationMetricParts(parts []string, entry *AgentEvaluation) []string {
	if entry.DurationMillis != 0 {
		parts = append(parts, fmt.Sprintf("Duration Millis: %d", entry.DurationMillis))
	}

	if entry.PassRateRecorded() {
		parts = append(parts, fmt.Sprintf("Pass Rate: %.2f", entry.PassRate))
	}

	if entry.FlakeCount != 0 {
		parts = append(parts, fmt.Sprintf("Flake Count: %d", entry.FlakeCount))
	}

	if entry.TotalTokens != 0 {
		parts = append(parts, fmt.Sprintf("Total Tokens: %d", entry.TotalTokens))
	}

	if entry.InputTokens != 0 || entry.OutputTokens != 0 {
		parts = append(parts, fmt.Sprintf("Tokens: input=%d output=%d", entry.InputTokens, entry.OutputTokens))
	}

	if entry.Cost != 0 {
		parts = append(parts, fmt.Sprintf("Cost: %.6f", entry.Cost))
	}

	if entry.Confidence != 0 {
		parts = append(parts, fmt.Sprintf("Confidence: %.2f", entry.Confidence))
	}

	return parts
}

func indexedArtifactSearchText(label string, entry *Artifact, policy normalizedSearchIndexPolicy) string {
	parts := []string{
		"Artifact: " + indexArtifactPathValue(label+".path", entry.Path, policy),
		"Kind: " + sanitizeIndexFieldString(label+".kind", entry.Kind, policy),
	}
	if entry.Summary != "" {
		parts = append(parts, "Summary: "+sanitizeIndexFieldString(label+".summary", entry.Summary, policy))
	}

	if entry.LogicalPath != "" && entry.LogicalPath != entry.Path {
		parts = append(parts, "Logical Path: "+indexArtifactPathValue(label+".logical_path", entry.LogicalPath, policy))
	}

	if entry.SourceAgent != "" && !policy.excludes(SearchFieldAgent) {
		parts = append(parts, "Source Agent: "+sanitizeIndexFieldString(label+".source_agent", entry.SourceAgent, policy))
	}

	if entry.SourceSessionID != "" && !policy.excludes(SearchFieldSession) {
		parts = append(parts, "Source Session: "+sanitizeIndexFieldString(label+".source_session_id", entry.SourceSessionID, policy))
	}

	artifactTextFields := []struct {
		label string
		name  string
		value string
	}{
		{label: "Source Command", name: ".source_command", value: entry.SourceCommand},
		{label: "Source Tool", name: ".source_tool", value: entry.SourceTool},
		{label: "Source Commit", name: ".source_commit", value: entry.SourceCommit},
		{label: "SHA256", name: ".sha256", value: entry.SHA256},
		{label: "Review Status", name: ".review_status", value: entry.ReviewStatus},
	}

	if !policy.excludes(SearchFieldRepo) {
		artifactTextFields = append(artifactTextFields,
			struct {
				label string
				name  string
				value string
			}{label: "Worktree", name: ".worktree_path", value: indexRepoPathValue(label+".worktree_path", entry.WorktreePath, policy)},
			struct {
				label string
				name  string
				value string
			}{label: "Worktree Branch", name: ".worktree_branch", value: entry.WorktreeBranch},
			struct {
				label string
				name  string
				value string
			}{label: "Worktree Base", name: ".worktree_base", value: entry.WorktreeBase},
		)
	}

	for _, field := range artifactTextFields {
		if field.value != "" {
			parts = append(parts, field.label+": "+sanitizeIndexFieldString(label+field.name, field.value, policy))
		}
	}

	return strings.Join(parts, " | ")
}

func indexArtifactPathValue(label, value string, policy normalizedSearchIndexPolicy) string {
	value = strings.TrimSpace(value)
	if isPrivatePath(value) {
		value = pathBase(value)
	}

	return sanitizeIndexFieldString(label, value, policy)
}

func isPrivatePath(value string) bool {
	return filepath.IsAbs(value) ||
		strings.HasPrefix(value, "~/") ||
		strings.HasPrefix(value, `~\`) ||
		strings.HasPrefix(value, `\\`) ||
		(len(value) >= 3 && value[1] == ':' && (value[2] == '\\' || value[2] == '/'))
}

func indexedMultiAgentRunSearchText(label string, run *MultiAgentRun, policy normalizedSearchIndexPolicy) string {
	summary := MultiAgentRunSummary{}
	if multiAgentRunHasAcceptedOutput(*run) {
		summary = run.Summary
	}

	parts := []string{
		"Multi-agent run: " + sanitizeIndexFieldString(label+".id", run.ID, policy),
		"Receipt: " + sanitizeIndexFieldString(label+".receipt_id", run.ReceiptID, policy),
		"Kind: " + sanitizeIndexFieldString(label+".kind", run.Kind, policy),
		"Status: " + string(run.Status),
		"Prompt: " + sanitizeIndexFieldString(label+".prompt", run.Prompt, policy),
		"Model: " + sanitizeIndexFieldString(label+".model", run.Model, policy),
		"Fallback models: " + sanitizeIndexFieldString(label+".fallback_models", strings.Join(run.FallbackModels, " "), policy),
		"Winner: " + sanitizeIndexFieldString(label+".summary.winner", summary.Winner, policy),
		"Reason: " + sanitizeIndexFieldString(label+".summary.reason", summary.Reason, policy),
		"Verdict reviewer: " + sanitizeIndexFieldString(label+".summary.verdict_reviewer", summary.VerdictReviewer, policy),
		"Cancellation: " + sanitizeIndexFieldString(label+".cancellation_reason", run.CancellationReason, policy),
		"Resume: " + sanitizeIndexFieldString(label+".resume_reason", run.ResumeReason, policy),
		"Error: " + sanitizeIndexFieldString(label+".error", run.Error, policy),
	}

	for index := range run.Branches {
		branch := &run.Branches[index]
		branchLabel := fmt.Sprintf("%s.branches[%d]", label, index+1)
		parts = append(parts, "Branch: "+strings.Join(nonEmptyIndexParts([]string{
			sanitizeIndexFieldString(branchLabel+".name", branch.Name, policy),
			sanitizeIndexFieldString(branchLabel+".role", branch.Role, policy),
			sanitizeIndexFieldString(branchLabel+".provenance", branch.Provenance, policy),
			sanitizeIndexFieldString(branchLabel+".model", branch.Model, policy),
			sanitizeIndexFieldString(branchLabel+".prompt_hash", branch.PromptHash, policy),
			string(branch.Status),
			sanitizeIndexFieldString(branchLabel+".error", branch.Error, policy),
			sanitizeIndexFieldString(branchLabel+".budget_rejection_rule", branch.BudgetRejectionRule, policy),
		}), " "))
	}

	for index := range run.Reviewers {
		reviewer := &run.Reviewers[index]
		reviewerLabel := fmt.Sprintf("%s.reviewers[%d]", label, index+1)
		parts = append(parts, "Reviewer: "+strings.Join(nonEmptyIndexParts([]string{
			sanitizeIndexFieldString(reviewerLabel+".name", reviewer.Name, policy),
			sanitizeIndexFieldString(reviewerLabel+".role", reviewer.Role, policy),
			sanitizeIndexFieldString(reviewerLabel+".target_agent", reviewer.TargetAgent, policy),
			sanitizeIndexFieldString(reviewerLabel+".model", reviewer.Model, policy),
			sanitizeIndexFieldString(reviewerLabel+".prompt_hash", reviewer.PromptHash, policy),
			sanitizeIndexFieldString(reviewerLabel+".call_id", reviewer.CallID, policy),
		}), " "))
	}

	for index := range run.Artifacts {
		artifact := &run.Artifacts[index]
		artifactLabel := fmt.Sprintf("%s.artifacts[%d]", label, index+1)
		parts = append(parts, "Artifact: "+strings.Join(nonEmptyIndexParts([]string{
			sanitizeIndexFieldString(artifactLabel+".kind", artifact.Kind, policy),
			sanitizeIndexFieldString(artifactLabel+".phase", artifact.Phase, policy),
			sanitizeIndexFieldString(artifactLabel+".agent", artifact.Agent, policy),
			sanitizeIndexFieldString(artifactLabel+".target_agent", artifact.TargetAgent, policy),
			sanitizeIndexFieldString(artifactLabel+".content", artifact.Content, policy),
			indexStringMapText(artifactLabel+".metadata", artifact.Metadata, policy),
		}), " "))
	}

	for index := range run.Gates {
		gate := &run.Gates[index]
		gateLabel := fmt.Sprintf("%s.gates[%d]", label, index+1)
		parts = append(parts, "Gate: "+strings.Join(nonEmptyIndexParts([]string{
			sanitizeIndexFieldString(gateLabel+".name", gate.Name, policy),
			sanitizeIndexFieldString(gateLabel+".phase", gate.Phase, policy),
			sanitizeIndexFieldString(gateLabel+".agent", gate.Agent, policy),
			sanitizeIndexFieldString(gateLabel+".notes", gate.Notes, policy),
		}), " "))
	}

	for index := range run.Decisions {
		decision := &run.Decisions[index]
		decisionLabel := fmt.Sprintf("%s.decisions[%d]", label, index+1)
		parts = append(parts, "Decision: "+strings.Join(nonEmptyIndexParts([]string{
			sanitizeIndexFieldString(decisionLabel+".kind", decision.Kind, policy),
			sanitizeIndexFieldString(decisionLabel+".phase", decision.Phase, policy),
			sanitizeIndexFieldString(decisionLabel+".agent", decision.Agent, policy),
			sanitizeIndexFieldString(decisionLabel+".target_agent", decision.TargetAgent, policy),
			sanitizeIndexFieldString(decisionLabel+".outcome", decision.Outcome, policy),
			sanitizeIndexFieldString(decisionLabel+".rationale", decision.Rationale, policy),
		}), " "))
	}

	for index := range run.Disagreements {
		disagreement := &run.Disagreements[index]
		disagreementLabel := fmt.Sprintf("%s.disagreements[%d]", label, index+1)
		parts = append(parts, "Disagreement: "+strings.Join(nonEmptyIndexParts([]string{
			sanitizeIndexFieldString(disagreementLabel+".phase", disagreement.Phase, policy),
			sanitizeIndexFieldString(disagreementLabel+".reviewer", disagreement.Reviewer, policy),
			sanitizeIndexFieldString(disagreementLabel+".target_agent", disagreement.TargetAgent, policy),
			sanitizeIndexFieldString(disagreementLabel+".subject", disagreement.Subject, policy),
			sanitizeIndexFieldString(disagreementLabel+".notes", disagreement.Notes, policy),
		}), " "))
	}

	for index := range run.Errors {
		runError := &run.Errors[index]
		errorLabel := fmt.Sprintf("%s.errors[%d]", label, index+1)
		parts = append(parts, "Workflow error: "+strings.Join(nonEmptyIndexParts([]string{
			sanitizeIndexFieldString(errorLabel+".stage", runError.Stage, policy),
			sanitizeIndexFieldString(errorLabel+".reviewer", runError.Reviewer, policy),
			sanitizeIndexFieldString(errorLabel+".target_agent", runError.TargetAgent, policy),
			sanitizeIndexFieldString(errorLabel+".message", runError.Message, policy),
		}), " "))
	}

	for index := range run.Calls {
		call := &run.Calls[index]
		callLabel := fmt.Sprintf("%s.calls[%d]", label, index+1)
		parts = append(parts, "Call: "+strings.Join(nonEmptyIndexParts([]string{
			sanitizeIndexFieldString(callLabel+".id", call.ID, policy),
			sanitizeIndexFieldString(callLabel+".phase", call.Phase, policy),
			sanitizeIndexFieldString(callLabel+".agent", call.Agent, policy),
			sanitizeIndexFieldString(callLabel+".target_agent", call.TargetAgent, policy),
			string(call.Status),
			sanitizeIndexFieldString(callLabel+".requested_model", call.RequestedModel, policy),
			sanitizeIndexFieldString(callLabel+".response_model", call.ResponseModel, policy),
			sanitizeIndexFieldString(callLabel+".fallback_models", strings.Join(call.FallbackModels, " "), policy),
			sanitizeIndexFieldString(callLabel+".prompt_hash", call.PromptHash, policy),
			sanitizeIndexFieldString(callLabel+".system_prompt", call.SystemPrompt, policy),
			sanitizeIndexFieldString(callLabel+".user_prompt", call.UserPrompt, policy),
			sanitizeIndexFieldString(callLabel+".response", call.Response, policy),
			sanitizeIndexFieldString(callLabel+".error", call.Error, policy),
			sanitizeIndexFieldString(callLabel+".budget_rejection_rule", call.BudgetRejectionRule, policy),
		}), " "))
	}

	return strings.Join(nonEmptyIndexParts(parts), " | ")
}

func multiAgentRunAgentValues(run *MultiAgentRun) []string {
	values := make([]string, 0, len(run.Branches)+len(run.Reviewers)+len(run.Artifacts)+len(run.Calls)*2)
	for i := range run.Branches {
		values = append(values, run.Branches[i].Role)
	}

	for i := range run.Reviewers {
		values = append(values, run.Reviewers[i].Name, run.Reviewers[i].Role, run.Reviewers[i].TargetAgent)
	}

	for i := range run.Artifacts {
		values = append(values, run.Artifacts[i].Agent, run.Artifacts[i].TargetAgent)
	}

	for i := range run.Calls {
		values = append(values, run.Calls[i].Agent, run.Calls[i].TargetAgent)
	}

	return values
}

func multiAgentRunModelValues(run *MultiAgentRun) []string {
	values := make([]string, 0, 1+len(run.FallbackModels)+len(run.Branches)+len(run.Reviewers)+len(run.Calls)*3)
	values = append(values, run.Model)
	values = append(values, run.FallbackModels...)

	for i := range run.Branches {
		values = append(values, run.Branches[i].Model)
	}

	for i := range run.Reviewers {
		values = append(values, run.Reviewers[i].Model)
	}

	for i := range run.Calls {
		values = append(values, run.Calls[i].RequestedModel, run.Calls[i].ResponseModel)
		values = append(values, run.Calls[i].FallbackModels...)
	}

	return values
}

func indexStringMapText(label string, values map[string]string, policy normalizedSearchIndexPolicy) string {
	if len(values) == 0 {
		return ""
	}

	parts := make([]string, 0, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}

		parts = append(parts, sanitizeIndexFieldString(label+"."+key, key+"="+strings.TrimSpace(value), policy))
	}

	sort.Strings(parts)

	return strings.Join(nonEmptyIndexParts(parts), " ")
}

func nonEmptyIndexParts(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}

	return out
}

func normalizeStringList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))

	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		key := strings.ToLower(value)
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
