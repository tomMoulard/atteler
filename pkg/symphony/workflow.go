//nolint:cyclop,gocognit,goconst,gocritic,gosec,govet,misspell,modernize,wsl_v5 // Workflow config parsing deliberately keeps spec defaults and validation explicit.
package symphony

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tommoulard/atteler/pkg/shell"
)

// WorkflowSnapshot is a last-known-good workflow definition and effective
// config pair.
type WorkflowSnapshot struct {
	Definition WorkflowDefinition
	Config     Config
	ModTime    time.Time
	Size       int64
}

// WorkflowManager owns loading and lightweight polling-based dynamic reloads.
type WorkflowManager struct {
	path    string
	workDir string
	current WorkflowSnapshot
	loaded  bool
}

// NewWorkflowManager returns a loader for an explicit workflow path or the
// default WORKFLOW.md in workDir.
func NewWorkflowManager(workDir, workflowPath string) (*WorkflowManager, error) {
	if strings.TrimSpace(workDir) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("locate working directory: %w", err)
		}

		workDir = cwd
	}

	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory %q: %w", workDir, err)
	}

	workflowPath = strings.TrimSpace(workflowPath)
	if workflowPath == "" {
		workflowPath = filepath.Join(absWorkDir, DefaultWorkflowFile)
	} else if !filepath.IsAbs(workflowPath) {
		workflowPath = filepath.Join(absWorkDir, workflowPath)
	}

	workflowPath, err = filepath.Abs(workflowPath)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow path: %w", err)
	}

	return &WorkflowManager{path: workflowPath, workDir: absWorkDir}, nil
}

// Path returns the selected workflow file path.
func (m *WorkflowManager) Path() string {
	if m == nil {
		return ""
	}

	return m.path
}

// Load reads and validates the workflow file, replacing the current snapshot.
func (m *WorkflowManager) Load(ctx context.Context) (WorkflowSnapshot, error) {
	if m == nil {
		return WorkflowSnapshot{}, errors.New("workflow manager is nil")
	}

	if ctx == nil {
		return WorkflowSnapshot{}, errors.New("workflow load: context is required")
	}

	data, info, err := readWorkflowFile(m.path)
	if err != nil {
		return WorkflowSnapshot{}, err
	}

	def, err := ParseWorkflow(data)
	if err != nil {
		return WorkflowSnapshot{}, err
	}

	cfg, err := ResolveConfig(ctx, def.Config, m.path)
	if err != nil {
		return WorkflowSnapshot{}, err
	}

	snapshot := WorkflowSnapshot{
		Definition: def,
		Config:     cfg,
		ModTime:    info.ModTime(),
		Size:       info.Size(),
	}

	m.current = snapshot
	m.loaded = true

	return snapshot, nil
}

// Current returns the last known good workflow snapshot.
func (m *WorkflowManager) Current() (WorkflowSnapshot, bool) {
	if m == nil || !m.loaded {
		return WorkflowSnapshot{}, false
	}

	return m.current, true
}

// ReloadIfChanged reloads the workflow when the file metadata differs. Invalid
// reloads are returned to the caller while the previous snapshot is preserved.
func (m *WorkflowManager) ReloadIfChanged(ctx context.Context) (WorkflowSnapshot, bool, error) {
	if m == nil {
		return WorkflowSnapshot{}, false, errors.New("workflow manager is nil")
	}

	if ctx == nil {
		return WorkflowSnapshot{}, false, errors.New("workflow reload: context is required")
	}

	if !m.loaded {
		snapshot, err := m.Load(ctx)
		return snapshot, true, err
	}

	info, err := os.Stat(m.path)
	if err != nil {
		return m.current, false, &ClassedError{Class: ErrMissingWorkflowFile, Err: fmt.Errorf("read %s: %w", m.path, err)}
	}

	if info.ModTime().Equal(m.current.ModTime) && info.Size() == m.current.Size {
		return m.current, false, nil
	}

	snapshot, err := m.Load(ctx)
	if err != nil {
		return m.current, false, err
	}

	return snapshot, true, nil
}

// ParseWorkflow splits a WORKFLOW.md file into optional YAML front matter and
// a trimmed prompt template body.
func ParseWorkflow(data []byte) (WorkflowDefinition, error) {
	body := string(data)
	config := map[string]any{}

	if bytes.HasPrefix(data, []byte("---")) && startsWithFrontMatterFence(body) {
		front, rest, err := splitFrontMatter(body)
		if err != nil {
			return WorkflowDefinition{}, &ClassedError{Class: ErrWorkflowParse, Err: err}
		}

		config, err = parseFrontMatter(front)
		if err != nil {
			return WorkflowDefinition{}, err
		}

		body = rest
	}

	return WorkflowDefinition{
		Config:         config,
		PromptTemplate: strings.TrimSpace(body),
	}, nil
}

func readWorkflowFile(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, &ClassedError{Class: ErrMissingWorkflowFile, Err: fmt.Errorf("read %s: %w", path, err)}
	}

	if info.IsDir() {
		return nil, nil, &ClassedError{Class: ErrMissingWorkflowFile, Err: fmt.Errorf("%s is a directory", path)}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, &ClassedError{Class: ErrMissingWorkflowFile, Err: fmt.Errorf("read %s: %w", path, err)}
	}

	return data, info, nil
}

func startsWithFrontMatterFence(body string) bool {
	return body == "---" || strings.HasPrefix(body, "---\n") || strings.HasPrefix(body, "---\r\n")
}

func splitFrontMatter(body string) (frontMatter string, markdown string, err error) {
	lines := strings.SplitAfter(body, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", body, nil
	}

	var front strings.Builder
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return front.String(), strings.Join(lines[i+1:], ""), nil
		}

		front.WriteString(lines[i])
	}

	return "", "", errors.New("front matter opening fence has no closing fence")
}

func parseFrontMatter(frontMatter string) (map[string]any, error) {
	if strings.TrimSpace(frontMatter) == "" {
		return map[string]any{}, nil
	}

	var node yaml.Node
	if err := yaml.Unmarshal([]byte(frontMatter), &node); err != nil {
		return nil, &ClassedError{Class: ErrWorkflowParse, Err: err}
	}

	content := firstYAMLDocument(&node)
	if content == nil || content.Kind == 0 {
		return map[string]any{}, nil
	}

	if content.Kind != yaml.MappingNode {
		return nil, &ClassedError{Class: ErrWorkflowFrontMatterNotMap, Err: fmt.Errorf("front matter decoded to %s", yamlKindName(content.Kind))}
	}

	var raw map[string]any
	if err := content.Decode(&raw); err != nil {
		return nil, &ClassedError{Class: ErrWorkflowParse, Err: err}
	}

	normalized, ok := normalizeYAMLValue(raw).(map[string]any)
	if !ok {
		return nil, &ClassedError{Class: ErrWorkflowFrontMatterNotMap, Err: errors.New("front matter is not a map")}
	}

	return normalized, nil
}

func firstYAMLDocument(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}

	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return node.Content[0]
	}

	return node
}

func yamlKindName(kind yaml.Kind) string {
	switch kind {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "map"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return fmt.Sprintf("kind %d", kind)
	}
}

func normalizeYAMLValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, next := range typed {
			out[key] = normalizeYAMLValue(next)
		}

		return out
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, next := range typed {
			out[fmt.Sprint(key)] = normalizeYAMLValue(next)
		}

		return out
	case []any:
		out := make([]any, len(typed))
		for i, next := range typed {
			out[i] = normalizeYAMLValue(next)
		}

		return out
	default:
		return value
	}
}

type rawConfig struct {
	Tracker   rawTrackerConfig   `yaml:"tracker"`
	Polling   rawPollingConfig   `yaml:"polling"`
	Workspace rawWorkspaceConfig `yaml:"workspace"`
	Publish   rawPublishConfig   `yaml:"publish"`
	Debug     rawDebugConfig     `yaml:"debug"`
	Hooks     rawHooksConfig     `yaml:"hooks"`
	Agent     rawAgentConfig     `yaml:"agent"`
	Codex     rawCodexConfig     `yaml:"codex"`
}

type rawTrackerConfig struct {
	Kind           *string  `yaml:"kind"`
	Endpoint       *string  `yaml:"endpoint"`
	APIKey         *string  `yaml:"api_key"`
	ProjectSlug    *string  `yaml:"project_slug"`
	Repository     *string  `yaml:"repository"`
	Owner          *string  `yaml:"owner"`
	Repo           *string  `yaml:"repo"`
	ActiveStates   []string `yaml:"active_states"`
	TerminalStates []string `yaml:"terminal_states"`
	Labels         []string `yaml:"labels"`
}

type rawPollingConfig struct {
	IntervalMS *int `yaml:"interval_ms"`
}

type rawWorkspaceConfig struct {
	Root *string `yaml:"root"`
}

type rawPublishConfig struct {
	Enabled                *bool    `yaml:"enabled"`
	Remote                 *string  `yaml:"remote"`
	RemoteURL              *string  `yaml:"remote_url"`
	BaseBranch             *string  `yaml:"base_branch"`
	BranchPrefix           *string  `yaml:"branch_prefix"`
	Draft                  *bool    `yaml:"draft"`
	MonitorChecks          *bool    `yaml:"monitor_checks"`
	RemoveLabels           []string `yaml:"remove_labels"`
	RequiredCheckNames     []string `yaml:"required_checks"`
	RequiredCheckPatterns  []string `yaml:"required_check_patterns"`
	NoChecksPolicy         *string  `yaml:"no_checks_policy"`
	DiscoverRequiredChecks *bool    `yaml:"discover_required_checks"`
	ReworkOptionalChecks   *bool    `yaml:"rework_optional_checks"`
	GitUserName            *string  `yaml:"git_user_name"`
	GitUserEmail           *string  `yaml:"git_user_email"`
	CheckIntervalMS        *int     `yaml:"check_interval_ms"`
	MaxCheckReworkAttempts *int     `yaml:"max_check_rework_attempts"`
}

type rawDebugConfig struct {
	Enabled    *bool   `yaml:"enabled"`
	Address    *string `yaml:"address"`
	EventLimit *int    `yaml:"event_limit"`
}

type rawHooksConfig struct {
	AfterCreate  *string `yaml:"after_create"`
	BeforeRun    *string `yaml:"before_run"`
	AfterRun     *string `yaml:"after_run"`
	BeforeRemove *string `yaml:"before_remove"`
	TimeoutMS    *int    `yaml:"timeout_ms"`
}

type rawAgentConfig struct {
	MaxConcurrentAgents        *int           `yaml:"max_concurrent_agents"`
	MaxTurns                   *int           `yaml:"max_turns"`
	MaxRetryBackoffMS          *int           `yaml:"max_retry_backoff_ms"`
	MaxConcurrentAgentsByState map[string]any `yaml:"max_concurrent_agents_by_state"`
}

type rawCodexConfig struct {
	ApprovalPolicy    any     `yaml:"approval_policy"`
	ThreadSandbox     any     `yaml:"thread_sandbox"`
	TurnSandboxPolicy any     `yaml:"turn_sandbox_policy"`
	Command           *string `yaml:"command"`
	TurnTimeoutMS     *int    `yaml:"turn_timeout_ms"`
	ReadTimeoutMS     *int    `yaml:"read_timeout_ms"`
	StallTimeoutMS    *int    `yaml:"stall_timeout_ms"`
}

// ResolveConfig applies defaults, environment indirection, path normalization,
// and preflight validation to raw workflow front matter.
func ResolveConfig(ctx context.Context, config map[string]any, workflowPath string) (Config, error) {
	if ctx == nil {
		return Config{}, errors.New("resolve workflow config: context is required")
	}

	workflowPath, err := filepath.Abs(workflowPath)
	if err != nil {
		return Config{}, fmt.Errorf("resolve workflow path: %w", err)
	}

	workflowDir := filepath.Dir(workflowPath)
	var raw rawConfig
	if len(config) > 0 {
		data, err := yaml.Marshal(config)
		if err != nil {
			return Config{}, fmt.Errorf("marshal workflow config: %w", err)
		}

		if err := yaml.Unmarshal(data, &raw); err != nil {
			return Config{}, &ClassedError{Class: ErrWorkflowParse, Err: err}
		}
	}

	cfg := Config{
		WorkflowPath: workflowPath,
		WorkflowDir:  workflowDir,
		Tracker: TrackerConfig{
			Endpoint:       defaultLinearEndpoint,
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
		},
		Polling: PollingConfig{Interval: defaultPollInterval},
		Workspace: WorkspaceConfig{
			Root: filepath.Join(os.TempDir(), "symphony_workspaces"),
		},
		Publish: PublishConfig{
			Remote:                 "origin",
			BaseBranch:             "main",
			BranchPrefix:           "symphony",
			GitUserName:            "Atteler Symphony",
			GitUserEmail:           "symphony@users.noreply.github.com",
			NoChecksPolicy:         defaultNoChecksPolicy,
			DiscoverRequiredChecks: true,
		},
		Debug: DebugConfig{
			Address:    defaultDebugAddress,
			EventLimit: defaultDebugEventLimit,
		},
		Hooks: HooksConfig{Timeout: defaultHookTimeout},
		Agent: AgentConfig{
			MaxConcurrentAgents:        defaultMaxConcurrent,
			MaxTurns:                   defaultMaxTurns,
			MaxRetryBackoff:            defaultMaxRetryBackoff,
			MaxConcurrentAgentsByState: map[string]int{},
		},
		Codex: CodexConfig{
			Command:      defaultCodexCommand,
			TurnTimeout:  defaultCodexTurnTimeout,
			ReadTimeout:  defaultCodexReadTimeout,
			StallTimeout: defaultCodexStallTimeout,
			ExtraConfig:  codexExtraConfig(config),
		},
	}

	applyRawConfig(&cfg, raw)
	if err := resolveConfigValues(ctx, &cfg, workflowDir); err != nil {
		return Config{}, err
	}

	if err := cfg.ValidatePreflight(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func applyRawConfig(cfg *Config, raw rawConfig) {
	if raw.Tracker.Kind != nil {
		cfg.Tracker.Kind = strings.TrimSpace(*raw.Tracker.Kind)
	}

	if raw.Tracker.Endpoint != nil {
		cfg.Tracker.Endpoint = strings.TrimSpace(*raw.Tracker.Endpoint)
	}

	if raw.Tracker.APIKey != nil {
		cfg.Tracker.APIKey = strings.TrimSpace(*raw.Tracker.APIKey)
	}

	if raw.Tracker.ProjectSlug != nil {
		cfg.Tracker.ProjectSlug = strings.TrimSpace(*raw.Tracker.ProjectSlug)
	}

	if raw.Tracker.Repository != nil {
		cfg.Tracker.Repository = strings.TrimSpace(*raw.Tracker.Repository)
	}

	if raw.Tracker.Owner != nil {
		cfg.Tracker.Owner = strings.TrimSpace(*raw.Tracker.Owner)
	}

	if raw.Tracker.Repo != nil {
		cfg.Tracker.Repo = strings.TrimSpace(*raw.Tracker.Repo)
	}

	if len(raw.Tracker.ActiveStates) > 0 {
		cfg.Tracker.ActiveStates = trimNonEmptyStrings(raw.Tracker.ActiveStates)
	}

	if len(raw.Tracker.TerminalStates) > 0 {
		cfg.Tracker.TerminalStates = trimNonEmptyStrings(raw.Tracker.TerminalStates)
	}

	if len(raw.Tracker.Labels) > 0 {
		cfg.Tracker.Labels = trimNonEmptyStrings(raw.Tracker.Labels)
	}

	if raw.Polling.IntervalMS != nil {
		cfg.Polling.Interval = durationFromMS(*raw.Polling.IntervalMS)
	}

	if raw.Workspace.Root != nil {
		cfg.Workspace.Root = strings.TrimSpace(*raw.Workspace.Root)
	}

	if raw.Publish.Enabled != nil {
		cfg.Publish.Enabled = *raw.Publish.Enabled
	}

	if raw.Publish.Remote != nil {
		cfg.Publish.Remote = strings.TrimSpace(*raw.Publish.Remote)
	}

	if raw.Publish.RemoteURL != nil {
		cfg.Publish.RemoteURL = strings.TrimSpace(*raw.Publish.RemoteURL)
	}

	if raw.Publish.BaseBranch != nil {
		cfg.Publish.BaseBranch = strings.TrimSpace(*raw.Publish.BaseBranch)
	}

	if raw.Publish.BranchPrefix != nil {
		cfg.Publish.BranchPrefix = strings.TrimSpace(*raw.Publish.BranchPrefix)
	}

	if raw.Publish.Draft != nil {
		cfg.Publish.Draft = *raw.Publish.Draft
	}

	if raw.Publish.MonitorChecks != nil {
		cfg.Publish.MonitorChecks = *raw.Publish.MonitorChecks
	}

	if len(raw.Publish.RemoveLabels) > 0 {
		cfg.Publish.RemoveLabels = trimNonEmptyStrings(raw.Publish.RemoveLabels)
	}

	if len(raw.Publish.RequiredCheckNames) > 0 {
		cfg.Publish.RequiredCheckNames = trimNonEmptyStrings(raw.Publish.RequiredCheckNames)
	}

	if len(raw.Publish.RequiredCheckPatterns) > 0 {
		cfg.Publish.RequiredCheckPatterns = trimNonEmptyStrings(raw.Publish.RequiredCheckPatterns)
	}

	if raw.Publish.NoChecksPolicy != nil {
		cfg.Publish.NoChecksPolicy = PullRequestNoChecksPolicy(strings.ToLower(strings.TrimSpace(*raw.Publish.NoChecksPolicy)))
	}

	if raw.Publish.DiscoverRequiredChecks != nil {
		cfg.Publish.DiscoverRequiredChecks = *raw.Publish.DiscoverRequiredChecks
	}

	if raw.Publish.ReworkOptionalChecks != nil {
		cfg.Publish.ReworkOptionalChecks = *raw.Publish.ReworkOptionalChecks
	}

	if raw.Publish.GitUserName != nil {
		cfg.Publish.GitUserName = strings.TrimSpace(*raw.Publish.GitUserName)
	}

	if raw.Publish.GitUserEmail != nil {
		cfg.Publish.GitUserEmail = strings.TrimSpace(*raw.Publish.GitUserEmail)
	}

	if raw.Publish.CheckIntervalMS != nil {
		cfg.Publish.CheckInterval = durationFromMS(*raw.Publish.CheckIntervalMS)
	}

	if raw.Publish.MaxCheckReworkAttempts != nil {
		cfg.Publish.MaxCheckReworkAttempts = *raw.Publish.MaxCheckReworkAttempts
	}

	if raw.Debug.Enabled != nil {
		cfg.Debug.Enabled = *raw.Debug.Enabled
	}

	if raw.Debug.Address != nil {
		cfg.Debug.Address = strings.TrimSpace(*raw.Debug.Address)
	}

	if raw.Debug.EventLimit != nil {
		cfg.Debug.EventLimit = *raw.Debug.EventLimit
	}

	if raw.Hooks.AfterCreate != nil {
		cfg.Hooks.AfterCreate = *raw.Hooks.AfterCreate
	}

	if raw.Hooks.BeforeRun != nil {
		cfg.Hooks.BeforeRun = *raw.Hooks.BeforeRun
	}

	if raw.Hooks.AfterRun != nil {
		cfg.Hooks.AfterRun = *raw.Hooks.AfterRun
	}

	if raw.Hooks.BeforeRemove != nil {
		cfg.Hooks.BeforeRemove = *raw.Hooks.BeforeRemove
	}

	if raw.Hooks.TimeoutMS != nil {
		cfg.Hooks.Timeout = durationFromMS(*raw.Hooks.TimeoutMS)
	}

	if raw.Agent.MaxConcurrentAgents != nil {
		cfg.Agent.MaxConcurrentAgents = *raw.Agent.MaxConcurrentAgents
	}

	if raw.Agent.MaxTurns != nil {
		cfg.Agent.MaxTurns = *raw.Agent.MaxTurns
	}

	if raw.Agent.MaxRetryBackoffMS != nil {
		cfg.Agent.MaxRetryBackoff = durationFromMS(*raw.Agent.MaxRetryBackoffMS)
	}

	cfg.Agent.MaxConcurrentAgentsByState = normalizeStateLimits(raw.Agent.MaxConcurrentAgentsByState)

	if raw.Codex.Command != nil {
		cfg.Codex.Command = strings.TrimSpace(*raw.Codex.Command)
	}

	if raw.Codex.TurnTimeoutMS != nil {
		cfg.Codex.TurnTimeout = durationFromMS(*raw.Codex.TurnTimeoutMS)
	}

	if raw.Codex.ReadTimeoutMS != nil {
		cfg.Codex.ReadTimeout = durationFromMS(*raw.Codex.ReadTimeoutMS)
	}

	if raw.Codex.StallTimeoutMS != nil {
		cfg.Codex.StallTimeout = durationFromMS(*raw.Codex.StallTimeoutMS)
	}

	cfg.Codex.ApprovalPolicy = normalizeYAMLValue(raw.Codex.ApprovalPolicy)
	cfg.Codex.ThreadSandbox = normalizeThreadSandboxValue(raw.Codex.ThreadSandbox)
	cfg.Codex.TurnSandboxPolicy = normalizeTurnSandboxPolicyValue(raw.Codex.TurnSandboxPolicy)
}

func normalizeThreadSandboxValue(value any) any {
	normalized := normalizeYAMLValue(value)
	raw, ok := normalized.(map[string]any)
	if !ok {
		return normalized
	}

	mode, ok := raw["mode"].(string)
	if !ok || strings.TrimSpace(mode) == "" {
		return normalized
	}

	return strings.TrimSpace(mode)
}

func normalizeTurnSandboxPolicyValue(value any) any {
	normalized := normalizeYAMLValue(value)

	mode, ok := normalized.(string)
	if ok {
		if policy := sandboxPolicyForMode(mode); policy != nil {
			return policy
		}

		return normalized
	}

	raw, ok := normalized.(map[string]any)
	if !ok {
		return normalized
	}

	if mode, ok := raw["mode"].(string); ok {
		if policy := sandboxPolicyForMode(mode); policy != nil {
			return mergeSandboxPolicy(raw, policy)
		}
	}

	if policyType, ok := raw["type"].(string); ok {
		if policy := sandboxPolicyForType(policyType); policy != nil {
			return mergeSandboxPolicy(raw, policy)
		}
	}

	return normalized
}

func sandboxPolicyForMode(mode string) map[string]any {
	switch strings.TrimSpace(mode) {
	case "read-only", "readOnly":
		return map[string]any{"type": "readOnly"}
	case "workspace-write", "workspaceWrite":
		return map[string]any{"type": "workspaceWrite"}
	case "danger-full-access", "dangerFullAccess":
		return map[string]any{"type": "dangerFullAccess"}
	default:
		return nil
	}
}

func sandboxPolicyForType(policyType string) map[string]any {
	switch strings.TrimSpace(policyType) {
	case "readOnly", "workspaceWrite", "dangerFullAccess", "externalSandbox":
		return map[string]any{"type": strings.TrimSpace(policyType)}
	default:
		return nil
	}
}

func mergeSandboxPolicy(raw map[string]any, policy map[string]any) map[string]any {
	out := make(map[string]any, len(raw)+len(policy))
	for key, value := range raw {
		if key == "mode" {
			continue
		}

		out[key] = value
	}

	for key, value := range policy {
		out[key] = value
	}

	return out
}

func resolveConfigValues(ctx context.Context, cfg *Config, workflowDir string) error {
	cfg.Tracker.Kind = strings.ToLower(strings.TrimSpace(cfg.Tracker.Kind))

	switch cfg.Tracker.Kind {
	case trackerKindLinear:
		if cfg.Tracker.Endpoint == "" {
			cfg.Tracker.Endpoint = defaultLinearEndpoint
		}
	case trackerKindGitHub:
		if cfg.Tracker.Endpoint == "" || cfg.Tracker.Endpoint == defaultLinearEndpoint {
			cfg.Tracker.Endpoint = defaultGitHubEndpoint
		}

		if sameStringSet(cfg.Tracker.ActiveStates, []string{"Todo", "In Progress"}) {
			cfg.Tracker.ActiveStates = []string{"OPEN"}
		}

		if sameStringSet(cfg.Tracker.TerminalStates, []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}) {
			cfg.Tracker.TerminalStates = []string{"CLOSED"}
		}
	}

	if cfg.Tracker.Kind == trackerKindLinear && cfg.Tracker.Endpoint == "" {
		cfg.Tracker.Endpoint = defaultLinearEndpoint
	}

	if strings.HasPrefix(cfg.Tracker.APIKey, "$") {
		cfg.Tracker.APIKey = os.Getenv(strings.TrimPrefix(cfg.Tracker.APIKey, "$"))
	}

	if cfg.Tracker.Kind == trackerKindLinear && cfg.Tracker.APIKey == "" {
		cfg.Tracker.APIKey = os.Getenv(linearAPIKeyEnv)
	}

	if cfg.Tracker.Kind == trackerKindGitHub && cfg.Tracker.APIKey == "" {
		cfg.Tracker.APIKey = firstNonEmpty(os.Getenv(githubTokenEnv), os.Getenv(githubCLITokenEnv))
	}

	if cfg.Tracker.Kind == trackerKindGitHub && cfg.Tracker.APIKey == "" {
		cfg.Tracker.APIKey = githubTokenFromCLI(ctx)
	}

	if cfg.Tracker.Kind == trackerKindGitHub {
		owner, repo := splitRepository(cfg.Tracker.Repository)
		if cfg.Tracker.Owner == "" {
			cfg.Tracker.Owner = owner
		}

		if cfg.Tracker.Repo == "" {
			cfg.Tracker.Repo = repo
		}
	}

	root := strings.TrimSpace(cfg.Workspace.Root)
	if root == "" {
		root = filepath.Join(os.TempDir(), "symphony_workspaces")
	}

	root = expandHome(root)
	root = os.ExpandEnv(root)
	if !filepath.IsAbs(root) {
		root = filepath.Join(workflowDir, root)
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve workspace root %q: %w", root, err)
	}

	cfg.Workspace.Root = filepath.Clean(absRoot)
	resolvePublishDefaults(cfg)
	resolveDebugDefaults(cfg)

	return nil
}

func resolvePublishDefaults(cfg *Config) {
	if cfg == nil {
		return
	}

	cfg.Publish.Remote = firstNonEmpty(cfg.Publish.Remote, "origin")
	cfg.Publish.BaseBranch = firstNonEmpty(cfg.Publish.BaseBranch, "main")
	cfg.Publish.BranchPrefix = firstNonEmpty(cfg.Publish.BranchPrefix, "symphony")
	cfg.Publish.GitUserName = firstNonEmpty(cfg.Publish.GitUserName, "Atteler Symphony")
	cfg.Publish.GitUserEmail = firstNonEmpty(cfg.Publish.GitUserEmail, "symphony@users.noreply.github.com")
	if cfg.Publish.NoChecksPolicy == "" {
		cfg.Publish.NoChecksPolicy = defaultNoChecksPolicy
	}

	if cfg.Publish.CheckInterval <= 0 {
		cfg.Publish.CheckInterval = defaultPRCheckInterval
	}

	if cfg.Publish.MaxCheckReworkAttempts <= 0 {
		cfg.Publish.MaxCheckReworkAttempts = defaultMaxPRRework
	}

	if len(cfg.Publish.RemoveLabels) == 0 {
		cfg.Publish.RemoveLabels = append([]string(nil), cfg.Tracker.Labels...)
	}
}

func resolveDebugDefaults(cfg *Config) {
	if cfg == nil {
		return
	}

	cfg.Debug.Address = firstNonEmpty(cfg.Debug.Address, defaultDebugAddress)
	if cfg.Debug.EventLimit <= 0 {
		cfg.Debug.EventLimit = defaultDebugEventLimit
	}
}

// ValidatePreflight checks the config required to poll and launch workers.
func (cfg Config) ValidatePreflight() error {
	switch strings.ToLower(strings.TrimSpace(cfg.Tracker.Kind)) {
	case "":
		return errors.New("tracker.kind is required")
	case trackerKindLinear:
		if strings.TrimSpace(cfg.Tracker.APIKey) == "" {
			return errors.New("missing_tracker_api_key")
		}

		if strings.TrimSpace(cfg.Tracker.ProjectSlug) == "" {
			return errors.New("missing_tracker_project_slug")
		}
	case trackerKindGitHub:
		if strings.TrimSpace(cfg.Tracker.APIKey) == "" {
			return errors.New("missing_github_token")
		}

		if strings.TrimSpace(cfg.Tracker.Owner) == "" || strings.TrimSpace(cfg.Tracker.Repo) == "" {
			return errors.New("missing_github_repository")
		}
	default:
		return fmt.Errorf("unsupported_tracker_kind: %s", cfg.Tracker.Kind)
	}

	if strings.TrimSpace(cfg.Codex.Command) == "" {
		return errors.New("codex.command is required")
	}

	if cfg.Polling.Interval <= 0 {
		return errors.New("polling.interval_ms must be > 0")
	}

	if cfg.Hooks.Timeout <= 0 {
		return errors.New("hooks.timeout_ms must be > 0")
	}

	if cfg.Agent.MaxConcurrentAgents <= 0 {
		return errors.New("agent.max_concurrent_agents must be > 0")
	}

	if cfg.Agent.MaxTurns <= 0 {
		return errors.New("agent.max_turns must be > 0")
	}

	if cfg.Agent.MaxRetryBackoff <= 0 {
		return errors.New("agent.max_retry_backoff_ms must be > 0")
	}

	if cfg.Codex.TurnTimeout <= 0 {
		return errors.New("codex.turn_timeout_ms must be > 0")
	}

	if cfg.Codex.ReadTimeout <= 0 {
		return errors.New("codex.read_timeout_ms must be > 0")
	}

	if err := cfg.validatePublishConfig(); err != nil {
		return err
	}

	if err := cfg.validateDebugConfig(); err != nil {
		return err
	}

	return nil
}

func (cfg Config) validatePublishConfig() error {
	if !cfg.Publish.Enabled {
		return nil
	}

	if strings.ToLower(strings.TrimSpace(cfg.Tracker.Kind)) != trackerKindGitHub {
		return errors.New("publish.enabled requires tracker.kind: github")
	}

	if strings.TrimSpace(cfg.Publish.Remote) == "" {
		return errors.New("publish.remote is required")
	}

	if strings.TrimSpace(cfg.Publish.BaseBranch) == "" {
		return errors.New("publish.base_branch is required")
	}

	if strings.TrimSpace(cfg.Publish.BranchPrefix) == "" {
		return errors.New("publish.branch_prefix is required")
	}

	if len(trimNonEmptyStrings(cfg.Publish.RemoveLabels)) == 0 {
		return errors.New("publish.remove_labels is required to stop redispatching")
	}

	if cfg.Publish.MonitorChecks {
		if cfg.Publish.CheckInterval <= 0 {
			return errors.New("publish.check_interval_ms must be > 0 when publish.monitor_checks is true")
		}

		if cfg.Publish.MaxCheckReworkAttempts <= 0 {
			return errors.New("publish.max_check_rework_attempts must be > 0 when publish.monitor_checks is true")
		}

		switch cfg.Publish.NoChecksPolicy {
		case PullRequestNoChecksPass, PullRequestNoChecksPending, PullRequestNoChecksFail:
		default:
			return fmt.Errorf("publish.no_checks_policy must be one of pass, pending, fail when publish.monitor_checks is true: %s", cfg.Publish.NoChecksPolicy)
		}
	}

	return nil
}

func (cfg Config) validateDebugConfig() error {
	if !cfg.Debug.Enabled {
		return nil
	}

	if strings.TrimSpace(cfg.Debug.Address) == "" {
		return errors.New("debug.address is required")
	}

	if cfg.Debug.EventLimit <= 0 {
		return errors.New("debug.event_limit must be > 0")
	}

	return nil
}

func githubTokenFromCLI(ctx context.Context) string {
	if token := runGitHubCLI(ctx, "auth", "token"); token != "" {
		return token
	}

	return parseGitHubAuthStatusToken(runGitHubCLI(ctx, "auth", "status", "--show-token"))
}

func runGitHubCLI(ctx context.Context, args ...string) string {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var output bytes.Buffer
	cmd, invocation, err := shell.CommandContext(cmdCtx, shell.CommandOptions{
		Program: "gh",
		Args:    args,
		Stdout:  &output,
		Stderr:  &output,
		Mode:    shell.ModeCaptured,
		Audit:   shell.AuditContext{Caller: "symphony.gh_token"},
	})
	if err != nil {
		return ""
	}

	runErr := cmd.Run()
	if finishErr := invocation.Finish(shell.FinishOptions{
		Error:         runErr,
		OutputCapture: shell.OutputSensitive,
		OutputNote:    "GitHub CLI token output is intentionally not captured",
	}); finishErr != nil && runErr == nil {
		return ""
	}
	if runErr != nil {
		return ""
	}

	return strings.TrimSpace(output.String())
}

func parseGitHubAuthStatusToken(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		if !strings.HasPrefix(line, "Token:") {
			continue
		}

		token := strings.TrimSpace(strings.TrimPrefix(line, "Token:"))
		if token == "" || strings.Contains(token, "*") {
			return ""
		}

		return token
	}

	return ""
}

func codexExtraConfig(config map[string]any) map[string]any {
	raw, ok := config["codex"].(map[string]any)
	if !ok {
		return nil
	}

	extra := make(map[string]any, len(raw))
	for key, value := range raw {
		switch key {
		case "command", "approval_policy", "thread_sandbox", "turn_sandbox_policy", "turn_timeout_ms", "read_timeout_ms", "stall_timeout_ms":
			continue
		default:
			extra[key] = normalizeYAMLValue(value)
		}
	}

	if len(extra) == 0 {
		return nil
	}

	return extra
}

func trimNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}

	return out
}

func durationFromMS(ms int) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

func normalizeStateLimits(raw map[string]any) map[string]int {
	if len(raw) == 0 {
		return map[string]int{}
	}

	out := make(map[string]int, len(raw))
	for key, value := range raw {
		limit, ok := positiveInt(value)
		if !ok {
			continue
		}

		out[normalizeState(key)] = limit
	}

	return out
}

func positiveInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, typed > 0
	case int64:
		return int(typed), typed > 0
	case float64:
		if typed <= 0 || typed != float64(int(typed)) {
			return 0, false
		}

		return int(typed), true
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &parsed); err != nil {
			return 0, false
		}

		return parsed, parsed > 0
	default:
		rv := reflect.ValueOf(value)
		if rv.IsValid() {
			switch rv.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				parsed := int(rv.Int())
				return parsed, parsed > 0
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				parsed := int(rv.Uint())
				return parsed, parsed > 0
			}
		}
	}

	return 0, false
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return home
		}
	}

	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}

	return path
}

func normalizeState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func splitRepository(repository string) (string, string) {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 {
		return "", ""
	}

	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}

	return ""
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	counts := make(map[string]int, len(a))
	for _, value := range a {
		counts[normalizeState(value)]++
	}

	for _, value := range b {
		key := normalizeState(value)
		if counts[key] == 0 {
			return false
		}

		counts[key]--
	}

	return true
}
