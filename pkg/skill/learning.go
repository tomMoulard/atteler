//nolint:gocognit,cyclop,wsl_v5 // Learning sanitization is intentionally explicit and audit-friendly.
package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	atteval "github.com/tommoulard/atteler/pkg/eval"
	"github.com/tommoulard/atteler/pkg/events"
)

const (
	learningStateVersion = 1

	// EnvSkillLearning disables automatic skill learning when set to a false-like
	// value such as "0", "false", "off", or "no".
	EnvSkillLearning = "ATTELER_SKILL_LEARNING"
	// EnvSkillLearningDir overrides the default learning state directory.
	EnvSkillLearningDir = "ATTELER_SKILL_LEARNING_DIR"
	// EnvSkillLearningSkillDir overrides the directory where generated skills are
	// written.
	EnvSkillLearningSkillDir = "ATTELER_SKILL_LEARNING_SKILL_DIR"

	// LearningSkillStatusActive marks a generated skill as available for future updates.
	LearningSkillStatusActive = "active"
	// LearningSkillStatusDisabled prevents a generated skill from being updated.
	LearningSkillStatusDisabled = "disabled"

	kubectlContextPlaceholder     = "context"
	kubectlNamespacePlaceholder   = "namespace"
	kubectlClusterPlaceholder     = "cluster"
	kubectlPodPlaceholder         = "pod"
	kubectlContainerPlaceholder   = "container"
	kubectlServerPlaceholder      = "server"
	kubectlUserPlaceholder        = "user"
	kubectlNodePlaceholder        = "node"
	kubernetesProjectPlaceholder  = "project"
	kubernetesRegionPlaceholder   = "region"
	kubernetesProfilePlaceholder  = "profile"
	kubernetesFilterPlaceholder   = "filter"
	kubernetesGroupPlaceholder    = "group"
	kubernetesResourcePlaceholder = "resource"
	kubernetesTokenPlaceholder    = "token"
	kubernetesDeployment          = "deployment"
	kubernetesSelectorPlaceholder = "selector"
	kubernetesSecret              = "secret"
	helmReleasePlaceholder        = "release"
	helmRepositoryPlaceholder     = "repository"

	kubernetesVerbCreate    = "create"
	kubernetesVerbDelete    = "delete"
	kubernetesVerbGet       = "get"
	kubernetesVerbAnnotate  = "annotate"
	kubernetesVerbAttach    = "attach"
	kubernetesVerbAuth      = "auth"
	kubernetesVerbAutoscale = "autoscale"
	kubernetesVerbCordon    = "cordon"
	kubernetesVerbDebug     = "debug"
	kubernetesVerbDrain     = "drain"
	kubernetesVerbExec      = "exec"
	kubernetesVerbExpose    = "expose"
	kubernetesVerbLabel     = "label"
	kubernetesVerbLog       = "log"
	kubernetesVerbLogs      = "logs"
	kubernetesVerbPatch     = "patch"
	kubernetesVerbProxy     = "proxy"
	kubernetesVerbRun       = "run"
	kubernetesVerbTaint     = "taint"
	kubernetesVerbUncordon  = "uncordon"
	kubernetesVerbWait      = "wait"

	kubernetesFlagCluster    = "--cluster"
	kubernetesFlagContext    = "--context"
	kubernetesFlagKubeconfig = "--kubeconfig"
	kubernetesFlagName       = "--name"
	kubernetesFlagNamespace  = "--namespace"
	kubernetesFlagOutput     = "--output"
	kubernetesFlagUser       = "--user"
	kubernetesFlagUsername   = "--username"

	commandWrapperEnv     = "env"
	commandWrapperTimeout = "timeout"
	commandWrapperWatch   = "watch"

	redactedPlaceholder = "[REDACTED]"

	generatedSkillFileName       = "SKILL.md"
	learningCommandSourceCLI     = "cli"
	learningCommandSourceLLMTool = "llm_tool"
	learningCommandSourceSlash   = "slash"
	learningCommandSourceUser    = kubectlUserPlaceholder

	defaultLearningMaxObservations = 300
	defaultLearningSuggestionLimit = 5
	defaultSkillReferenceLimit     = 3
	defaultSkillReferenceMaxBytes  = 16 * 1024
	learningStateFile              = "state.json"
	learningStateLockFile          = "state.lock"
	// Check the generated skill root, skill directory, and common generated-skill
	// parent directories without walking up into OS-level symlinks such as
	// /var -> /private/var on macOS temp directories.
	learningReviewCreatePathSymlinkCheckDepth = 4
)

// LearningOptions controls automatic recurring-workflow observation and skill
// generation. A nil Enabled value means enabled; callers can set it to false to
// opt out without needing to write a state file.
type LearningOptions struct {
	Enabled         *bool
	StoreDir        string
	SkillDir        string
	MaxObservations int
	MaxSteps        int
	MinOccurrences  int
}

// LearningState is the durable automatic-skill-learning database. It stores
// only redacted, reusable observation summaries and generated-skill metadata;
// raw command output is intentionally not persisted.
//
//nolint:govet // JSON readability matters more than field packing for persisted state.
type LearningState struct {
	UpdatedAt    time.Time           `json:"updated_at,omitzero"`
	Version      int                 `json:"version"`
	Disabled     bool                `json:"disabled,omitempty"`
	Observations []ObservationRecord `json:"observations,omitempty"`
	Skills       []GeneratedSkill    `json:"skills,omitempty"`
}

// ObservationRecord is one sanitized action observed during normal harness use.
type ObservationRecord struct {
	ObservedAt           time.Time `json:"observed_at,omitzero"`
	EventType            string    `json:"event_type"`
	Action               string    `json:"action"`
	SequenceKey          string    `json:"sequence_key,omitempty"`
	Prompt               string    `json:"prompt,omitempty"`
	ToolClass            string    `json:"tool_class,omitempty"`
	Inputs               []string  `json:"inputs,omitempty"`
	Outputs              []string  `json:"outputs,omitempty"`
	VerificationCommands []string  `json:"verification_commands,omitempty"`
	StopConditions       []string  `json:"stop_conditions,omitempty"`
}

// GeneratedSkill records a managed skill created or updated by automatic
// learning. Users can inspect or edit SkillPath directly, or disable/delete the
// record with the management command.
//
//nolint:govet // JSON readability matters more than field packing for persisted state.
type GeneratedSkill struct {
	CreatedAt   time.Time          `json:"created_at,omitzero"`
	UpdatedAt   time.Time          `json:"updated_at,omitzero"`
	Name        string             `json:"name"`
	Slug        string             `json:"slug"`
	Status      string             `json:"status"`
	SkillPath   string             `json:"skill_path"`
	Steps       []string           `json:"steps,omitempty"`
	Occurrences int                `json:"occurrences"`
	SourceHash  string             `json:"source_hash"`
	SkillHash   string             `json:"skill_hash,omitempty"`
	Revisions   []LearningRevision `json:"revisions,omitempty"`
}

// GeneratedSkillReference is an active generated skill that matched a future
// request and can be injected as reusable context.
type GeneratedSkillReference struct {
	Slug      string
	Name      string
	Path      string
	Content   string
	Truncated bool
}

// ReferenceOptions controls lookup of generated skills for future
// requests. Unset limits use conservative defaults to keep background context
// small.
type ReferenceOptions struct {
	StoreDir string
	// SkillDir, when set, limits reference loading to generated skills under
	// the configured generated-skill directory.
	SkillDir string
	Limit    int
	MaxBytes int
}

// LearningRevision is an append-only improvement record for a generated skill.
//
//nolint:govet // JSON readability matters more than field packing for persisted state.
type LearningRevision struct {
	CreatedAt   time.Time `json:"created_at,omitzero"`
	Occurrences int       `json:"occurrences"`
	SourceHash  string    `json:"source_hash"`
	Rationale   string    `json:"rationale,omitempty"`
}

// LearningStore reads and writes the automatic-skill-learning state file.
type LearningStore struct {
	dir string
}

// Learner silently observes lifecycle events and updates the learning store.
type Learner struct {
	store *LearningStore
	opts  LearningOptions
	mu    sync.Mutex
}

// DefaultLearningOptions returns conservative local defaults. The default is
// enabled, uses .atteler/skill-learning for state, and writes generated skills
// under .atteler/skills/generated.
func DefaultLearningOptions() LearningOptions {
	return LearningOptions{
		StoreDir:        filepath.Join(".atteler", "skill-learning"),
		SkillDir:        filepath.Join(".atteler", "skills", "generated"),
		MaxObservations: defaultLearningMaxObservations,
		MaxSteps:        defaultMaxSteps,
		MinOccurrences:  defaultMinOccurrences,
	}
}

// NewLearningStore returns a state store rooted at dir, or the default learning
// directory when dir is empty.
func NewLearningStore(dir string) *LearningStore {
	opts := DefaultLearningOptions()
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = opts.StoreDir
	}

	return &LearningStore{dir: filepath.Clean(dir)}
}

// NewLearner returns a silent automatic skill learner.
func NewLearner(opts LearningOptions) *Learner {
	opts = normalizeLearningOptions(opts)

	return &Learner{store: NewLearningStore(opts.StoreDir), opts: opts}
}

// StoreDir returns the directory containing state.json.
func (s *LearningStore) StoreDir() string {
	if s == nil {
		return ""
	}

	return s.dir
}

// StatePath returns the JSON state file path.
func (s *LearningStore) StatePath() string {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return ""
	}

	return filepath.Join(s.dir, learningStateFile)
}

// Load reads the learning state. Missing state is treated as an enabled empty
// store.
func (s *LearningStore) Load() (LearningState, error) {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return LearningState{}, errors.New("skill learning: store directory is required")
	}

	if _, err := os.Stat(s.StatePath()); errors.Is(err, os.ErrNotExist) {
		return LearningState{Version: learningStateVersion}, nil
	} else if err != nil {
		return LearningState{}, fmt.Errorf("skill learning: inspect %s: %w", s.StatePath(), err)
	}

	var state LearningState
	if err := s.withLock(func() error {
		loaded, err := s.loadUnlocked()
		if err != nil {
			return err
		}

		state = loaded

		return nil
	}); err != nil {
		return LearningState{}, err
	}

	return state, nil
}

func (s *LearningStore) loadUnlocked() (LearningState, error) {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return LearningState{}, errors.New("skill learning: store directory is required")
	}

	path := s.StatePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LearningState{Version: learningStateVersion}, nil
		}

		return LearningState{}, fmt.Errorf("skill learning: read %s: %w", path, err)
	}

	var state LearningState
	if err := json.Unmarshal(data, &state); err != nil {
		return LearningState{}, fmt.Errorf("skill learning: parse %s: %w", path, err)
	}

	if state.Version == 0 {
		state.Version = learningStateVersion
	}

	return normalizeLearningState(state), nil
}

// Save writes state atomically enough for a local CLI state file.
func (s *LearningStore) Save(state LearningState) error {
	return s.withLock(func() error {
		return s.saveUnlocked(state)
	})
}

func (s *LearningStore) saveUnlocked(state LearningState) error {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return errors.New("skill learning: store directory is required")
	}

	state = normalizeLearningState(state)
	state.UpdatedAt = time.Now().UTC()

	if mkdirErr := os.MkdirAll(s.dir, 0o750); mkdirErr != nil {
		return fmt.Errorf("skill learning: create state dir: %w", mkdirErr)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("skill learning: marshal state: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("skill learning: create temp state: %w", err)
	}

	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("skill learning: write temp state: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("skill learning: close temp state: %w", err)
	}

	if err := os.Rename(tmpPath, s.StatePath()); err != nil {
		return fmt.Errorf("skill learning: replace state: %w", err)
	}

	return nil
}

// SetEnabled enables or disables all automatic skill learning in the store.
func (s *LearningStore) SetEnabled(enabled bool) error {
	return s.withLock(func() error {
		state, err := s.loadUnlocked()
		if err != nil {
			return err
		}

		state.Disabled = !enabled

		return s.saveUnlocked(state)
	})
}

// SetSkillStatus updates one generated skill status.
func (s *LearningStore) SetSkillStatus(slug, status string) (GeneratedSkill, error) {
	slug = strings.TrimSpace(slug)
	status = strings.TrimSpace(status)
	if slug == "" {
		return GeneratedSkill{}, errors.New("skill learning: slug is required")
	}
	if status != LearningSkillStatusActive && status != LearningSkillStatusDisabled {
		return GeneratedSkill{}, fmt.Errorf("skill learning: unsupported status %q", status)
	}

	var updated GeneratedSkill
	err := s.withLock(func() error {
		state, loadErr := s.loadUnlocked()
		if loadErr != nil {
			return loadErr
		}

		for i := range state.Skills {
			if state.Skills[i].Slug != slug {
				continue
			}

			state.Skills[i].Status = status
			state.Skills[i].UpdatedAt = time.Now().UTC()
			if status == LearningSkillStatusActive {
				state.Skills[i].SkillHash = currentGeneratedSkillHash(state.Skills[i])
			}
			updated = state.Skills[i]

			return s.saveUnlocked(state)
		}

		return fmt.Errorf("skill learning: generated skill %q not found", slug)
	})
	if err != nil {
		return GeneratedSkill{}, err
	}

	return updated, nil
}

func (s *LearningStore) withLock(fn func() error) (err error) {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return errors.New("skill learning: store directory is required")
	}

	if mkdirErr := os.MkdirAll(s.dir, 0o750); mkdirErr != nil {
		return fmt.Errorf("skill learning: create state dir: %w", mkdirErr)
	}

	lockPath := filepath.Join(s.dir, learningStateLockFile)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("skill learning: open state lock %s: %w", lockPath, err)
	}
	defer file.Close()

	if lockErr := lockLearningFile(file); lockErr != nil {
		return lockErr
	}

	defer func() {
		if unlockErr := unlockLearningFile(file); err == nil && unlockErr != nil {
			err = unlockErr
		}
	}()

	return fn()
}

// DeleteSkill removes one generated skill record and, when removeFiles is true,
// removes its generated skill directory as well.
func (s *LearningStore) DeleteSkill(slug string, removeFiles bool) error {
	return s.DeleteSkillInDir(slug, removeFiles, "")
}

// DeleteSkillInDir removes one generated skill record and, when removeFiles is
// true, removes its generated skill directory only if the recorded SKILL.md
// path is inside skillDir. An empty skillDir preserves DeleteSkill's legacy
// path-shape checks.
func (s *LearningStore) DeleteSkillInDir(slug string, removeFiles bool, skillDir string) error {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return errors.New("skill learning: slug is required")
	}

	return s.withLock(func() error {
		state, err := s.loadUnlocked()
		if err != nil {
			return err
		}

		index := slices.IndexFunc(state.Skills, func(candidate GeneratedSkill) bool { return candidate.Slug == slug })
		if index < 0 {
			return fmt.Errorf("skill learning: generated skill %q not found", slug)
		}

		skill := state.Skills[index]
		state.Skills = append(state.Skills[:index], state.Skills[index+1:]...)
		state.Observations = removeGeneratedSkillStepRecords(state.Observations, skill.Steps)

		if removeFiles && strings.TrimSpace(skill.SkillPath) != "" {
			if err := ValidateGeneratedSkillPath(skill, skillDir); err != nil {
				return err
			}

			dir, dirErr := generatedSkillDirectory(skill)
			if dirErr != nil {
				return dirErr
			}

			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("skill learning: delete skill directory %s: %w", dir, err)
			}
		}

		return s.saveUnlocked(state)
	})
}

func removeGeneratedSkillStepRecords(records []ObservationRecord, steps []string) []ObservationRecord {
	stepSet := make(map[string]struct{}, len(steps))
	for _, step := range steps {
		step = strings.TrimSpace(step)
		if step != "" {
			stepSet[step] = struct{}{}
		}
	}
	if len(stepSet) == 0 || len(records) == 0 {
		return records
	}

	out := make([]ObservationRecord, 0, len(records))
	pendingPrompts := make([]ObservationRecord, 0)
	lastSequenceKey := ""
	seenRecord := false

	for i := range records {
		record := records[i]
		if seenRecord && record.SequenceKey != lastSequenceKey {
			out = append(out, pendingPrompts...)
			pendingPrompts = pendingPrompts[:0]
		}
		lastSequenceKey = record.SequenceKey
		seenRecord = true

		if record.EventType == events.UserMessage || record.ToolClass == "prompt" {
			pendingPrompts = append(pendingPrompts, record)
			continue
		}

		if generatedSkillStepRecordMatches(record, stepSet) {
			pendingPrompts = pendingPrompts[:0]
			continue
		}

		out = append(out, pendingPrompts...)
		pendingPrompts = pendingPrompts[:0]
		out = append(out, record)
	}

	out = append(out, pendingPrompts...)

	return out
}

func generatedSkillStepRecordMatches(record ObservationRecord, stepSet map[string]struct{}) bool {
	action, _ := parameterizeAction(normalizeStep(record.Action))
	if _, ok := stepSet[action]; ok {
		return true
	}

	_, ok := stepSet[strings.TrimSpace(record.Action)]

	return ok
}

// ValidateGeneratedSkillPath verifies that skill points at a managed SKILL.md
// file and, when skillDir is set, that it lives under the generated-skill root.
func ValidateGeneratedSkillPath(skill GeneratedSkill, skillDir string) error {
	if _, err := generatedSkillDirectory(skill); err != nil {
		return err
	}

	skillDir = strings.TrimSpace(skillDir)
	if skillDir == "" {
		return nil
	}

	if !pathWithin(skillDir, skill.SkillPath) {
		return fmt.Errorf("skill learning: generated skill path %q is outside generated skill directory %q", skill.SkillPath, skillDir)
	}

	if err := validateGeneratedSkillRealPath(skill.SkillPath, skillDir); err != nil {
		return err
	}

	return nil
}

func validateGeneratedSkillRealPath(skillPath, skillDir string) error {
	rootReal, rootErr := evalExistingPath(skillDir)
	if rootErr != nil {
		if errors.Is(rootErr, os.ErrNotExist) {
			if _, skillErr := evalExistingPath(skillPath); errors.Is(skillErr, os.ErrNotExist) {
				return nil
			}
		}

		return fmt.Errorf("skill learning: inspect generated skill directory %q: %w", skillDir, rootErr)
	}

	skillReal, skillErr := evalExistingPath(skillPath)
	if skillErr != nil {
		if errors.Is(skillErr, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("skill learning: inspect generated skill path %q: %w", skillPath, skillErr)
	}

	if !pathWithin(rootReal, skillReal) {
		return fmt.Errorf("skill learning: generated skill path %q resolves outside generated skill directory %q", skillPath, skillDir)
	}

	return nil
}

func evalExistingPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	}

	return resolved, nil
}

// MatchingGeneratedSkills returns active generated skills that are relevant to
// prompt. Disabled stores/skills are ignored, and unreadable skill files are
// skipped so stale local state never interrupts the user's request path.
func MatchingGeneratedSkills(prompt string, opts ReferenceOptions) ([]GeneratedSkillReference, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, nil
	}

	if opts.Limit <= 0 {
		opts.Limit = defaultSkillReferenceLimit
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultSkillReferenceMaxBytes
	}

	state, err := NewLearningStore(opts.StoreDir).Load()
	if err != nil {
		return nil, err
	}
	if state.Disabled {
		return nil, nil
	}

	skills := append([]GeneratedSkill(nil), state.Skills...)
	sort.SliceStable(skills, func(i, j int) bool {
		if skills[i].Occurrences != skills[j].Occurrences {
			return skills[i].Occurrences > skills[j].Occurrences
		}
		if !skills[i].UpdatedAt.Equal(skills[j].UpdatedAt) {
			return skills[i].UpdatedAt.After(skills[j].UpdatedAt)
		}

		return skills[i].Slug < skills[j].Slug
	})

	refs := make([]GeneratedSkillReference, 0, min(opts.Limit, len(skills)))
	for i := range skills {
		skill := skills[i]
		if len(refs) >= opts.Limit {
			break
		}
		if !generatedSkillActive(skill) || !generatedSkillMatchesPrompt(skill, prompt) {
			continue
		}
		if !generatedSkillPathAllowed(skill, opts.SkillDir) {
			continue
		}

		content, truncated, readErr := readGeneratedSkillFile(skill.SkillPath, opts.MaxBytes)
		if readErr != nil || strings.TrimSpace(content) == "" {
			continue
		}

		refs = append(refs, GeneratedSkillReference{
			Slug:      skill.Slug,
			Name:      skill.Name,
			Path:      skill.SkillPath,
			Content:   content,
			Truncated: truncated,
		})
	}

	return refs, nil
}

// ObserveEvent records event when it contains a reusable, privacy-preserving
// workflow signal and may update a generated skill. Errors are returned for
// tests and direct callers; events.Runner treats observers as best-effort.
func (l *Learner) ObserveEvent(ctx context.Context, event events.Event) error {
	if l == nil || !learningOptionsEnabled(l.opts) {
		return nil
	}

	if ctx == nil {
		return errors.New("skill learning: context is required")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("skill learning: context: %w", err)
	}

	if !learningEventTypeCandidate(event.Type) {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.store.withLock(func() error {
		state, err := l.store.loadUnlocked()
		if err != nil {
			return err
		}
		if state.Disabled {
			return nil
		}

		observation, ok := ObservationFromEvent(event)
		if !ok {
			return nil
		}

		state.Observations = append(state.Observations, observation)
		state.Observations = pruneObservations(state.Observations, l.opts.MaxObservations)

		updatedState, err := l.updateGeneratedSkill(state)
		if err != nil {
			if saveErr := l.store.saveUnlocked(state); saveErr != nil {
				return errors.Join(err, saveErr)
			}

			return err
		}

		return l.store.saveUnlocked(updatedState)
	})
}

func learningEventTypeCandidate(eventType string) bool {
	switch eventType {
	case events.CommandExecute, events.ToolExecute, events.UserMessage:
		return true
	default:
		return false
	}
}

func generatedSkillActive(skill GeneratedSkill) bool {
	status := strings.TrimSpace(skill.Status)

	return status == "" || status == LearningSkillStatusActive
}

func generatedSkillMatchesPrompt(skill GeneratedSkill, prompt string) bool {
	prompt = normalizeStep(prompt)
	if prompt == "" || len(skill.Steps) == 0 {
		return false
	}

	promptTokens := tokenSet(prompt)
	if generatedSkillExplicitlyNamed(skill, prompt, promptTokens) {
		return true
	}

	if generatedSkillHasRiskyKubernetesWorkflow(skill) {
		return false
	}

	if PromptTriggers(Suggestion{Name: skill.Name, Slug: skill.Slug, Steps: skill.Steps}, prompt) {
		return true
	}

	if generatedSkillHasKubernetesInvestigationWorkflow(skill) && promptMentionsKubernetesInvestigation(promptTokens) {
		return true
	}

	return sharedWorkflowAnchorCount(skill.Steps, promptTokens) >= 2
}

func generatedSkillExplicitlyNamed(skill GeneratedSkill, prompt string, promptTokens map[string]struct{}) bool {
	if generatedSkillPhraseMatches(prompt, promptTokens, strings.ReplaceAll(skill.Slug, "-", " ")) {
		return true
	}

	name := normalizeStep(strings.TrimSuffix(skill.Name, " Skill"))

	return generatedSkillPhraseMatches(prompt, promptTokens, name)
}

func generatedSkillHasRiskyKubernetesWorkflow(skill GeneratedSkill) bool {
	hasKubernetesAnchor := false
	hasRiskyStep := false

	for _, step := range skill.Steps {
		tokens := tokenSet(step)
		if kubernetesStepLooksMutating(step) || kubernetesStepLooksSensitive(step) {
			hasRiskyStep = true
		}

		if stepMentionsKubernetesDomain(tokens) {
			hasKubernetesAnchor = true
		}
	}

	return hasKubernetesAnchor && hasRiskyStep
}

func generatedSkillHasKubernetesInvestigationWorkflow(skill GeneratedSkill) bool {
	hasKubernetesAnchor := false
	hasDiagnosticStep := false

	for _, step := range skill.Steps {
		if kubernetesStepLooksMutating(step) || kubernetesStepLooksSensitive(step) {
			return false
		}

		tokens := tokenSet(step)
		if stepMentionsKubernetesDomain(tokens) {
			hasKubernetesAnchor = true
		}
		for _, token := range []string{"describe", kubernetesVerbGet, "event", "events", kubernetesVerbLog, kubernetesVerbLogs, "status", "top"} {
			if _, ok := tokens[token]; ok {
				hasDiagnosticStep = true
			}
		}
	}

	return hasKubernetesAnchor && hasDiagnosticStep
}

func stepMentionsKubernetesDomain(tokens map[string]struct{}) bool {
	for _, token := range []string{
		"aks",
		kubectlClusterPlaceholder,
		kubectlContainerPlaceholder,
		kubernetesDeployment,
		"deployments",
		"eks",
		"helm",
		"k8s",
		"kubectl",
		"kubectx",
		"kubens",
		"kubernetes",
		kubectlNodePlaceholder,
		"nodes",
		kubectlNamespacePlaceholder,
		"namespaces",
		kubectlPodPlaceholder,
		"pods",
	} {
		if _, ok := tokens[token]; ok {
			return true
		}
	}

	return false
}

func kubernetesStepLooksSensitive(step string) bool {
	tokens := tokenSet(step)
	for _, token := range []string{"password", "passwords", kubernetesSecret, "secrets", kubernetesTokenPlaceholder, "tokens"} {
		if _, ok := tokens[token]; ok {
			return true
		}
	}

	return false
}

func kubernetesStepLooksMutating(step string) bool {
	tokens := tokenSet(step)
	for _, token := range []string{
		kubernetesVerbAnnotate,
		"apply",
		kubernetesVerbAttach,
		kubernetesVerbAutoscale,
		kubernetesVerbCordon,
		"cp",
		kubernetesVerbCreate,
		kubernetesVerbDelete,
		kubernetesVerbDrain,
		"edit",
		kubernetesVerbExec,
		kubernetesVerbExpose,
		kubernetesVerbLabel,
		kubernetesVerbPatch,
		"port",
		"port-forward",
		kubernetesVerbProxy,
		"restart",
		"scale",
		"set",
		kubernetesVerbTaint,
		kubernetesVerbUncordon,
		"upgrade",
	} {
		if _, ok := tokens[token]; ok {
			return true
		}
	}

	return kubernetesStepUsesKubectlVerb(step, kubernetesVerbDebug) ||
		kubernetesStepUsesKubectlVerb(step, kubernetesVerbRun)
}

func kubernetesStepUsesKubectlVerb(step, verb string) bool {
	fields := splitCommandFields(normalizeStep(step))
	for i := range fields {
		if tokenCoreLower(fields[i]) != "kubectl" {
			continue
		}

		for j := i + 1; j < len(fields); j++ {
			field := fields[j]
			if shellControlToken(field) {
				break
			}
			if strings.HasPrefix(field, "-") {
				if kubectlFlagConsumesValue(flagNameOnly(field)) && !strings.Contains(field, "=") {
					j++
				}
				continue
			}

			return tokenCoreLower(field) == verb
		}
	}

	return false
}

func promptMentionsKubernetesInvestigation(tokens map[string]struct{}) bool {
	if !promptMentionsKubernetes(tokens) {
		return false
	}

	for _, token := range []string{
		"crash",
		"crashloop",
		"debug",
		"diagnose",
		"error",
		"errors",
		"events",
		"failing",
		"failure",
		"incident",
		"investigate",
		kubernetesVerbLogs,
		"outage",
		"pods",
		"restarts",
		"root",
		"troubleshoot",
	} {
		if _, ok := tokens[token]; ok {
			return true
		}
	}

	return false
}

func promptMentionsKubernetes(tokens map[string]struct{}) bool {
	for _, token := range []string{
		"kubectl",
		"kubernetes",
		"k8s",
		"cluster",
		"clusters",
		"namespace",
		"namespaces",
		kubectlPodPlaceholder,
		"pods",
		kubernetesDeployment,
		"deployments",
	} {
		if _, ok := tokens[token]; ok {
			return true
		}
	}

	return false
}

func sharedWorkflowAnchorCount(steps []string, promptTokens map[string]struct{}) int {
	seen := make(map[string]struct{})
	for _, step := range steps {
		for _, token := range stepAnchorTokens(step) {
			if generatedSkillReferenceWeakToken(token) {
				continue
			}
			if _, ok := promptTokens[token]; !ok {
				continue
			}

			seen[token] = struct{}{}
		}
	}

	return len(seen)
}

func generatedSkillReferenceWeakToken(token string) bool {
	if isStopWord(token) || isPlaceholderName(token) {
		return true
	}

	switch token {
	case "", "run", "check", "inspect", "review", "summarize", "suggest", "use",
		"command", kubectlContextPlaceholder, "namespace", kubectlPodPlaceholder, kubectlContainerPlaceholder, kubectlClusterPlaceholder, kubectlUserPlaceholder,
		parameterPath, kubernetesFilterPlaceholder, kubernetesTokenPlaceholder, kubectlServerPlaceholder, kubectlNodePlaceholder, kubernetesRegionPlaceholder, kubernetesProjectPlaceholder, kubernetesProfilePlaceholder, kubernetesResourcePlaceholder, kubernetesGroupPlaceholder, helmReleasePlaceholder:
		return true
	default:
		return false
	}
}

func readGeneratedSkillFile(path string, maxBytes int) (content string, truncated bool, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false, errors.New("skill learning: generated skill path is required")
	}

	file, err := os.Open(path)
	if err != nil {
		return "", false, fmt.Errorf("skill learning: open generated skill %s: %w", path, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return "", false, fmt.Errorf("skill learning: read generated skill %s: %w", path, err)
	}

	truncated = len(data) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}

	return string(data), truncated, nil
}

// ObservationFromEvent converts lifecycle events into redacted, reusable skill
// observations. Raw command output is intentionally ignored.
func ObservationFromEvent(event events.Event) (ObservationRecord, bool) {
	switch event.Type {
	case events.CommandExecute:
		if !learningCommandEventAllowed(event) {
			return ObservationRecord{}, false
		}

		command := firstNonEmpty(event.Metadata["command"], event.Content)
		command = sanitizeCommand(command)
		if command == "" {
			return ObservationRecord{}, false
		}

		observed := event.Timestamp
		if observed.IsZero() {
			observed = time.Now().UTC()
		}

		observation := ObservationRecord{
			ObservedAt:     observed,
			EventType:      event.Type,
			Action:         "run " + command,
			SequenceKey:    learningSequenceKey(event),
			ToolClass:      "shell",
			Inputs:         []string{command},
			StopConditions: []string{"the command is unavailable, times out, or returns an error"},
		}
		if isVerificationCommand(command) {
			observation.VerificationCommands = []string{command}
		}

		return observation, true
	case events.ToolExecute:
		if !learningToolEventAllowed(event) {
			return ObservationRecord{}, false
		}

		tool := sanitizeToolName(event.Metadata["tool"])
		if tool == "" {
			return ObservationRecord{}, false
		}

		observed := event.Timestamp
		if observed.IsZero() {
			observed = time.Now().UTC()
		}

		return ObservationRecord{
			ObservedAt:     observed,
			EventType:      event.Type,
			Action:         "use tool " + tool,
			SequenceKey:    learningSequenceKey(event),
			ToolClass:      "tool",
			Inputs:         []string{tool},
			StopConditions: []string{"the tool is unavailable or returns an error"},
		}, true
	case events.UserMessage:
		prompt := summarizePromptForLearning(event.Content)
		if prompt == "" {
			return ObservationRecord{}, false
		}

		observed := event.Timestamp
		if observed.IsZero() {
			observed = time.Now().UTC()
		}

		return ObservationRecord{
			ObservedAt:  observed,
			EventType:   event.Type,
			Action:      prompt,
			SequenceKey: learningSequenceKey(event),
			ToolClass:   "prompt",
			Prompt:      prompt,
		}, true
	default:
		return ObservationRecord{}, false
	}
}

func learningSequenceKey(event events.Event) string {
	if event.Type == events.CommandExecute && strings.TrimSpace(event.Metadata["source"]) == learningCommandSourceCLI {
		return ""
	}

	sessionID := strings.TrimSpace(event.SessionID)
	if sessionID == "" {
		return ""
	}

	return hashBytes([]byte(sessionID))[:slugHashLength]
}

func learningToolEventAllowed(event events.Event) bool {
	tool := event.Metadata["tool"]
	if strings.TrimSpace(tool) == "" {
		return false
	}

	return !isInternalLearningTool(tool)
}

func isInternalLearningTool(tool string) bool {
	fields := splitCommandFields(tool)
	if len(fields) == 0 {
		return false
	}

	switch strings.ToLower(commandName(fields[0])) {
	case "llm.complete", "codex.auth.check", "codex.responses", "claude_code.auth.check",
		"claude_code.messages":
		return true
	default:
		return false
	}
}

func sanitizeToolName(tool string) string {
	tool = normalizeLearningText(tool)
	if tool == "" {
		return ""
	}

	fields := strings.Fields(tool)
	if len(fields) != 1 {
		return ""
	}

	tool = strings.ToLower(fields[0])
	if genericSensitiveFlag(tool) || !safeToolName(tool) {
		return ""
	}

	return truncateRunes(tool, 120)
}

func safeToolName(tool string) bool {
	if tool == "" {
		return false
	}

	for i, r := range tool {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		case r == '.', r == '_', r == '-', r == ':':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}

	return true
}

func learningCommandEventAllowed(event events.Event) bool {
	command := firstNonEmpty(event.Metadata["command"], event.Content)
	if isInternalLearningCommand(command) {
		return false
	}

	source := strings.TrimSpace(event.Metadata["source"])
	if source != "" {
		switch source {
		case learningCommandSourceCLI, learningCommandSourceLLMTool, learningCommandSourceSlash, learningCommandSourceUser:
			return true
		default:
			return false
		}
	}

	if strings.TrimSpace(event.Metadata["provider"]) != "" {
		return false
	}

	return true
}

func isInternalLearningCommand(command string) bool {
	fields := splitCommandFields(command)
	if len(fields) == 0 {
		return false
	}

	for i, field := range fields {
		if internalLearningCommandName(commandName(field)) && commandStartsShellSegment(fields, i) {
			return true
		}
	}

	return false
}

func internalLearningCommandName(name string) bool {
	switch name {
	case "async-run", "codex.auth.check", "codex.responses", "claude_code.auth.check",
		"claude_code.messages", "fzf", "spawn-agent":
		return true
	default:
		return false
	}
}

func (l *Learner) updateGeneratedSkill(state LearningState) (LearningState, error) {
	suggestions := learningSuggestionsFromRecords(state.Observations, Options{
		MaxSteps:       l.opts.MaxSteps,
		MinOccurrences: l.opts.MinOccurrences,
	}, defaultLearningSuggestionLimit)

	for i := range suggestions {
		suggestion := suggestions[i]
		updated, changed, err := l.applyGeneratedSkillSuggestion(state, suggestion)
		if err != nil {
			return state, err
		}
		if changed {
			return updated, nil
		}

		state = updated
	}

	return state, nil
}

func (l *Learner) applyGeneratedSkillSuggestion(state LearningState, suggestion Suggestion) (LearningState, bool, error) {
	suggestion = scrubAutomaticSuggestion(suggestion)
	hash := suggestionHash(suggestion)

	index := slices.IndexFunc(state.Skills, func(skill GeneratedSkill) bool { return skill.Slug == suggestion.Slug })
	if index >= 0 && state.Skills[index].Status == LearningSkillStatusDisabled {
		return state, false, nil
	}
	if index >= 0 && generatedSkillProtectedFromUpdate(state.Skills[index], l.opts.SkillDir) {
		return state, false, nil
	}
	if index >= 0 && state.Skills[index].SourceHash == hash {
		return state, false, nil
	}
	if index < 0 {
		exists, err := generatedSkillRootExists(l.opts.SkillDir, suggestion.Slug)
		if err != nil {
			return state, false, err
		}
		if exists {
			return state, false, nil
		}
	}

	review, err := BuildReview(l.opts.SkillDir, suggestion)
	if err != nil {
		return state, false, err
	}
	if err := writeLearningReview(review); err != nil {
		return state, false, err
	}

	skillHash := reviewSkillHash(review)

	now := time.Now().UTC()
	revision := LearningRevision{
		CreatedAt:   now,
		Occurrences: suggestion.Occurrences,
		SourceHash:  hash,
		Rationale:   suggestion.Rationale,
	}

	if index < 0 {
		state.Skills = removeSubsumedGeneratedSkills(state.Skills, suggestion, l.opts.SkillDir)
		state.Skills = append(state.Skills, GeneratedSkill{
			CreatedAt:   now,
			UpdatedAt:   now,
			Name:        suggestion.Name,
			Slug:        suggestion.Slug,
			Status:      LearningSkillStatusActive,
			SkillPath:   review.SkillPath,
			Steps:       append([]string(nil), suggestion.Steps...),
			Occurrences: suggestion.Occurrences,
			SourceHash:  hash,
			SkillHash:   skillHash,
			Revisions:   []LearningRevision{revision},
		})
	} else {
		skill := &state.Skills[index]
		if skill.CreatedAt.IsZero() {
			skill.CreatedAt = now
		}
		skill.UpdatedAt = now
		skill.Name = suggestion.Name
		skill.SkillPath = review.SkillPath
		skill.Steps = append([]string(nil), suggestion.Steps...)
		skill.Occurrences = suggestion.Occurrences
		skill.SourceHash = hash
		skill.SkillHash = skillHash
		if strings.TrimSpace(skill.Status) == "" {
			skill.Status = LearningSkillStatusActive
		}
		skill.Revisions = append(skill.Revisions, revision)
	}

	sort.SliceStable(state.Skills, func(i, j int) bool { return state.Skills[i].Slug < state.Skills[j].Slug })

	return state, true, nil
}

func learningSuggestionsFromRecords(records []ObservationRecord, opts Options, limit int) []Suggestion {
	observations := observationsForSuggestion(records)
	suggestions := make([]Suggestion, 0, min(limit, len(observations)))

	for limit <= 0 || len(suggestions) < limit {
		suggestion, ok := SuggestFromObservations(observations, opts)
		if !ok {
			break
		}

		suggestions = append(suggestions, suggestion)

		next := removeSuggestionStepObservations(observations, suggestion)
		if len(next) == len(observations) {
			break
		}

		observations = next
	}

	return suggestions
}

func removeSuggestionStepObservations(observations []Observation, suggestion Suggestion) []Observation {
	stepSet := make(map[string]struct{}, len(suggestion.Steps))
	for _, step := range suggestion.Steps {
		stepSet[step] = struct{}{}
	}
	if len(stepSet) == 0 {
		return observations
	}

	out := make([]Observation, 0, len(observations))
	for i := range observations {
		observation := observations[i]
		action, _ := parameterizeAction(normalizeStep(observation.Action))
		if _, ok := stepSet[action]; ok {
			continue
		}

		out = append(out, observation)
	}

	return out
}

func generatedSkillFileModified(skill GeneratedSkill) bool {
	storedHash := strings.TrimSpace(skill.SkillHash)
	if strings.TrimSpace(skill.SkillPath) == "" {
		return false
	}

	data, err := os.ReadFile(skill.SkillPath)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	if storedHash == "" {
		return true
	}

	return hashBytes(data) != storedHash
}

func generatedSkillProtectedFromUpdate(skill GeneratedSkill, skillDir string) bool {
	if strings.TrimSpace(skillDir) != "" {
		if err := ValidateGeneratedSkillPath(skill, skillDir); err != nil {
			return true
		}
	}

	return generatedSkillFileModified(skill)
}

func currentGeneratedSkillHash(skill GeneratedSkill) string {
	if strings.TrimSpace(skill.SkillPath) == "" {
		return ""
	}

	data, err := os.ReadFile(skill.SkillPath)
	if err != nil {
		return strings.TrimSpace(skill.SkillHash)
	}

	return hashBytes(data)
}

func reviewSkillHash(review Review) string {
	for _, file := range review.Files {
		if filepath.ToSlash(filepath.Clean(file.RelativePath)) == generatedSkillFileName {
			return hashBytes([]byte(file.Content))
		}
	}

	return ""
}

func generatedSkillRootExists(dir, slug string) (bool, error) {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(slug) == "" {
		return true, nil
	}

	path := filepath.Join(dir, slug)
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return false, fmt.Errorf("skill learning: inspect generated skill directory %s: %w", path, err)
}

func removeSubsumedGeneratedSkills(skills []GeneratedSkill, suggestion Suggestion, skillDir string) []GeneratedSkill {
	out := skills[:0]
	for i := range skills {
		skill := &skills[i]
		if skill.Status == LearningSkillStatusDisabled ||
			generatedSkillProtectedFromUpdate(*skill, skillDir) ||
			!skillSubsumedBySuggestion(skill, suggestion) {
			out = append(out, *skill)
			continue
		}

		if strings.TrimSpace(skill.SkillPath) != "" {
			if err := ValidateGeneratedSkillPath(*skill, skillDir); err != nil {
				out = append(out, *skill)
				continue
			}

			dir, err := generatedSkillDirectory(*skill)
			if err != nil {
				out = append(out, *skill)
				continue
			}

			_ = os.RemoveAll(dir)
		}
	}

	return out
}

func skillSubsumedBySuggestion(skill *GeneratedSkill, suggestion Suggestion) bool {
	if skill == nil {
		return false
	}

	if skill.Slug == suggestion.Slug {
		return false
	}
	if len(skill.Steps) > 0 && len(skill.Steps) < len(suggestion.Steps) {
		for i := range skill.Steps {
			if skill.Steps[i] != suggestion.Steps[i] {
				return false
			}
		}

		return true
	}

	return strings.HasPrefix(suggestion.Slug, skill.Slug+"-")
}

func normalizeLearningOptions(opts LearningOptions) LearningOptions {
	defaults := DefaultLearningOptions()
	opts.StoreDir = strings.TrimSpace(opts.StoreDir)
	opts.SkillDir = strings.TrimSpace(opts.SkillDir)
	if opts.StoreDir == "" {
		opts.StoreDir = defaults.StoreDir
	}
	if opts.SkillDir == "" {
		opts.SkillDir = defaults.SkillDir
	}
	if opts.MaxObservations <= 0 {
		opts.MaxObservations = defaults.MaxObservations
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = defaults.MaxSteps
	}
	if opts.MinOccurrences <= 0 {
		opts.MinOccurrences = defaults.MinOccurrences
	}

	return opts
}

func learningOptionsEnabled(opts LearningOptions) bool {
	return opts.Enabled == nil || *opts.Enabled
}

func normalizeLearningState(state LearningState) LearningState {
	if state.Version == 0 {
		state.Version = learningStateVersion
	}

	for i := range state.Skills {
		if strings.TrimSpace(state.Skills[i].Status) == "" {
			state.Skills[i].Status = LearningSkillStatusActive
		}
	}

	return state
}

func pruneObservations(observations []ObservationRecord, limit int) []ObservationRecord {
	if limit <= 0 || len(observations) <= limit {
		return observations
	}

	out := make([]ObservationRecord, limit)
	copy(out, observations[len(observations)-limit:])

	return out
}

func observationsForSuggestion(records []ObservationRecord) []Observation {
	out := make([]Observation, 0, len(records))
	var pendingPrompt string
	lastSequenceKey := ""
	seenRecord := false
	boundaryIndex := 0

	for i := range records {
		record := records[i]
		if seenRecord && record.SequenceKey != lastSequenceKey {
			pendingPrompt = ""
			out = append(out, Observation{Action: fmt.Sprintf("__atteler_sequence_boundary_%d__", boundaryIndex)})
			boundaryIndex++
		}
		lastSequenceKey = record.SequenceKey
		seenRecord = true

		if record.EventType == events.UserMessage || record.ToolClass == "prompt" {
			pendingPrompt = appendLearningPrompt(pendingPrompt, firstNonEmpty(record.Prompt, record.Action))
			continue
		}

		prompt := record.Prompt
		if pendingPrompt != "" {
			prompt = appendLearningPrompt(pendingPrompt, prompt)
			pendingPrompt = ""
		}

		out = append(out, Observation{
			Action:               record.Action,
			Prompt:               prompt,
			ToolClass:            record.ToolClass,
			Inputs:               append([]string(nil), record.Inputs...),
			Outputs:              append([]string(nil), record.Outputs...),
			VerificationCommands: append([]string(nil), record.VerificationCommands...),
			StopConditions:       append([]string(nil), record.StopConditions...),
		})
	}

	return out
}

func appendLearningPrompt(existing, next string) string {
	existing = strings.TrimSpace(existing)
	next = strings.TrimSpace(next)
	if existing == "" {
		return next
	}
	if next == "" || strings.Contains(existing, next) {
		return existing
	}

	return existing + "\n" + next
}

func scrubAutomaticSuggestion(suggestion Suggestion) Suggestion {
	for i := range suggestion.Parameters {
		suggestion.Parameters[i].Examples = nil
	}

	for i := range suggestion.Workflow {
		step := &suggestion.Workflow[i]
		step.Prompts = scrubStrings(step.Prompts)
		step.Inputs = scrubStrings(step.Inputs)
		step.Outputs = nil
		step.VerificationCommands = scrubStrings(step.VerificationCommands)
		step.StopConditions = scrubStrings(step.StopConditions)
		step.SourceActions = scrubStrings(step.SourceActions)
	}

	return suggestion
}

func scrubStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = sanitizeFreeText(value)
		if value != "" && !slices.Contains(out, value) {
			out = append(out, value)
		}
	}

	return out
}

func suggestionHash(suggestion Suggestion) string {
	var payload strings.Builder
	fmt.Fprintf(&payload, "occurrences=%d\n", suggestion.Occurrences)
	writeHashStrings(&payload, "steps", suggestion.Steps)

	for i := range suggestion.Workflow {
		step := &suggestion.Workflow[i]
		fmt.Fprintf(&payload, "workflow[%d].action=%s\n", i, step.Action)
		writeHashStrings(&payload, "source", step.SourceActions)
		writeHashStrings(&payload, "prompts", step.Prompts)
		writeHashStrings(&payload, "tools", step.ToolClasses)
		writeHashStrings(&payload, "inputs", step.Inputs)
		writeHashStrings(&payload, "verify", step.VerificationCommands)
		writeHashStrings(&payload, "stop", step.StopConditions)
	}

	sum := sha256.Sum256([]byte(payload.String()))

	return hex.EncodeToString(sum[:])
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func writeHashStrings(payload *strings.Builder, label string, values []string) {
	fmt.Fprintf(payload, "%s.len=%d\n", label, len(values))
	for i, value := range values {
		fmt.Fprintf(payload, "%s[%d]=%s\n", label, i, value)
	}
}

func writeLearningReview(review Review) error {
	if err := validateReview(review); err != nil {
		return err
	}

	if err := validateLearningReviewCreatePath(review.Root); err != nil {
		return err
	}

	if err := os.MkdirAll(review.Root, 0o750); err != nil {
		return fmt.Errorf("skill learning: create skill directory %s: %w", review.Root, err)
	}

	rootRealPath, err := validateLearningReviewRoot(review.Root)
	if err != nil {
		return err
	}

	for _, file := range review.Files {
		path := filepath.Join(review.Root, file.RelativePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("skill learning: create skill subdirectory %s: %w", filepath.Dir(path), err)
		}
		if err := validateLearningReviewWritePath(rootRealPath, path); err != nil {
			return err
		}
		if err := writeLearningReviewFile(path, file.Content, file.Mode); err != nil {
			return fmt.Errorf("skill learning: write generated file %s: %w", path, err)
		}
	}

	return nil
}

func validateLearningReviewCreatePath(root string) error {
	path := filepath.Clean(root)

	for range learningReviewCreatePathSymlinkCheckDepth {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("skill learning: refusing to update through symlinked generated skill path %s", path)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("skill learning: inspect generated skill path %s: %w", path, err)
		}

		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}

		path = parent
	}

	return nil
}

func validateLearningReviewRoot(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("skill learning: inspect generated skill directory %s: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("skill learning: refusing to update symlinked generated skill directory %s", root)
	}

	rootRealPath, err := evalExistingPath(root)
	if err != nil {
		return "", fmt.Errorf("skill learning: inspect generated skill directory %s: %w", root, err)
	}

	return rootRealPath, nil
}

func validateLearningReviewWritePath(rootRealPath, path string) error {
	dir := filepath.Dir(path)
	dirRealPath, err := evalExistingPath(dir)
	if err != nil {
		return fmt.Errorf("skill learning: inspect generated skill subdirectory %s: %w", dir, err)
	}

	if !pathWithin(rootRealPath, dirRealPath) {
		return fmt.Errorf("skill learning: generated skill write path %q resolves outside generated skill directory %q", path, rootRealPath)
	}

	return nil
}

func writeLearningReviewFile(path, content string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp generated skill file: %w", err)
	}

	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if mode != 0 {
		if err := tmp.Chmod(mode); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("chmod temp generated skill file: %w", err)
		}
	}

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp generated skill file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp generated skill file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace generated skill file: %w", err)
	}

	return nil
}

func generatedSkillDirectory(skill GeneratedSkill) (string, error) {
	slug := strings.TrimSpace(skill.Slug)
	if !validSlug.MatchString(slug) {
		return "", fmt.Errorf("skill learning: invalid generated skill slug %q", slug)
	}

	path := filepath.Clean(strings.TrimSpace(skill.SkillPath))
	if filepath.Base(path) != generatedSkillFileName {
		return "", fmt.Errorf("skill learning: refusing to delete unexpected generated skill path %q", skill.SkillPath)
	}

	dir := filepath.Clean(filepath.Dir(path))
	if dir == "." || dir == string(filepath.Separator) || filepath.Base(dir) != slug {
		return "", fmt.Errorf("skill learning: refusing to delete unexpected generated skill directory %q", dir)
	}

	return dir, nil
}

func generatedSkillPathAllowed(skill GeneratedSkill, skillDir string) bool {
	return ValidateGeneratedSkillPath(skill, skillDir) == nil
}

func sanitizeCommand(command string) string {
	command = normalizeLearningText(command)
	if command == "" {
		return ""
	}

	fields := splitCommandFields(command)
	if len(fields) == 0 {
		return ""
	}

	return truncateRunes(strings.Join(strings.Fields(strings.Join(sanitizeCommandFields(fields), " ")), " "), 200)
}

func sanitizeCommandFields(fields []string) []string {
	if len(fields) == 0 {
		return nil
	}

	if index, sanitizer, ok := firstKubernetesCommandSanitizer(fields); ok {
		return sanitizePrefixedCommand(fields, index, sanitizer.sanitize)
	}

	return sanitizeGenericFields(fields)
}

func splitCommandFields(command string) []string {
	return strings.Fields(spaceShellControlOperators(command))
}

func spaceShellControlOperators(command string) string {
	var out strings.Builder
	out.Grow(len(command) + 8)

	for i := 0; i < len(command); i++ {
		switch command[i] {
		case '&':
			if i+1 < len(command) && command[i+1] == '&' {
				out.WriteString(" && ")
				i++
			} else {
				out.WriteByte(command[i])
			}
		case '|':
			if i+1 < len(command) && command[i+1] == '|' {
				out.WriteString(" || ")
				i++
			} else {
				out.WriteString(" | ")
			}
		case ';':
			out.WriteString(" ; ")
		default:
			out.WriteByte(command[i])
		}
	}

	return out.String()
}

type commandSanitizer struct {
	sanitize func([]string) []string
	command  string
}

func kubernetesCommandSanitizers() []commandSanitizer {
	return []commandSanitizer{
		{command: "kubectl", sanitize: sanitizeKubectlFields},
		{command: "kubectx", sanitize: sanitizeKubectxFields},
		{command: "kubens", sanitize: sanitizeKubensFields},
		{command: "gcloud", sanitize: sanitizeGcloudFields},
		{command: "aws", sanitize: sanitizeAWSFields},
		{command: "az", sanitize: sanitizeAzureFields},
		{command: "curl", sanitize: sanitizeCurlFields},
		{command: "helm", sanitize: sanitizeHelmFields},
	}
}

func firstKubernetesCommandSanitizer(fields []string) (int, commandSanitizer, bool) {
	bestIndex := -1
	var best commandSanitizer

	for _, sanitizer := range kubernetesCommandSanitizers() {
		index := commandIndex(fields, sanitizer.command)
		if index < 0 {
			continue
		}
		if bestIndex < 0 || index < bestIndex {
			bestIndex = index
			best = sanitizer
		}
	}

	if bestIndex < 0 {
		return -1, commandSanitizer{}, false
	}

	return bestIndex, best, true
}

func commandIndex(fields []string, command string) int {
	for i, field := range fields {
		if commandName(field) == command && commandStartsShellSegment(fields, i) {
			return i
		}
	}

	return -1
}

func commandStartsShellSegment(fields []string, index int) bool {
	if index <= 0 {
		return true
	}

	start := 0
	for i := index - 1; i >= 0; i-- {
		if shellControlToken(fields[i]) {
			start = i + 1
			break
		}
	}
	if start == index {
		return true
	}

	seenCommandWrapper := false
	activeWrapper := ""
	remainingWrapperPositionals := 0
	for i := start; i < index; i++ {
		field := fields[i]
		if isCommandEnvironmentAssignment(field) {
			i = shellQuotedValueEnd(fields, i) - 1
			continue
		}

		name := commandName(field)
		if shellPrefixCommand(name) {
			seenCommandWrapper = true
			activeWrapper = name
			remainingWrapperPositionals = commandWrapperPositionalValues(name)
			continue
		}

		if seenCommandWrapper && strings.HasPrefix(tokenCoreLower(field), "-") {
			if commandWrapperFlagConsumesValue(activeWrapper, field) && !strings.Contains(field, "=") && i+1 < index {
				i = shellQuotedValueEnd(fields, i+1) - 1
			}
			continue
		}

		if seenCommandWrapper && remainingWrapperPositionals > 0 {
			remainingWrapperPositionals--
			i = shellQuotedValueEnd(fields, i) - 1
			continue
		}

		return false
	}

	return true
}

func isCommandEnvironmentAssignment(field string) bool {
	_, core, _ := splitToken(field)
	name, _, ok := strings.Cut(core, "=")

	return ok && strings.TrimSpace(name) != "" && !strings.HasPrefix(name, "-")
}

func shellPrefixCommand(name string) bool {
	return name == commandWrapperEnv ||
		name == "sudo" ||
		name == "doas" ||
		name == "command" ||
		name == "time" ||
		name == commandWrapperWatch ||
		name == "nohup" ||
		name == commandWrapperTimeout ||
		name == "nice" ||
		name == "stdbuf" ||
		shellStringCommand(name)
}

func shellStringCommand(name string) bool {
	switch name {
	case "bash", "sh", "zsh", "fish":
		return true
	default:
		return false
	}
}

func commandWrapperPositionalValues(wrapper string) int {
	switch wrapper {
	case commandWrapperTimeout:
		return 1
	default:
		return 0
	}
}

func commandWrapperFlagConsumesValue(wrapper, field string) bool {
	flag := strings.ToLower(flagNameOnly(tokenCoreLower(field)))

	switch wrapper {
	case "sudo":
		switch flag {
		case "-c", "--close-from", "-d", "--chdir", "-g", "--group", "-h", "--host", "-p", "--prompt", "-t", "--command-timeout", "-u", kubernetesFlagUser:
			return true
		default:
			return false
		}
	case "doas":
		return flag == "-u"
	case commandWrapperEnv:
		switch flag {
		case "-c", "--chdir", "-s", "-u", "--unset":
			return true
		default:
			return false
		}
	case "time":
		switch flag {
		case "-f", "--format", "-o", kubernetesFlagOutput:
			return true
		default:
			return false
		}
	case commandWrapperWatch:
		return flag == "-n" || flag == "--interval"
	case "nice":
		return flag == "-n" || flag == "--adjustment"
	case commandWrapperTimeout:
		return flag == "-k" || flag == "--kill-after" || flag == "-s" || flag == "--signal"
	case "stdbuf":
		switch flag {
		case "-i", "--input", "-o", kubernetesFlagOutput, "-e", "--error":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func commandName(field string) string {
	_, core, _ := splitToken(field)

	return filepath.Base(core)
}

func sanitizePrefixedCommand(fields []string, index int, sanitize func([]string) []string) []string {
	prefix := sanitizeGenericFields(fields[:index])
	sanitized := make([]string, 0, len(fields))
	sanitized = append(sanitized, prefix...)
	sanitized = append(sanitized, sanitize(fields[index:])...)

	return sanitized
}

func sanitizeFreeText(value string) string {
	value = normalizeLearningText(value)
	if value == "" {
		return ""
	}

	return truncateRunes(value, 200)
}

func normalizeLearningText(value string) string {
	return strings.Join(strings.Fields(atteval.Redact(value)), " ")
}

func sanitizeGenericFields(fields []string) []string {
	out := append([]string(nil), fields...)
	for i := 0; i < len(out); i++ {
		field := out[i]
		if sanitized, ok := sanitizeGenericAssignmentField(field); ok {
			out = replaceShellAssignmentValue(out, i, sanitized)
			continue
		}
		if strings.HasPrefix(field, "-") && genericHeaderFlag(field) && !strings.Contains(field, "=") &&
			shellValueFollows(out, i) && headerField(out[i+1]) {
			replacement := "{{" + kubernetesFilterPlaceholder + "}}"
			if sensitiveHeaderField(out[i+1]) {
				replacement = redactedPlaceholder
			}
			out = replaceHeaderValue(out, i+1, replacement)
			i++
			continue
		}
		if strings.HasPrefix(field, "-") && genericSensitiveFlag(field) && !strings.Contains(field, "=") && shellValueFollows(out, i) {
			out = replaceShellQuotedValue(out, i+1, redactedPlaceholder)
			i++
			continue
		}
		if placeholder := genericFlagPlaceholder(field); placeholder != "" && !strings.Contains(field, "=") && i+1 < len(out) && !strings.HasPrefix(out[i+1], "-") {
			out = replaceShellQuotedValue(out, i+1, "{{"+placeholder+"}}")
			i++
			continue
		}
		if strings.Contains(field, redactedPlaceholder) {
			out = replaceShellAssignmentValue(out, i, redactAssignmentValue(field))
			continue
		}

		if sanitized, ok := sanitizeBareSensitiveField(field); ok {
			out = replaceShellQuotedValue(out, i, sanitized)
			continue
		}

		out[i] = parameterizeGenericField(field)
	}

	return out
}

func sanitizeCurlFields(fields []string) []string {
	out := sanitizeGenericFields(fields)
	for i := 1; i < len(out); i++ {
		field := out[i]
		if name, value, ok := splitFlagAssignment(field); ok {
			if curlBodyFlag(name) && value != "" {
				out[i] = name + "=" + curlBodyReplacement(value)
				continue
			}
			if placeholder := curlFlagPlaceholder(name); placeholder != "" && value != "" {
				out = replaceShellAssignmentValue(out, i, name+"={{"+placeholder+"}}")
			}
			continue
		}

		if strings.HasPrefix(field, "-") && curlBodyFlag(field) && !strings.Contains(field, "=") && shellValueFollows(out, i) {
			out = replaceShellQuotedValue(out, i+1, curlBodyReplacement(out[i+1]))
			i++
			continue
		}
		if placeholder := curlFlagPlaceholder(field); placeholder != "" && !strings.Contains(field, "=") && shellValueFollows(out, i) {
			out = replaceShellQuotedValue(out, i+1, "{{"+placeholder+"}}")
			i++
			continue
		}
	}

	return sanitizeShellSuffixFields(out)
}

func curlFlagPlaceholder(field string) string {
	switch strings.ToLower(flagNameOnly(field)) {
	case "--connect-to", "--resolve":
		return kubernetesFilterPlaceholder
	default:
		return ""
	}
}

func curlBodyFlag(field string) bool {
	switch strings.ToLower(flagNameOnly(field)) {
	case "-d", "--data", "--data-ascii", "--data-binary", "--data-raw", "--data-urlencode", "-F", "--form", "--form-string":
		return true
	default:
		return false
	}
}

func curlBodyReplacement(value string) string {
	if strings.Contains(value, redactedPlaceholder) {
		return redactedPlaceholder
	}

	return "{{" + kubernetesFilterPlaceholder + "}}"
}

func genericFlagPlaceholder(field string) string {
	name := strings.ToLower(flagNameOnly(field))
	switch name {
	case "-u", kubernetesFlagUser, kubernetesFlagUsername:
		return kubectlUserPlaceholder
	case kubernetesFlagContext:
		return kubectlContextPlaceholder
	case kubernetesFlagNamespace:
		return kubectlNamespacePlaceholder
	case kubernetesFlagCluster:
		return kubectlClusterPlaceholder
	case "--host", "--server":
		return kubectlServerPlaceholder
	case "--profile":
		return kubernetesProfilePlaceholder
	case "--project":
		return kubernetesProjectPlaceholder
	default:
		return ""
	}
}

func sanitizeGenericAssignmentField(field string) (string, bool) {
	prefix, core, suffix := splitToken(field)
	name, value, ok := strings.Cut(core, "=")
	if !ok || strings.TrimSpace(name) == "" {
		return "", false
	}
	if strings.Contains(value+suffix, "{{") && strings.Contains(value+suffix, "}}") {
		return "", false
	}
	if genericSensitiveFlag(name) {
		if strings.Contains(value+suffix, redactedPlaceholder) {
			return prefix + name + "=" + redactedPlaceholder, true
		}

		return prefix + name + "=" + redactedPlaceholder + suffix, true
	}

	if genericHeaderFlag(name) && headerField(value+suffix) {
		replacement := "{{" + kubernetesFilterPlaceholder + "}}"
		if sensitiveHeaderField(value + suffix) {
			replacement = redactedPlaceholder
		}

		return prefix + name + "=" + replacement, true
	}

	if placeholder := genericAssignmentPlaceholder(name, value); placeholder != "" {
		return prefix + name + "={{" + placeholder + "}}" + suffix, true
	}

	return "", false
}

func genericHeaderFlag(field string) bool {
	switch strings.ToLower(flagNameOnly(field)) {
	case "-h", "--header":
		return true
	default:
		return false
	}
}

func headerField(field string) bool {
	_, ok := headerFieldName(field)

	return ok
}

func headerFieldName(field string) (string, bool) {
	_, core, suffix := splitToken(field)

	name, _, ok := strings.Cut(core, ":")
	if !ok {
		if !strings.Contains(suffix, ":") {
			return "", false
		}
		name = core
	}

	name = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "_", "-"))
	if name == "" {
		return "", false
	}

	return name, true
}

func sensitiveHeaderField(field string) bool {
	name, ok := headerFieldName(field)
	if !ok {
		return false
	}

	compactName := strings.ReplaceAll(name, "-", "")

	switch name {
	case "authorization", "cookie", "proxy-authorization", "set-cookie":
		return true
	default:
		return sensitiveName(name, compactName)
	}
}

func replaceHeaderValue(fields []string, start int, replacement string) []string {
	end := shellQuotedValueEnd(fields, start)
	if end <= start+1 {
		end = headerValueEnd(fields, start)
	}
	if start >= 0 && start < len(fields) {
		fields[start] = replacement
	}
	if end <= start+1 || start+1 >= len(fields) {
		return fields
	}

	return append(fields[:start+1], fields[end:]...)
}

func headerValueEnd(fields []string, start int) int {
	end := start + 1
	for end < len(fields) {
		field := fields[end]
		if shellControlToken(field) || strings.HasPrefix(field, "-") || urlLikeValue.MatchString(tokenCoreLower(field)) {
			break
		}
		end++
	}

	return end
}

func genericAssignmentPlaceholder(name, value string) string {
	name = strings.ToLower(strings.ReplaceAll(strings.TrimLeft(name, "-"), "_", "-"))

	switch {
	case strings.Contains(name, "context"):
		return kubectlContextPlaceholder
	case strings.Contains(name, "namespace"):
		return kubectlNamespacePlaceholder
	case strings.Contains(name, "cluster"):
		return kubectlClusterPlaceholder
	case strings.Contains(name, kubectlContainerPlaceholder):
		return kubectlContainerPlaceholder
	case strings.Contains(name, kubectlPodPlaceholder):
		return kubectlPodPlaceholder
	case strings.Contains(name, kubectlNodePlaceholder):
		return kubectlNodePlaceholder
	case strings.Contains(name, kubernetesDeployment):
		return kubernetesDeployment
	case strings.Contains(name, kubernetesSecret):
		return kubernetesSecret
	case strings.Contains(name, helmReleasePlaceholder):
		return helmReleasePlaceholder
	case strings.Contains(name, "server") || strings.Contains(name, "host"):
		return kubectlServerPlaceholder
	case strings.Contains(name, "user"):
		return kubectlUserPlaceholder
	case strings.Contains(name, kubernetesProfilePlaceholder) || strings.Contains(name, "account") ||
		strings.Contains(name, kubernetesProjectPlaceholder) || strings.Contains(name, "tenant") ||
		strings.Contains(name, "workspace"):
		return parameterID
	}

	switch placeholder := parameterName(value); placeholder {
	case parameterEmail, parameterID, parameterIssue, parameterPath, parameterURL:
		return placeholder
	default:
		return ""
	}
}

func genericSensitiveFlag(field string) bool {
	name := strings.TrimLeft(flagNameOnly(field), "-")
	name = strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	compactName := strings.ReplaceAll(name, "-", "")

	return sensitiveName(name, compactName)
}

func sensitiveName(name, compactName string) bool {
	return strings.Contains(name, "api-key") ||
		strings.Contains(compactName, "apikey") ||
		strings.Contains(compactName, "accesskey") ||
		strings.Contains(name, "auth-token") ||
		strings.Contains(name, "authorization") ||
		strings.Contains(name, "bearer") ||
		strings.Contains(name, "cookie") ||
		strings.Contains(name, "credential") ||
		strings.Contains(name, "password") ||
		strings.Contains(compactName, "privatekey") ||
		strings.Contains(name, kubernetesSecret) ||
		strings.Contains(name, "token")
}

func sanitizeBareSensitiveField(field string) (string, bool) {
	prefix, core, suffix := splitToken(field)
	if core == "" || strings.HasPrefix(core, "-") || strings.Contains(core, "=") {
		return "", false
	}

	name := strings.ToLower(strings.ReplaceAll(core, "_", "-"))
	compactName := strings.ReplaceAll(name, "-", "")
	if !sensitiveName(name, compactName) {
		return "", false
	}

	switch name {
	case "secret", "secrets", kubernetesTokenPlaceholder, "tokens", "password", "passwords":
		return "", false
	default:
		return prefix + redactedPlaceholder + suffix, true
	}
}

func parameterizeGenericField(field string) string {
	prefix, core, suffix := splitToken(field)
	switch name := parameterName(core); name {
	case parameterEmail, parameterID, parameterIssue, parameterPath, parameterURL:
		return prefix + "{{" + name + "}}" + suffix
	default:
		return field
	}
}

func sanitizeKubectlFields(fields []string) []string {
	out := append([]string(nil), fields...)
	tailStart := kubectlCommandTailStart(out)
	verb := ""
	for i := 1; i < len(out); i++ {
		if tailStart >= 0 && i >= tailStart {
			break
		}

		field := out[i]
		if verb == "" && !strings.HasPrefix(field, "-") && isKubectlVerb(tokenCoreLower(field)) {
			verb = tokenCoreLower(field)
		}

		if name, value, ok := splitFlagAssignment(field); ok {
			if replacement := kubectlFlagAssignmentReplacementForVerb(verb, name, value); replacement != "" {
				out = replaceShellAssignmentValue(out, i, replacement)
			}
			continue
		}

		if kubectlOutputValueNeedsPlaceholder(field, nextShellValue(out, i)) {
			out = replaceShellQuotedValue(out, i+1, "{{"+kubernetesFilterPlaceholder+"}}")
			i++
			continue
		}

		if placeholder := kubectlOptionalFlagPlaceholder(field); placeholder != "" &&
			i+1 < len(out) && !strings.HasPrefix(out[i+1], "-") && !shellControlToken(out[i+1]) {
			if strings.HasPrefix(field, "--") {
				out[i] = field + "={{" + placeholder + "}}"
				end := shellQuotedValueEnd(out, i+1)
				out = append(out[:i+1], out[end:]...)
				continue
			}

			out = replaceShellQuotedValue(out, i+1, "{{"+placeholder+"}}")
			i++
			continue
		}

		if placeholder := kubectlFlagPlaceholderForVerb(verb, field); placeholder != "" && i+1 < len(out) {
			if strings.HasPrefix(field, "--") {
				out[i] = field + "={{" + placeholder + "}}"
				end := shellQuotedValueEnd(out, i+1)
				out = append(out[:i+1], out[end:]...)
				continue
			}

			out = replaceShellQuotedValue(out, i+1, "{{"+placeholder+"}}")
			i++
			continue
		}

		if strings.HasPrefix(field, "-") && kubectlSensitiveValueFlag(field) && !strings.Contains(field, "=") && shellValueFollows(out, i) {
			out = replaceShellQuotedValue(out, i+1, redactedPlaceholder)
			i++
			continue
		}

		if strings.HasPrefix(field, "-") && genericSensitiveFlag(field) && !strings.Contains(field, "=") && shellValueFollows(out, i) {
			out = replaceShellQuotedValue(out, i+1, redactedPlaceholder)
			i++
		}
	}

	rewriteKubectlResourceNames(out)
	out = sanitizeShellSuffixFields(out)

	return out
}

func kubectlCommandTailStart(fields []string) int {
	verb := ""
	for i := 1; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return -1
		}
		if verb != "" && kubectlVerbHasCommandTail(verb) && tokenCoreLower(field) == "--" && i+1 < len(fields) {
			return i + 1
		}
		if strings.HasPrefix(field, "-") {
			if kubectlFlagConsumesValueForVerb(verb, flagNameOnly(field)) && !strings.Contains(field, "=") {
				i++
			}
			continue
		}

		if verb == "" {
			if isKubectlVerb(field) {
				verb = tokenCoreLower(field)
			}
			continue
		}

		if !kubectlVerbHasCommandTail(verb) {
			return -1
		}
	}

	return -1
}

func kubectlVerbHasCommandTail(verb string) bool {
	return verb == kubernetesVerbDebug || verb == kubernetesVerbExec || verb == kubernetesVerbRun
}

func sanitizeKubectxFields(fields []string) []string {
	return sanitizeSingleValueKubernetesCommand(fields, kubectlContextPlaceholder)
}

func sanitizeKubensFields(fields []string) []string {
	return sanitizeSingleValueKubernetesCommand(fields, kubectlNamespacePlaceholder)
}

func sanitizeGcloudFields(fields []string) []string {
	out := append([]string(nil), fields...)
	sanitizeGcloudLoggingQuery(out)
	out = sanitizeFlaggedKubernetesFields(out, gcloudFlagPlaceholder)
	rewriteGcloudClusterName(out)

	return out
}

func sanitizeAWSFields(fields []string) []string {
	serviceIndex := cliSubcommandIndex(fields, 1, awsEksFlagPlaceholder)
	if serviceIndex < 0 || tokenCoreLower(fields[serviceIndex]) != "eks" {
		return sanitizeGenericFields(fields)
	}

	return sanitizeFlaggedKubernetesFields(fields, awsEksFlagPlaceholder)
}

func sanitizeAzureFields(fields []string) []string {
	serviceIndex := cliSubcommandIndex(fields, 1, azureAKSFlagPlaceholder)
	if serviceIndex < 0 || tokenCoreLower(fields[serviceIndex]) != "aks" {
		return sanitizeGenericFields(fields)
	}

	return sanitizeFlaggedKubernetesFields(fields, azureAKSFlagPlaceholder)
}

func sanitizeHelmFields(fields []string) []string {
	out := sanitizeFlaggedKubernetesFields(fields, helmFlagPlaceholder)
	rewriteHelmRepositoryName(out)
	rewriteHelmReleaseName(out)

	return out
}

func sanitizeFlaggedKubernetesFields(fields []string, placeholderForFlag func(string) string) []string {
	out := append([]string(nil), fields...)
	for i := 1; i < len(out); i++ {
		field := out[i]
		if name, value, ok := splitFlagAssignment(field); ok {
			if replacement := flaggedKubernetesAssignmentReplacement(name, value, placeholderForFlag); replacement != "" {
				out = replaceShellAssignmentValue(out, i, replacement)
			}
			continue
		}

		if placeholder := placeholderForFlag(field); placeholder != "" && i+1 < len(out) {
			out = replaceShellQuotedValue(out, i+1, "{{"+placeholder+"}}")
			i++
			continue
		}

		if strings.HasPrefix(field, "-") && genericSensitiveFlag(field) && !strings.Contains(field, "=") && shellValueFollows(out, i) {
			out = replaceShellQuotedValue(out, i+1, redactedPlaceholder)
			i++
			continue
		}

		if sanitized, ok := sanitizeGenericAssignmentField(field); ok {
			out = replaceShellAssignmentValue(out, i, sanitized)
			continue
		}

		out[i] = parameterizeGenericField(field)
	}

	out = sanitizeShellSuffixFields(out)

	return out
}

func sanitizeShellSuffixFields(fields []string) []string {
	for i := range fields {
		if !shellControlToken(fields[i]) || i+1 >= len(fields) {
			continue
		}

		out := append([]string(nil), fields[:i+1]...)
		out = append(out, sanitizeKubernetesShellSuffixFields(fields[i+1:])...)

		return out
	}

	return fields
}

func sanitizeKubernetesShellSuffixFields(fields []string) []string {
	out := sanitizeCommandFields(fields)

	for start := 0; start < len(out); {
		end := start
		for end < len(out) && !shellControlToken(out[end]) {
			end++
		}

		sanitizeKubernetesShellSuffixSegment(out[start:end])

		start = end + 1
	}

	return out
}

func sanitizeKubernetesShellSuffixSegment(fields []string) {
	if len(fields) == 0 || !kubernetesShellFilterCommand(commandName(fields[0])) {
		return
	}

	for i := 1; i < len(fields); i++ {
		if strings.HasPrefix(tokenCoreLower(fields[i]), "-") {
			continue
		}

		fields[i] = parameterizeKubernetesFilterField(fields[i])
		end := shellQuotedValueEnd(fields, i)
		for j := i + 1; j < end; j++ {
			fields[j] = ""
		}
	}
}

func kubernetesShellFilterCommand(command string) bool {
	switch command {
	case "awk", "egrep", "fgrep", "grep", "jq", "rg", "sed", "yq":
		return true
	default:
		return false
	}
}

func parameterizeKubernetesShellSuffixField(field string) string {
	if shellControlToken(field) || strings.Contains(field, redactedPlaceholder) {
		return field
	}

	prefix, core, suffix := splitToken(field)
	if core == "" || strings.HasPrefix(core, "-") || strings.Contains(core, "=") ||
		(strings.HasPrefix(core, "{{") && strings.HasSuffix(core, "}}")) {
		return field
	}

	if resource, name, ok := strings.Cut(core, "/"); ok && singularResource(resource) != "" && strings.TrimSpace(name) != "" {
		return replaceSlashName(field, singularResource(resource))
	}

	if looksLikeKubernetesGeneratedName(core) {
		return prefix + "{{" + kubectlPodPlaceholder + "}}" + suffix
	}

	return field
}

func parameterizeKubernetesFilterField(field string) string {
	parameterized := parameterizeKubernetesShellSuffixField(field)
	if parameterized != field {
		return parameterized
	}
	if shellControlToken(field) || strings.Contains(field, redactedPlaceholder) {
		return field
	}

	prefix, core, suffix := splitToken(field)
	if core == "" || strings.HasPrefix(core, "-") ||
		(strings.HasPrefix(core, "{{") && strings.HasSuffix(core, "}}")) {
		return field
	}

	return prefix + "{{" + kubernetesFilterPlaceholder + "}}" + suffix
}

func looksLikeKubernetesGeneratedName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 4 || !strings.Contains(value, "-") {
		return false
	}

	hasLetter := false
	hasDigit := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '-':
		default:
			return false
		}
	}

	return hasLetter && hasDigit
}

func shellControlToken(field string) bool {
	switch strings.TrimSpace(field) {
	case "|", "&&", "||", ";":
		return true
	}

	switch tokenCoreLower(field) {
	case "|", "&&", "||", ";":
		return true
	default:
		return false
	}
}

func shellValueFollows(fields []string, index int) bool {
	return index+1 < len(fields) && !shellControlToken(fields[index+1])
}

func nextShellValue(fields []string, index int) string {
	if !shellValueFollows(fields, index) {
		return ""
	}

	return fields[index+1]
}

func shellQuotedValueEnd(fields []string, start int) int {
	if start < 0 || start >= len(fields) {
		return start
	}
	if shellControlToken(fields[start]) {
		return start
	}

	quote := unclosedShellQuote(fields[start])
	if quote == 0 {
		return start + 1
	}

	for end := start + 1; end < len(fields); end++ {
		if shellQuoteClosesInField(fields[end], quote) {
			return end + 1
		}
	}

	return len(fields)
}

func replaceShellQuotedValue(fields []string, start int, replacement string) []string {
	end := shellQuotedValueEnd(fields, start)
	if start >= 0 && start < len(fields) {
		fields[start] = replacement
	}
	if end <= start+1 || start+1 >= len(fields) {
		return fields
	}

	return append(fields[:start+1], fields[end:]...)
}

func replaceShellAssignmentValue(fields []string, start int, replacement string) []string {
	end := shellAssignmentValueEnd(fields, start)
	if start >= 0 && start < len(fields) {
		fields[start] = replacement
	}
	if end <= start+1 || start+1 >= len(fields) {
		return fields
	}

	return append(fields[:start+1], fields[end:]...)
}

func shellAssignmentValueEnd(fields []string, start int) int {
	if start < 0 || start >= len(fields) {
		return start
	}
	if shellControlToken(fields[start]) {
		return start
	}

	_, value, ok := strings.Cut(fields[start], "=")
	if !ok {
		return shellQuotedValueEnd(fields, start)
	}

	quote := unclosedShellQuote(value)
	if quote == 0 {
		return start + 1
	}

	for end := start + 1; end < len(fields); end++ {
		if shellQuoteClosesInField(fields[end], quote) {
			return end + 1
		}
	}

	return len(fields)
}

func unclosedShellQuote(field string) rune {
	var quote rune
	escaped := false

	for _, r := range field {
		if escaped {
			escaped = false
			continue
		}
		if quote != '\'' && r == '\\' {
			escaped = true
			continue
		}
		if r != '\'' && r != '"' {
			continue
		}

		if quote == 0 {
			quote = r
			continue
		}
		if quote == r {
			quote = 0
		}
	}

	return quote
}

func shellQuoteClosesInField(field string, quote rune) bool {
	if quote == 0 {
		return false
	}

	escaped := false
	for _, r := range field {
		if escaped {
			escaped = false
			continue
		}
		if quote != '\'' && r == '\\' {
			escaped = true
			continue
		}
		if r == quote {
			return true
		}
	}

	return false
}

func sanitizeSingleValueKubernetesCommand(fields []string, placeholder string) []string {
	out := append([]string(nil), fields...)
	for i := 1; i < len(out); i++ {
		field := out[i]
		if strings.HasPrefix(field, "-") {
			continue
		}
		if field == "" || field == "-" {
			return sanitizeShellSuffixFields(out)
		}

		out = replaceShellQuotedValue(out, i, "{{"+placeholder+"}}")
		return sanitizeShellSuffixFields(out)
	}

	return sanitizeShellSuffixFields(out)
}

func rewriteGcloudClusterName(fields []string) {
	for i := 1; i+2 < len(fields); i++ {
		if shellControlToken(fields[i]) {
			return
		}
		if tokenCoreLower(fields[i]) != kubectlContainerPlaceholder ||
			tokenCoreLower(fields[i+1]) != "clusters" ||
			tokenCoreLower(fields[i+2]) != "get-credentials" {
			continue
		}

		index := nextKubernetesPositional(fields, i+3, gcloudFlagPlaceholder)
		if index >= 0 {
			fields[index] = replaceSlashName(fields[index], kubectlClusterPlaceholder)
		}

		return
	}
}

func sanitizeGcloudLoggingQuery(fields []string) {
	commandIndex := gcloudCommandIndex(fields)
	if commandIndex < 0 || tokenCoreLower(fields[commandIndex]) != "logging" {
		return
	}

	subcommandIndex := nextGcloudPositional(fields, commandIndex+1)
	if subcommandIndex < 0 || tokenCoreLower(fields[subcommandIndex]) != "read" {
		return
	}

	for i := subcommandIndex + 1; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return
		}
		if strings.HasPrefix(tokenCoreLower(field), "-") {
			if !strings.Contains(field, "=") && (gcloudFlagPlaceholder(flagNameOnly(field)) != "" || genericSensitiveFlag(field)) {
				i++
			}
			continue
		}

		fields[i] = parameterizeGcloudLoggingQueryField(field)
		end := shellQuotedValueEnd(fields, i)
		for j := i + 1; j < end; j++ {
			fields[j] = ""
		}
	}
}

func parameterizeGcloudLoggingQueryField(field string) string {
	if shellControlToken(field) || strings.Contains(field, redactedPlaceholder) {
		return field
	}

	prefix, core, suffix := splitToken(field)
	if core == "" || strings.HasPrefix(core, "-") || (strings.HasPrefix(core, "{{") && strings.HasSuffix(core, "}}")) {
		return field
	}

	if strings.Contains(core, "{{") && strings.Contains(core, "}}") {
		return field
	}

	return prefix + "{{" + kubernetesFilterPlaceholder + "}}" + suffix
}

func gcloudCommandIndex(fields []string) int {
	for i := 1; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return -1
		}
		if strings.HasPrefix(field, "-") {
			if !strings.Contains(field, "=") && (gcloudFlagPlaceholder(flagNameOnly(field)) != "" || genericSensitiveFlag(field)) {
				i++
			}
			continue
		}

		return i
	}

	return -1
}

func nextGcloudPositional(fields []string, start int) int {
	for i := start; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return -1
		}
		if strings.HasPrefix(field, "-") {
			if !strings.Contains(field, "=") && (gcloudFlagPlaceholder(flagNameOnly(field)) != "" || genericSensitiveFlag(field)) {
				i++
			}
			continue
		}

		return i
	}

	return -1
}

func rewriteHelmReleaseName(fields []string) {
	verbIndex := helmCommandIndex(fields)
	if verbIndex < 0 {
		return
	}
	verb := tokenCoreLower(fields[verbIndex])

	releaseOffset := 1
	if verb == kubernetesVerbGet {
		releaseOffset = 2
	}

	index := verbIndex
	for range releaseOffset {
		index = nextHelmPositional(fields, index+1)
		if index < 0 {
			return
		}
	}

	switch verb {
	case kubernetesVerbDelete, kubernetesVerbGet, "history", "install", "rollback", "status", "uninstall", "upgrade":
		fields[index] = replaceSlashName(fields[index], helmReleasePlaceholder)
	}
}

func rewriteHelmRepositoryName(fields []string) {
	commandIndex := helmCommandIndex(fields)
	if commandIndex < 0 || tokenCoreLower(fields[commandIndex]) != "repo" {
		return
	}

	subcommandIndex := nextHelmPositional(fields, commandIndex+1)
	if subcommandIndex < 0 {
		return
	}

	switch tokenCoreLower(fields[subcommandIndex]) {
	case "add", "remove", "rm":
		repoIndex := nextHelmPositional(fields, subcommandIndex+1)
		if repoIndex >= 0 {
			fields[repoIndex] = replaceSlashName(fields[repoIndex], helmRepositoryPlaceholder)
		}
	case "update":
		for index := nextHelmPositional(fields, subcommandIndex+1); index >= 0; index = nextHelmPositional(fields, index+1) {
			fields[index] = replaceSlashName(fields[index], helmRepositoryPlaceholder)
		}
	}
}

func helmCommandIndex(fields []string) int {
	for i := 1; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return -1
		}
		if strings.HasPrefix(field, "-") {
			if !strings.Contains(field, "=") && (helmFlagPlaceholder(flagNameOnly(field)) != "" || genericSensitiveFlag(field)) {
				i++
			}
			continue
		}

		return i
	}

	return -1
}

func nextHelmPositional(fields []string, start int) int {
	for i := start; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return -1
		}
		if strings.HasPrefix(field, "-") {
			if !strings.Contains(field, "=") && (helmFlagPlaceholder(flagNameOnly(field)) != "" || genericSensitiveFlag(field)) {
				i++
			}
			continue
		}

		return i
	}

	return -1
}

func nextKubernetesPositional(fields []string, start int, placeholderForFlag func(string) string) int {
	for i := start; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return -1
		}
		if strings.HasPrefix(field, "-") {
			if placeholderForFlag(flagNameOnly(field)) != "" && !strings.Contains(field, "=") {
				i++
			}
			continue
		}

		return i
	}

	return -1
}

func cliSubcommandIndex(fields []string, start int, placeholderForFlag func(string) string) int {
	for i := start; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return -1
		}
		if strings.HasPrefix(field, "-") {
			if !strings.Contains(field, "=") && (placeholderForFlag(flagNameOnly(field)) != "" || genericSensitiveFlag(field)) {
				i++
			}
			continue
		}
		if _, _, ok := strings.Cut(field, "="); ok {
			continue
		}

		return i
	}

	return -1
}

func splitFlagAssignment(field string) (name, value string, ok bool) {
	if !strings.HasPrefix(field, "-") {
		return "", "", false
	}

	name, value, ok = strings.Cut(field, "=")

	return name, value, ok
}

func kubectlFlagAssignmentReplacement(name, value string) string {
	if kubectlOutputValueNeedsPlaceholder(name, value) {
		return name + "={{" + kubernetesFilterPlaceholder + "}}"
	}
	if placeholder := kubectlOptionalFlagPlaceholder(name); placeholder != "" && value != "" {
		return name + "={{" + placeholder + "}}"
	}
	if placeholder := kubectlFlagPlaceholder(name); placeholder != "" && value != "" {
		return name + "={{" + placeholder + "}}"
	}
	if genericSensitiveFlag(name) || kubectlSensitiveValueFlag(name) {
		return name + "=" + redactedPlaceholder
	}
	if placeholder := genericAssignmentPlaceholder(name, value); placeholder != "" {
		return name + "={{" + placeholder + "}}"
	}

	return ""
}

func kubectlFlagAssignmentReplacementForVerb(verb, name, value string) string {
	if kubectlFlagIsBooleanForVerb(verb, name) {
		return ""
	}
	if verb == kubernetesVerbProxy && strings.ToLower(flagNameOnly(name)) == "-p" {
		return ""
	}

	return kubectlFlagAssignmentReplacement(name, value)
}

func flaggedKubernetesAssignmentReplacement(name, value string, placeholderForFlag func(string) string) string {
	if placeholder := placeholderForFlag(name); placeholder != "" && value != "" {
		return name + "={{" + placeholder + "}}"
	}
	if genericSensitiveFlag(name) {
		return name + "=" + redactedPlaceholder
	}
	if placeholder := genericAssignmentPlaceholder(name, value); placeholder != "" {
		return name + "={{" + placeholder + "}}"
	}

	return ""
}

func gcloudFlagPlaceholder(flag string) string {
	switch strings.ToLower(flagNameOnly(flag)) {
	case kubernetesFlagCluster, kubernetesFlagName:
		return kubectlClusterPlaceholder
	case kubernetesFlagContext:
		return kubectlContextPlaceholder
	case "--location", "--region", "--zone":
		return kubernetesRegionPlaceholder
	case "--project":
		return kubernetesProjectPlaceholder
	case "--account", "--impersonate-service-account":
		return kubectlUserPlaceholder
	case "--configuration":
		return kubernetesProfilePlaceholder
	case kubernetesFlagKubeconfig:
		return parameterPath
	default:
		return ""
	}
}

func awsEksFlagPlaceholder(flag string) string {
	switch strings.ToLower(flagNameOnly(flag)) {
	case "--alias":
		return kubectlContextPlaceholder
	case kubernetesFlagName, "--cluster-name":
		return kubectlClusterPlaceholder
	case "--region":
		return kubernetesRegionPlaceholder
	case "--profile":
		return kubernetesProfilePlaceholder
	case "--role-arn":
		return kubectlUserPlaceholder
	case "--user-alias":
		return kubectlUserPlaceholder
	case kubernetesFlagKubeconfig:
		return parameterPath
	default:
		return ""
	}
}

func azureAKSFlagPlaceholder(flag string) string {
	switch strings.ToLower(flagNameOnly(flag)) {
	case kubernetesFlagName, "-n":
		return kubectlClusterPlaceholder
	case "--resource-group", "-g":
		return "resource-group"
	case "--subscription":
		return "subscription"
	case kubernetesFlagContext:
		return kubectlContextPlaceholder
	case "--file":
		return parameterPath
	default:
		return ""
	}
}

func helmFlagPlaceholder(flag string) string {
	switch strings.ToLower(flagNameOnly(flag)) {
	case "--kube-context":
		return kubectlContextPlaceholder
	case "--kube-apiserver", "--kube-tls-server-name":
		return kubectlServerPlaceholder
	case "--kube-as-group":
		return kubernetesGroupPlaceholder
	case "--kube-as-user":
		return kubectlUserPlaceholder
	case "--kube-token":
		return kubernetesTokenPlaceholder
	case kubernetesFlagNamespace, "-n":
		return kubectlNamespacePlaceholder
	case kubernetesFlagKubeconfig, "--kube-ca-file":
		return parameterPath
	case "--ca-file", "--cert-file", "--key-file", "--set-file", "--values", "-f":
		return parameterPath
	case "--registry-config", "--repository-cache", "--repository-config":
		return parameterPath
	case "--repo":
		return parameterURL
	case "--set", "--set-json", "--set-literal", "--set-string":
		return kubernetesFilterPlaceholder
	case kubernetesFlagUsername:
		return kubectlUserPlaceholder
	default:
		return ""
	}
}

func kubectlFlagPlaceholder(flag string) string {
	switch flag {
	case kubernetesFlagContext:
		return kubectlContextPlaceholder
	case "--accept-hosts":
		return kubernetesFilterPlaceholder
	case "--accept-paths":
		return kubernetesFilterPlaceholder
	case "--api-prefix":
		return kubernetesFilterPlaceholder
	case "--custom-columns":
		return kubernetesFilterPlaceholder
	case kubernetesFlagName:
		return kubernetesResourcePlaceholder
	case kubernetesFlagNamespace, "-n":
		return kubectlNamespacePlaceholder
	case kubernetesFlagKubeconfig:
		return parameterPath
	case "--cache-dir":
		return parameterPath
	case "--certificate-authority":
		return parameterPath
	case "--cert":
		return parameterPath
	case "--client-certificate":
		return parameterPath
	case "--client-key":
		return parameterPath
	case "--from-env-file":
		return parameterPath
	case "--from-file":
		return parameterPath
	case "--www":
		return parameterPath
	case "--www-prefix":
		return kubernetesFilterPlaceholder
	case "--filename", "-f":
		return parameterPath
	case "--env":
		return kubernetesFilterPlaceholder
	case "--image":
		return parameterPath
	case "--key":
		return parameterPath
	case kubernetesFlagCluster:
		return kubectlClusterPlaceholder
	case kubernetesFlagUsername:
		return kubectlUserPlaceholder
	case kubernetesFlagUser:
		return kubectlUserPlaceholder
	case "--as":
		return kubectlUserPlaceholder
	case "--as-group":
		return kubernetesGroupPlaceholder
	case "--password":
		return "password"
	case "--container", "-c":
		return kubectlContainerPlaceholder
	case "--docker-email":
		return kubectlUserPlaceholder
	case "--docker-server":
		return kubectlServerPlaceholder
	case "--docker-username":
		return kubectlUserPlaceholder
	case "--token":
		return kubernetesTokenPlaceholder
	case "--server":
		return kubectlServerPlaceholder
	case "--tls-server-name":
		return kubectlServerPlaceholder
	case "--target":
		return kubectlContainerPlaceholder
	case "--template":
		return kubernetesFilterPlaceholder
	case "--selector", "-l":
		return kubernetesSelectorPlaceholder
	case "--field-selector":
		return kubernetesSelectorPlaceholder
	case "--overrides":
		return "overrides"
	case "--patch", "-p":
		return kubernetesVerbPatch
	default:
		return ""
	}
}

func kubectlFlagPlaceholderForVerb(verb, flag string) string {
	if kubectlFlagIsBooleanForVerb(verb, flag) {
		return ""
	}
	if verb == kubernetesVerbProxy && strings.ToLower(flagNameOnly(flag)) == "-p" {
		return ""
	}

	return kubectlFlagPlaceholder(flag)
}

func kubectlOptionalFlagPlaceholder(flag string) string {
	switch strings.ToLower(flagNameOnly(flag)) {
	case "--raw":
		return parameterPath
	default:
		return ""
	}
}

func kubectlOutputValueNeedsPlaceholder(flag, value string) bool {
	if !kubectlOutputFlag(flag) || strings.TrimSpace(value) == "" {
		return false
	}

	value = strings.ToLower(strings.TrimSpace(value))
	for _, marker := range []string{"custom-columns", "go-template", "jsonpath", "template"} {
		if strings.Contains(value, marker) {
			return true
		}
	}

	return false
}

func kubectlOutputFlag(flag string) bool {
	switch strings.ToLower(flagNameOnly(flag)) {
	case kubernetesFlagOutput, "-o":
		return true
	default:
		return false
	}
}

func kubectlSensitiveValueFlag(flag string) bool {
	switch strings.ToLower(flagNameOnly(flag)) {
	case "--docker-password", "--from-literal":
		return true
	default:
		return false
	}
}

func kubectlFlagConsumesValue(flag string) bool {
	if kubectlFlagPlaceholder(flag) != "" {
		return true
	}
	if kubectlSensitiveValueFlag(flag) {
		return true
	}

	switch flag {
	case "--chunk-size", "--current-replicas", "--dry-run", "--field-manager", "--grace-period", "--image", "--limit-bytes",
		kubernetesFlagOutput, "-o", "--overrides", "--patch", "-p", "--port", "--replicas", "--request-timeout",
		"--resource-version", "--since", "--since-time", "--sort-by", "--tail", "--target-port", "--template",
		"--timeout", "--v":
		return true
	default:
		return false
	}
}

func kubectlFlagConsumesValueForVerb(verb, flag string) bool {
	if kubectlFlagIsBooleanForVerb(verb, flag) {
		return false
	}

	return kubectlFlagConsumesValue(flag)
}

func kubectlFlagIsBooleanForVerb(verb, flag string) bool {
	if verb != kubernetesVerbLog && verb != kubernetesVerbLogs {
		return false
	}

	switch strings.ToLower(flagNameOnly(flag)) {
	case "-f", "--follow", "-p", "--previous":
		return true
	default:
		return false
	}
}

func rewriteKubectlResourceNames(fields []string) {
	verb := ""
	for i := 1; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return
		}
		if strings.HasPrefix(field, "-") {
			if kubectlFlagConsumesValueForVerb(verb, flagNameOnly(field)) && !strings.Contains(field, "=") {
				i++
			}
			continue
		}

		if verb == "" {
			if isKubectlVerb(field) {
				verb = field
			}
			continue
		}

		switch verb {
		case "config":
			if rewriteKubectlConfigName(fields, i) {
				return
			}
		case kubernetesVerbAuth:
			if rewriteKubectlAuth(fields, i) {
				return
			}
		case kubernetesVerbCreate:
			if rewriteKubectlCreateName(fields, i) {
				return
			}
		case kubernetesVerbAttach, kubernetesVerbLogs, kubernetesVerbLog:
			if !strings.HasPrefix(field, "-") {
				fields[i] = replaceSlashName(field, kubectlResourcePlaceholder(field, kubectlPodPlaceholder))
				return
			}
		case kubernetesVerbExec:
			if !strings.HasPrefix(field, "-") {
				fields[i] = replaceSlashName(field, kubectlResourcePlaceholder(field, kubectlPodPlaceholder))
				sanitizeKubectlExecTail(fields, i+1)
				return
			}
		case "cp":
			rewriteKubectlCopyOperands(fields, i)
			return
		case "rollout":
			if rewriteKubectlResourceOperand(fields, i, kubectlVerbHasMetadataTail(verb)) {
				return
			}
		case "apply", "describe", kubernetesVerbGet, kubernetesVerbDelete, "edit", "scale", kubernetesVerbAutoscale, kubernetesVerbExpose, kubernetesVerbAnnotate, kubernetesVerbLabel, kubernetesVerbPatch, "port-forward", "top", kubernetesVerbWait:
			if rewriteKubectlResourceOrCustomOperand(fields, i, kubectlVerbHasMetadataTail(verb)) {
				return
			}
		case kubernetesVerbDebug:
			rewriteKubectlResourceOrFallbackOperand(fields, i, kubectlPodPlaceholder, false)
			sanitizeKubectlDebugTail(fields, i+1)
			return
		case kubernetesVerbRun:
			fields[i] = replaceSlashName(field, kubectlPodPlaceholder)
			sanitizeKubectlRunTail(fields, i+1)
			return
		case kubernetesVerbCordon, kubernetesVerbDrain, kubernetesVerbUncordon:
			fields[i] = replaceSlashName(field, kubectlNodePlaceholder)
			return
		case kubernetesVerbTaint:
			rewriteKubectlResourceOrNodeOperand(fields, i, true)
			return
		case "set":
			if rewriteKubectlSet(fields, i) {
				return
			}
		}
	}
}

func kubectlVerbHasMetadataTail(verb string) bool {
	return verb == kubernetesVerbAnnotate || verb == kubernetesVerbLabel || verb == kubernetesVerbTaint
}

func rewriteKubectlAuth(fields []string, index int) bool {
	if index >= len(fields) || tokenCoreLower(fields[index]) != "can-i" {
		return false
	}

	for i := index + 1; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return true
		}
		if strings.HasPrefix(field, "-") {
			if kubectlFlagConsumesValue(flagNameOnly(field)) && !strings.Contains(field, "=") {
				i++
			}
			continue
		}
		if kubectlAuthActionToken(field) {
			continue
		}

		return rewriteKubectlResourceOrCustomOperand(fields, i, false)
	}

	return true
}

func kubectlAuthActionToken(field string) bool {
	switch tokenCoreLower(field) {
	case "*", "bind", "create", kubernetesVerbDelete, "deletecollection", "escalate", kubernetesVerbGet,
		"impersonate", "list", kubernetesVerbPatch, "update", "use", "watch":
		return true
	default:
		return false
	}
}

func rewriteKubectlResourceOperand(fields []string, index int, sanitizeTail bool) bool {
	resource := singularResource(fields[index])
	if resource == "" {
		return false
	}
	if _, name, ok := strings.Cut(fields[index], "/"); ok && name != "" {
		fields[index] = replaceSlashName(fields[index], resource)
		if sanitizeTail {
			sanitizeKubectlMetadataTail(fields, index+1)
		}
		return true
	}

	next := nextPositional(fields, index+1)
	if next >= 0 {
		fields[next] = replaceSlashName(fields[next], resource)
		if sanitizeTail {
			sanitizeKubectlMetadataTail(fields, next+1)
		}
	}
	return true
}

func rewriteKubectlResourceOrCustomOperand(fields []string, index int, sanitizeTail bool) bool {
	if rewriteKubectlResourceOperand(fields, index, sanitizeTail) {
		return true
	}

	return rewriteKubectlCustomResourceOperand(fields, index, sanitizeTail)
}

func rewriteKubectlCustomResourceOperand(fields []string, index int, sanitizeTail bool) bool {
	prefix, core, suffix := splitToken(fields[index])
	if resource, name, ok := strings.Cut(core, "/"); ok {
		if !customKubectlResourceToken(resource) || strings.TrimSpace(name) == "" {
			return false
		}

		fields[index] = prefix + resource + "/{{" + kubernetesResourcePlaceholder + "}}" + suffix
		if sanitizeTail {
			sanitizeKubectlMetadataTail(fields, index+1)
		}

		return true
	}

	if !customKubectlResourceToken(core) {
		return false
	}

	next := nextPositional(fields, index+1)
	if next < 0 {
		return false
	}

	fields[next] = replaceSlashName(fields[next], kubernetesResourcePlaceholder)
	if sanitizeTail {
		sanitizeKubectlMetadataTail(fields, next+1)
	}

	return true
}

func customKubectlResourceToken(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || strings.HasPrefix(value, "-") ||
		strings.Contains(value, "=") ||
		strings.Contains(value, "{{") ||
		numberValue.MatchString(value) {
		return false
	}

	hasLetter := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			hasLetter = true
		case r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}

	return hasLetter
}

func rewriteKubectlResourceOrNodeOperand(fields []string, index int, sanitizeTail bool) {
	rewriteKubectlResourceOrFallbackOperand(fields, index, kubectlNodePlaceholder, sanitizeTail)
}

func rewriteKubectlResourceOrFallbackOperand(fields []string, index int, fallback string, sanitizeTail bool) {
	if rewriteKubectlResourceOperand(fields, index, sanitizeTail) {
		return
	}

	fields[index] = replaceSlashName(fields[index], fallback)
	if sanitizeTail {
		sanitizeKubectlMetadataTail(fields, index+1)
	}
}

func sanitizeKubectlMetadataTail(fields []string, start int) {
	if start < 0 || start >= len(fields) {
		return
	}

	end := start
	for end < len(fields) && !shellControlToken(fields[end]) {
		end++
	}

	tail := sanitizeGenericFields(fields[start:end])
	for i := range tail {
		tail[i] = parameterizeKubectlMetadataTailField(tail[i])
	}
	copy(fields[start:end], tail)
}

func parameterizeKubectlMetadataTailField(field string) string {
	if shellControlToken(field) {
		return field
	}
	if strings.HasPrefix(field, "{{") && strings.HasSuffix(field, "}}") {
		return field
	}

	prefix, core, suffix := splitToken(field)
	if strings.HasPrefix(core, "-") {
		return field
	}
	if core == "" {
		return field
	}
	if _, _, ok := strings.Cut(core, "="); ok {
		if strings.Contains(field, redactedPlaceholder) {
			return prefix + "{{" + kubernetesFilterPlaceholder + "}}=" + redactedPlaceholder
		}

		return prefix + "{{" + kubernetesFilterPlaceholder + "}}={{" + kubernetesFilterPlaceholder + "}}" + suffix
	}

	return parameterizeKubernetesFilterField(field)
}

func rewriteKubectlSet(fields []string, index int) bool {
	subcommand := tokenCoreLower(fields[index])
	switch subcommand {
	case commandWrapperEnv, "image", "resources", kubernetesSelectorPlaceholder, "serviceaccount", "subject":
	default:
		return false
	}

	resourceIndex := nextPositional(fields, index+1)
	if resourceIndex < 0 {
		return false
	}

	sanitizeKubectlSetTail(fields, kubectlSetAssignmentStart(fields, resourceIndex))

	return true
}

func kubectlSetAssignmentStart(fields []string, resourceIndex int) int {
	resource := singularResource(fields[resourceIndex])
	if resource == "" {
		return resourceIndex
	}

	_, core, _ := splitToken(fields[resourceIndex])
	if strings.Contains(core, "/") {
		fields[resourceIndex] = replaceSlashName(fields[resourceIndex], resource)
		return resourceIndex + 1
	}

	nameIndex := nextPositional(fields, resourceIndex+1)
	if nameIndex < 0 {
		return resourceIndex + 1
	}

	fields[nameIndex] = replaceSlashName(fields[nameIndex], resource)

	return nameIndex + 1
}

func sanitizeKubectlSetTail(fields []string, start int) {
	if start < 0 || start >= len(fields) {
		return
	}

	end := start
	for end < len(fields) && !shellControlToken(fields[end]) {
		end++
	}

	tail := sanitizeGenericFields(fields[start:end])
	for i := range tail {
		tail[i] = parameterizeKubectlSetTailField(tail[i])
	}
	replaceFieldSpan(fields, start, end, tail)
}

func parameterizeKubectlSetTailField(field string) string {
	if shellControlToken(field) || strings.Contains(field, redactedPlaceholder) {
		return field
	}
	if strings.Contains(field, "={{") && strings.Contains(field, "}}") {
		return field
	}

	prefix, core, suffix := splitToken(field)
	name, value, ok := strings.Cut(core, "=")
	if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
		return parameterizeKubernetesShellSuffixField(field)
	}

	if strings.HasPrefix(value, "{{") && strings.HasSuffix(value, "}}") {
		return field
	}

	return prefix + name + "={{" + kubernetesFilterPlaceholder + "}}" + suffix
}

func rewriteKubectlCreateName(fields []string, index int) bool {
	resource := singularResource(fields[index])
	if resource == "" {
		return false
	}

	nameStart := index + 1
	if resource == kubernetesSecret && nameStart < len(fields) {
		switch tokenCoreLower(fields[nameStart]) {
		case "docker-registry", "generic", "tls":
			nameStart++
		}
	}

	next := nextPositional(fields, nameStart)
	if next < 0 {
		return false
	}

	fields[next] = replaceSlashName(fields[next], resource)

	return true
}

func sanitizeKubectlExecTail(fields []string, start int) {
	sanitizeKubectlCommandTail(fields, start)
}

func sanitizeKubectlRunTail(fields []string, start int) {
	sanitizeKubectlCommandTail(fields, start)
}

func sanitizeKubectlDebugTail(fields []string, start int) {
	sanitizeKubectlCommandTail(fields, start)
}

func sanitizeKubectlCommandTail(fields []string, start int) {
	delimiter := -1
	for i := start; i < len(fields); i++ {
		if shellControlToken(fields[i]) {
			return
		}
		if tokenCoreLower(fields[i]) == "--" {
			delimiter = i
			break
		}
	}
	if delimiter < 0 || delimiter+1 >= len(fields) {
		return
	}

	end := len(fields)
	for i := delimiter + 1; i < len(fields); i++ {
		if shellControlToken(fields[i]) {
			end = i
			break
		}
	}

	tail := sanitizeCommandFields(fields[delimiter+1 : end])
	for i := range tail {
		tail[i] = parameterizeKubernetesShellSuffixField(tail[i])
	}
	replaceFieldSpan(fields, delimiter+1, end, tail)
}

func replaceFieldSpan(fields []string, start, end int, replacement []string) {
	if start < 0 || end < start || start > len(fields) {
		return
	}
	if end > len(fields) {
		end = len(fields)
	}

	for i := start; i < end; i++ {
		fields[i] = ""
	}
	copy(fields[start:end], replacement)
}

func rewriteKubectlCopyOperands(fields []string, start int) {
	for i := start; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return
		}
		if strings.HasPrefix(field, "-") {
			if kubectlFlagConsumesValue(flagNameOnly(field)) && !strings.Contains(field, "=") {
				i++
			}
			continue
		}

		fields[i] = parameterizeKubectlCopyOperand(field)
	}
}

func parameterizeKubectlCopyOperand(field string) string {
	prefix, core, suffix := splitToken(field)
	if core == "" || (strings.HasPrefix(core, "{{") && strings.HasSuffix(core, "}}")) {
		return field
	}

	if target, _, ok := strings.Cut(core, ":"); ok && strings.TrimSpace(target) != "" {
		if namespace, pod, hasNamespace := strings.Cut(target, "/"); hasNamespace && strings.TrimSpace(namespace) != "" && strings.TrimSpace(pod) != "" {
			return prefix + "{{" + kubectlNamespacePlaceholder + "}}/{{" + kubectlPodPlaceholder + "}}:{{" + parameterPath + "}}" + suffix
		}
		if looksLikeKubernetesGeneratedName(target) {
			return prefix + "{{" + kubectlPodPlaceholder + "}}:{{" + parameterPath + "}}" + suffix
		}

		return prefix + "{{" + kubectlPodPlaceholder + "}}:{{" + parameterPath + "}}" + suffix
	}

	return parameterizeGenericField(field)
}

func rewriteKubectlConfigName(fields []string, index int) bool {
	if index >= len(fields) {
		return false
	}

	subcommand := fields[index]
	placeholder := kubectlConfigSubcommandPlaceholder(subcommand)
	if placeholder == "" {
		return false
	}

	for i := index + 1; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return false
		}
		if strings.HasPrefix(field, "-") {
			if kubectlFlagConsumesValue(flagNameOnly(field)) && !strings.Contains(field, "=") {
				i++
			}
			continue
		}

		fields[i] = "{{" + placeholder + "}}"
		return true
	}

	return false
}

func kubectlConfigSubcommandPlaceholder(subcommand string) string {
	switch subcommand {
	case "delete-context", "rename-context", "set-context", "use-context":
		return kubectlContextPlaceholder
	case "delete-cluster", "set-cluster":
		return kubectlClusterPlaceholder
	case "delete-user", "set-credentials", "unset-credentials":
		return kubectlUserPlaceholder
	default:
		return ""
	}
}

func flagNameOnly(field string) string {
	name, _, ok := strings.Cut(field, "=")
	if ok {
		return name
	}

	return field
}

func tokenCoreLower(field string) string {
	_, core, _ := splitToken(field)

	return strings.ToLower(core)
}

func isKubectlVerb(value string) bool {
	switch value {
	case kubernetesVerbAnnotate, "apply", kubernetesVerbAttach, kubernetesVerbAuth, kubernetesVerbAutoscale, "config", kubernetesVerbCordon, "cp", kubernetesVerbCreate, kubernetesVerbDebug, kubernetesVerbDelete, "describe", kubernetesVerbDrain, "edit", kubernetesVerbExec, kubernetesVerbExpose, kubernetesVerbGet, kubernetesVerbLabel, kubernetesVerbLog, kubernetesVerbLogs, kubernetesVerbPatch, "port-forward", kubernetesVerbProxy, "rollout", kubernetesVerbRun, "scale", "set", kubernetesVerbTaint, "top", kubernetesVerbUncordon, kubernetesVerbWait:
		return true
	default:
		return false
	}
}

func singularResource(value string) string {
	_, core, _ := splitToken(value)
	value = strings.Trim(strings.ToLower(core), " ,:;")
	if value == "" || strings.HasPrefix(value, "-") {
		return ""
	}

	if prefix, name, ok := strings.Cut(value, "/"); ok {
		resource := singularResource(prefix)
		if resource == "" || name == "" {
			return ""
		}
		return resource
	}

	switch value {
	case kubectlPodPlaceholder, "pods", "po":
		return kubectlPodPlaceholder
	case kubernetesDeployment, "deployments", "deploy", "deploys":
		return kubernetesDeployment
	case "service", "services", "svc":
		return "service"
	case kubernetesSecret, "secrets":
		return kubernetesSecret
	case "configmap", "configmaps", "cm":
		return "configmap"
	case "event", "events", "ev":
		return "event"
	case "namespace", "namespaces", "ns":
		return kubectlNamespacePlaceholder
	case kubectlNodePlaceholder, "nodes", "no":
		return kubectlNodePlaceholder
	case "statefulset", "statefulsets", "sts":
		return "statefulset"
	case "daemonset", "daemonsets", "ds":
		return "daemonset"
	case "job", "jobs":
		return "job"
	case "cronjob", "cronjobs":
		return "cronjob"
	default:
		return ""
	}
}

func nextPositional(fields []string, start int) int {
	for i := start; i < len(fields); i++ {
		field := fields[i]
		if shellControlToken(field) {
			return -1
		}
		if strings.HasPrefix(field, "-") {
			if kubectlFlagConsumesValue(flagNameOnly(field)) && !strings.Contains(field, "=") {
				i++
			}
			continue
		}

		return i
	}

	return -1
}

func kubectlResourcePlaceholder(value, fallback string) string {
	if prefix, name, ok := strings.Cut(value, "/"); ok && name != "" {
		if resource := singularResource(prefix); resource != "" {
			return resource
		}
	}

	return fallback
}

func replaceSlashName(value, placeholder string) string {
	prefix, core, suffix := splitToken(value)
	if strings.HasPrefix(core, "{{") && strings.HasSuffix(core, "}}") {
		return value
	}

	if resource, name, ok := strings.Cut(core, "/"); ok && name != "" {
		if singularResource(resource) != "" {
			return prefix + resource + "/{{" + placeholder + "}}" + suffix
		}
	}

	return prefix + "{{" + placeholder + "}}" + suffix
}

func redactAssignmentValue(field string) string {
	name, _, ok := strings.Cut(field, "=")
	if ok {
		return name + "=" + redactedPlaceholder
	}

	return field
}

func isVerificationCommand(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}

	for i, field := range fields {
		name := commandName(field)
		if !commandStartsShellSegment(fields, i) {
			continue
		}
		if verificationCommandSegment(fields[i:], name) {
			return true
		}
	}

	return false
}

func verificationCommandSegment(fields []string, name string) bool {
	if len(fields) == 0 {
		return false
	}

	segmentEnd := len(fields)
	for i, field := range fields {
		if shellControlToken(field) {
			segmentEnd = i
			break
		}
	}
	fields = fields[:segmentEnd]

	switch name {
	case "go":
		return len(fields) > 1 && tokenCoreLower(fields[1]) == "test"
	case "make":
		return len(fields) > 1 && strings.Contains(tokenCoreLower(fields[1]), "test")
	case "npm", "pnpm", "yarn":
		return slices.ContainsFunc(fields[1:], func(field string) bool {
			return tokenCoreLower(field) == "test"
		})
	default:
		return false
	}
}

func summarizePromptForLearning(prompt string) string {
	prompt = strings.ToLower(sanitizeFreeText(prompt))
	if prompt == "" {
		return ""
	}

	if promptMentionsKubernetesLearningDomain(tokenSet(prompt)) {
		return "investigate kubernetes workflow"
	}

	return ""
}

func promptMentionsKubernetesLearningDomain(tokens map[string]struct{}) bool {
	for _, token := range []string{
		"helm",
		"k8s",
		"kubectl",
		"kubectx",
		"kubens",
		"kubernetes",
		kubectlContainerPlaceholder,
		"containers",
		kubernetesDeployment,
		"deployments",
		kubectlNamespacePlaceholder,
		"namespaces",
		kubectlPodPlaceholder,
		"pods",
	} {
		if _, ok := tokens[token]; ok {
			return true
		}
	}

	return false
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return value
	}

	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}

	return string(runes[:limit])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}

	return ""
}
