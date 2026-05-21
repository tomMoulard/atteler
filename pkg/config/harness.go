package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	providerAnthropic  = "anthropic"
	providerClaudeCode = "claude-code"
	providerCodex      = "codex"
	providerOpenAI     = "openai"
)

// LoadHarnessDefaults imports best-effort defaults from other local coding
// harnesses. These imported values are intentionally lowest-precedence; any
// atteler config file loaded by Load overrides them.
func LoadHarnessDefaults() (cfg Config, loaded []string) {
	cfg, loaded, _ = LoadHarnessDefaultsWithOrigins()

	return cfg, loaded
}

// LoadHarnessDefaultsWithOrigins imports best-effort defaults from other local
// coding harnesses and records them as lowest-precedence harness-import origins.
func LoadHarnessDefaultsWithOrigins() (cfg Config, loaded []string, origins OriginMap) {
	origins = OriginMap{}
	recorder := newOriginRecorder(origins)

	for _, importer := range []func() (Config, string, bool){
		importCodexConfig,
		importClaudeConfig,
		importOpencodeConfig,
		importForgeConfig,
	} {
		next, path, ok := importer()
		if !ok {
			continue
		}

		mergeConfigFromSource(&cfg, next, recorder, originSource{
			kind:   OriginHarnessImport,
			source: path,
		})

		loaded = append(loaded, path)
	}

	normalizeEmptyMaps(&cfg)

	return cfg, loaded, origins
}

func importCodexConfig() (Config, string, bool) {
	for _, path := range codexConfigPaths() {
		data, ok := readOptional(path)
		if !ok {
			continue
		}

		cfg := parseCodexConfig(data)

		return cfg, path, !cfg.empty()
	}

	return Config{}, "", false
}

func codexConfigPaths() []string {
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		return []string{filepath.Join(dir, "config.toml"), homePath(".codex", "config.toml")}
	}

	return []string{homePath(".codex", "config.toml")}
}

func parseCodexConfig(data []byte) Config {
	topLevel, sections := parseSimpleTOML(data)

	provider := normalizeProvider(firstNonEmpty(
		topLevel["model_provider"],
		topLevel["provider"],
		topLevel["default_provider"],
	))
	model := topLevel["model"]

	if provider == "" {
		provider = providerCodex
	}

	cfg := Config{
		DefaultProvider: provider,
		DefaultModel:    model,
	}

	for section, values := range sections {
		name, ok := strings.CutPrefix(section, "model_providers.")
		if !ok {
			continue
		}

		providerName := normalizeProvider(name)
		if providerName == "" {
			continue
		}

		baseURL := firstNonEmpty(values["base_url"], values["baseURL"])
		if baseURL != "" {
			setProvider(&cfg, providerName, ProviderConfig{BaseURL: baseURL})
		}
	}

	return cfg
}

func importClaudeConfig() (Config, string, bool) {
	for _, path := range []string{
		homePath(".claude", "settings.json"),
		homePath(".claude.json"),
	} {
		data, ok := readOptional(path)
		if !ok {
			continue
		}

		cfg := parseGenericJSONHarness(data, providerClaudeCode)

		return cfg, path, !cfg.empty()
	}

	return Config{}, "", false
}

func importOpencodeConfig() (Config, string, bool) {
	cfg := Config{}
	loaded := make([]string, 0, 6)
	customConfig := strings.TrimSpace(os.Getenv("OPENCODE_CONFIG"))

	for _, path := range opencodeConfigPaths() {
		if customConfig != "" && path == customConfig {
			continue
		}

		data, ok := readOptional(path)
		if !ok {
			continue
		}

		mergeConfig(&cfg, parseOpencodeConfig(path, data))
		loaded = append(loaded, path)
	}

	for _, dir := range opencodeAgentDirs() {
		next, path, ok := loadOpencodeAgentDir(dir)
		if !ok {
			continue
		}

		mergeConfig(&cfg, next)

		loaded = append(loaded, path)
	}

	if customConfig != "" {
		data, ok := readOptional(customConfig)
		if ok {
			mergeConfig(&cfg, parseOpencodeConfig(customConfig, data))
			loaded = append(loaded, customConfig)
		}
	}

	if cfg.empty() {
		return Config{}, "", false
	}

	return cfg, strings.Join(loaded, ", "), true
}

func opencodeConfigPaths() []string {
	var paths []string

	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || slices.Contains(paths, path) {
			return
		}

		paths = append(paths, path)
	}

	add(configHomePath("opencode", "opencode.json"))
	add(configHomePath("opencode", "opencode.jsonc"))
	add(configHomePath("opencode", "config.json"))
	add(configHomePath("opencode", "config.jsonc"))
	add(configHomePath("opencode", "oh-my-openagent.json"))
	add(configHomePath("opencode", "oh-my-opencode.json"))
	add(homePath(".opencode.json"))
	add(homePath(".opencode.jsonc"))

	if cwd, err := os.Getwd(); err == nil {
		add(filepath.Join(cwd, "opencode.json"))
		add(filepath.Join(cwd, "opencode.jsonc"))
	}

	if custom := strings.TrimSpace(os.Getenv("OPENCODE_CONFIG")); custom != "" {
		add(custom)
	}

	return paths
}

func opencodeAgentDirs() []string {
	var dirs []string

	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || slices.Contains(dirs, path) {
			return
		}

		dirs = append(dirs, path)
	}

	add(configHomePath("opencode", "agents"))
	add(configHomePath("opencode", "agent"))

	if customDir := strings.TrimSpace(os.Getenv("OPENCODE_CONFIG_DIR")); customDir != "" {
		add(filepath.Join(customDir, "agents"))
		add(filepath.Join(customDir, "agent"))
	}

	if cwd, err := os.Getwd(); err == nil {
		add(filepath.Join(cwd, ".opencode", "agents"))
		add(filepath.Join(cwd, ".opencode", "agent"))
	}

	return dirs
}

func importForgeConfig() (Config, string, bool) {
	for _, path := range forgeConfigPaths() {
		data, ok := readOptional(path)
		if !ok {
			continue
		}

		cfg := parseForgeConfig(data)

		return cfg, path, !cfg.empty()
	}

	return Config{}, "", false
}

func forgeConfigPaths() []string {
	var paths []string

	add := func(path string) {
		if path == "" {
			return
		}

		if slices.Contains(paths, path) {
			return
		}

		paths = append(paths, path)
	}

	if dir := os.Getenv("FORGE_CONFIG"); dir != "" {
		add(filepath.Join(dir, ".forge.toml"))
	}

	add(homePath("forge", ".forge.toml"))
	add(homePath(".forge", ".forge.toml"))

	return paths
}

func parseForgeConfig(data []byte) Config {
	topLevel, sections := parseSimpleTOML(data)
	session := sections["session"]
	model := firstNonEmpty(
		session["model_id"],
		session["model"],
		topLevel["model_id"],
		topLevel["model"],
	)

	provider := normalizeProvider(firstNonEmpty(
		session["provider_id"],
		session["provider"],
		topLevel["provider_id"],
		topLevel["provider"],
	))
	if provider == "" {
		provider = inferProvider(model)
	}

	cfg := Config{
		DefaultProvider: provider,
		DefaultModel:    model,
	}

	if provider != "" {
		baseURL := firstNonEmpty(session["base_url"], topLevel["base_url"])
		if baseURL != "" {
			setProvider(&cfg, provider, ProviderConfig{BaseURL: baseURL})
		}
	}

	return cfg
}

func parseGenericJSONHarness(data []byte, fallbackProvider string) Config {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}
	}

	model := findString(raw, "model", "default_model", "defaultModel")

	provider := normalizeProvider(findString(raw, "provider", "model_provider", "default_provider", "defaultProvider"))
	if provider == "" {
		provider = normalizeProvider(fallbackProvider)
	}

	if provider == "" {
		provider = inferProvider(model)
	}

	cfg := Config{
		DefaultProvider: provider,
		DefaultModel:    model,
	}

	baseURL := findString(raw, "base_url", "baseURL", "api_base", "apiBase", "api_url", "apiURL")
	if provider != "" && baseURL != "" {
		setProvider(&cfg, provider, ProviderConfig{BaseURL: baseURL})
	}

	return cfg
}

func parseOpencodeConfig(path string, data []byte) Config {
	var raw map[string]any
	if err := json.Unmarshal(stripJSONComments(data), &raw); err != nil {
		return Config{}
	}

	model := topLevelString(raw, "model", "default_model", "defaultModel")
	if model == "" {
		model = findOpenCodeFallbackModel(raw)
	}

	provider := normalizeProvider(topLevelString(raw, "default_provider", "defaultProvider"))
	if provider == "" {
		provider = inferProviderFromQualifiedModel(model)
	}

	if provider == "" {
		provider = inferProvider(model)
	}

	cfg := Config{
		DefaultProvider: provider,
		DefaultModel:    model,
	}

	addOpenCodeProviders(&cfg, raw, provider)
	addOpenCodeAgents(&cfg, filepath.Dir(path), raw)

	return cfg
}

func addOpenCodeProviders(cfg *Config, raw map[string]any, defaultProvider string) {
	if providers, ok := raw["provider"].(map[string]any); ok {
		for name, value := range providers {
			providerName := normalizeProvider(name)

			nested, ok := value.(map[string]any)
			if !ok || providerName == "" {
				continue
			}

			baseURL := findString(nested, "base_url", "baseURL", "api_base", "apiBase", "api_url", "apiURL")
			if baseURL != "" {
				setProvider(cfg, providerName, ProviderConfig{BaseURL: baseURL})
			}
		}
	}

	baseURL := topLevelString(raw, "base_url", "baseURL", "api_base", "apiBase", "api_url", "apiURL")
	if defaultProvider != "" && baseURL != "" {
		setProvider(cfg, defaultProvider, ProviderConfig{BaseURL: baseURL})
	}
}

func addOpenCodeAgents(cfg *Config, baseDir string, raw map[string]any) {
	agents, ok := raw["agent"].(map[string]any)
	if !ok {
		return
	}

	for name, value := range agents {
		nested, ok := value.(map[string]any)
		if !ok {
			continue
		}

		if agentCfg, ok := parseOpenCodeAgentMap(baseDir, name, nested); ok {
			setAgent(cfg, name, agentCfg)
		}
	}
}

func loadOpencodeAgentDir(dir string) (Config, string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Config{}, "", false
	}

	cfg := Config{}

	loadedFiles := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}

		if !isOpenCodeAgentFile(entry.Name()) {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		agentCfg, ok := parseOpenCodeAgentFile(path)
		if !ok {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		setAgent(&cfg, name, agentCfg)

		loadedFiles = append(loadedFiles, path)
	}

	if len(loadedFiles) == 0 {
		return Config{}, "", false
	}

	return cfg, strings.Join(loadedFiles, ", "), true
}

func isOpenCodeAgentFile(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}

	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}

func parseOpenCodeAgentFile(path string) (AgentConfig, bool) {
	data, ok := readOptional(path)
	if !ok {
		return AgentConfig{}, false
	}

	frontmatter, body := splitFrontmatter(string(data))
	if strings.TrimSpace(frontmatter) == "" {
		if prompt := strings.TrimSpace(string(data)); prompt != "" {
			return AgentConfig{SystemPrompt: prompt}, true
		}

		return AgentConfig{}, false
	}

	var meta openCodeAgentFrontmatter
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return AgentConfig{}, false
	}

	if meta.Disable {
		return AgentConfig{}, false
	}

	cfg, ok := meta.agentConfig(filepath.Dir(path))
	if !ok {
		return AgentConfig{}, false
	}

	if prompt := strings.TrimSpace(body); prompt != "" {
		cfg.SystemPrompt = prompt
	}

	return cfg, !agentConfigEmpty(cfg)
}

type openCodeAgentFrontmatter struct {
	Temperature *float64        `yaml:"temperature"`
	TopP        *float64        `yaml:"top_p"`
	TopPAlt     *float64        `yaml:"topP"`
	Hidden      *bool           `yaml:"hidden"`
	Tools       map[string]bool `yaml:"tools"`
	Description string          `yaml:"description"`
	Model       string          `yaml:"model"`
	Mode        string          `yaml:"mode"`
	Prompt      string          `yaml:"prompt"`
	MaxTokens   int             `yaml:"max_tokens"`
	Disable     bool            `yaml:"disable"`
}

func (m openCodeAgentFrontmatter) agentConfig(baseDir string) (AgentConfig, bool) {
	topP := m.TopP
	if topP == nil {
		topP = m.TopPAlt
	}

	cfg := AgentConfig{
		Description:     m.Description,
		Model:           m.Model,
		Mode:            m.Mode,
		ToolPermissions: m.Tools,
		Temperature:     m.Temperature,
		TopP:            topP,
		MaxTokens:       m.MaxTokens,
	}
	if prompt, ok := resolvePromptReference(baseDir, m.Prompt); ok {
		cfg.SystemPrompt = prompt
	} else {
		return AgentConfig{}, false
	}

	if m.Hidden != nil {
		cfg.Hidden = *m.Hidden
		cfg.hiddenSet = true
	}

	return cfg, true
}

func parseOpenCodeAgentMap(baseDir, name string, raw map[string]any) (AgentConfig, bool) {
	if boolValue(raw, "disable") {
		return AgentConfig{}, false
	}

	cfg := AgentConfig{
		Description: strings.TrimSpace(topLevelString(raw, "description")),
		Model:       strings.TrimSpace(topLevelString(raw, "model")),
		Mode:        strings.TrimSpace(topLevelString(raw, "mode")),
		MaxTokens:   intValue(raw, "max_tokens", "maxTokens"),
	}

	// Parse tool permissions from "tools" key (map[string]bool).
	if toolsRaw, ok := raw["tools"]; ok {
		if toolsMap, ok := toolsRaw.(map[string]any); ok {
			cfg.ToolPermissions = make(map[string]bool, len(toolsMap))
			for k, v := range toolsMap {
				bv, ok := v.(bool)
				if !ok {
					slog.Warn("opencode agent tool permission ignored: expected boolean", "agent", name, "tool", k, "type", fmt.Sprintf("%T", v))

					continue
				}

				cfg.ToolPermissions[k] = bv
			}
		}
	}

	prompt, ok := resolvePromptReference(baseDir, topLevelString(raw, "prompt"))
	if !ok {
		return AgentConfig{}, false
	}

	cfg.SystemPrompt = prompt
	if hidden, ok := boolPtr(raw, "hidden"); ok {
		cfg.Hidden = *hidden
		cfg.hiddenSet = true
	}

	if value, ok := numberPtr(raw, "temperature"); ok {
		cfg.Temperature = value
	}

	if value, ok := numberPtr(raw, "top_p", "topP"); ok {
		cfg.TopP = value
	}

	if strings.TrimSpace(name) == "" || agentConfigEmpty(cfg) {
		return AgentConfig{}, false
	}

	return cfg, true
}

func splitFrontmatter(data string) (frontmatter, body string) {
	if !strings.HasPrefix(data, "---") {
		return "", data
	}

	lines := strings.Split(data, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return "", data
	}

	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "---" {
			continue
		}

		return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n")
	}

	return "", data
}

func resolvePromptReference(baseDir, prompt string) (string, bool) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", true
	}

	trimmed := strings.TrimSuffix(strings.TrimPrefix(prompt, "{file:"), "}")
	if strings.HasPrefix(prompt, "{file:") && trimmed != prompt {
		trimmed = strings.TrimSpace(trimmed)
		if filepath.IsAbs(trimmed) {
			return "", false
		}

		resolved := filepath.Clean(filepath.Join(baseDir, trimmed))
		if !pathWithinDir(baseDir, resolved) {
			return "", false
		}

		safePath, ok := safeRegularFilePath(resolved)
		if !ok {
			return "", false
		}

		data, ok := readOptional(safePath)
		if !ok {
			return "", false
		}

		return strings.TrimSpace(string(data)), true
	}

	return prompt, true
}

func pathWithinDir(baseDir, path string) bool {
	baseEval, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return false
	}

	pathEval, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}

	rel, err := filepath.Rel(baseEval, pathEval)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

func safeRegularFilePath(path string) (string, bool) {
	evaluated, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}

	info, err := os.Stat(evaluated)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}

	return evaluated, true
}

func setAgent(cfg *Config, name string, agent AgentConfig) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}

	if cfg.Agents == nil {
		cfg.Agents = make(map[string]AgentConfig)
	}

	current := cfg.Agents[name]
	mergeConfigAgent(&current, agent, nil, originSource{}, name)
	cfg.Agents[name] = current
}

func agentConfigEmpty(cfg AgentConfig) bool {
	return cfg.Model == "" &&
		cfg.Description == "" &&
		cfg.SystemPrompt == "" &&
		cfg.Temperature == nil &&
		cfg.TopP == nil &&
		cfg.MaxTokens == 0 &&
		!cfg.Hidden &&
		!cfg.hiddenSet
}

func parseSimpleTOML(data []byte) (topLevel map[string]string, sections map[string]map[string]string) {
	topLevel = make(map[string]string)
	sections = make(map[string]map[string]string)
	current := topLevel

	for rawLine := range strings.SplitSeq(string(data), "\n") {
		line := strings.TrimSpace(stripTOMLComment(rawLine))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(strings.Trim(line, "[]"))
			if sections[section] == nil {
				sections[section] = make(map[string]string)
			}

			current = sections[section]

			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)

		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}

		current[key] = unquoteTOMLString(value)
	}

	return topLevel, sections
}

func stripTOMLComment(line string) string {
	inQuote := rune(0)

	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}

		if r == '\\' {
			escaped = true
			continue
		}

		if inQuote != 0 {
			if r == inQuote {
				inQuote = 0
			}

			continue
		}

		if r == '\'' || r == '"' {
			inQuote = r
			continue
		}

		if r == '#' {
			return line[:i]
		}
	}

	return line
}

func unquoteTOMLString(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return value
	}

	quote := value[0]
	if (quote != '"' && quote != '\'') || value[len(value)-1] != quote {
		return value
	}

	unquoted := strings.Trim(value, string(quote))
	if quote == '"' {
		unquoted = strings.ReplaceAll(unquoted, `\"`, `"`)
		unquoted = strings.ReplaceAll(unquoted, `\\`, `\`)
	}

	return unquoted
}

func findString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			if s, ok := value.(string); ok && s != "" {
				return s
			}
		}
	}

	for _, value := range raw {
		nested, ok := value.(map[string]any)
		if !ok {
			continue
		}

		if found := findString(nested, keys...); found != "" {
			return found
		}
	}

	return ""
}

func topLevelString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			if s, ok := value.(string); ok && s != "" {
				return s
			}
		}
	}

	return ""
}

func boolValue(raw map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			if b, ok := value.(bool); ok {
				return b
			}
		}
	}

	return false
}

func boolPtr(raw map[string]any, keys ...string) (*bool, bool) {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			if b, ok := value.(bool); ok {
				v := b
				return &v, true
			}
		}
	}

	return nil, false
}

func intValue(raw map[string]any, keys ...string) int {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			switch typed := value.(type) {
			case float64:
				return int(typed)
			case int:
				return typed
			}
		}
	}

	return 0
}

func numberPtr(raw map[string]any, keys ...string) (*float64, bool) {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			if number, ok := value.(float64); ok {
				v := number
				return &v, true
			}
		}
	}

	return nil, false
}

func inferProvider(model string) string {
	model = strings.ToLower(model)
	switch {
	case strings.HasPrefix(model, "claude"):
		return providerAnthropic
	case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
		return providerOpenAI
	default:
		return ""
	}
}

func inferProviderFromQualifiedModel(model string) string {
	provider, _, ok := strings.Cut(strings.TrimSpace(model), "/")
	if !ok {
		return ""
	}

	return normalizeProvider(provider)
}

func normalizeProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))

	provider = strings.Trim(provider, `"'`)
	switch {
	case provider == "":
		return ""
	case strings.Contains(provider, "claude-code") || strings.Contains(provider, "claude_code"):
		return providerClaudeCode
	case strings.Contains(provider, "anthropic") || strings.Contains(provider, "claude"):
		return providerAnthropic
	case strings.Contains(provider, "codex"):
		return providerCodex
	case strings.Contains(provider, "openai") || strings.Contains(provider, "chatgpt"):
		return providerOpenAI
	default:
		return provider
	}
}

func setProvider(cfg *Config, name string, provider ProviderConfig) {
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderConfig)
	}

	current := cfg.Providers[name]
	if provider.BaseURL != "" {
		current.BaseURL = provider.BaseURL
	}

	current.Disabled = provider.Disabled

	if provider.TimeoutSeconds > 0 {
		current.TimeoutSeconds = provider.TimeoutSeconds
	}

	cfg.Providers[name] = current
}

func (c Config) empty() bool {
	return c.DefaultProvider == "" &&
		c.DefaultModel == "" &&
		len(c.Providers) == 0 &&
		len(c.Agents) == 0 &&
		len(c.Hooks) == 0
}

func readOptional(path string) ([]byte, bool) {
	if path == "" {
		return nil, false
	}
	// #nosec G304,G703 -- config import intentionally reads caller-selected paths.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}

	return data, true
}

func stripJSONComments(data []byte) []byte {
	var out strings.Builder
	out.Grow(len(data))

	state := jsonStripState{}

	for i := 0; i < len(data); i++ {
		ch := data[i]
		if state.skipBlockCommentEnd(data, i) {
			i++
			continue
		}

		if state.writeNext(&out, ch) {
			continue
		}

		skippedComment := state.skipComment(data, i)
		if skippedComment {
			i++
			continue
		}

		if ch == ',' {
			next := nextNonSpace(data, i+1)
			if next == ']' || next == '}' {
				continue
			}
		}

		out.WriteByte(ch)
	}

	return stripJSONTrailingCommas([]byte(out.String()))
}

type jsonStripState struct {
	inString       bool
	escaped        bool
	inLineComment  bool
	inBlockComment bool
}

func (s *jsonStripState) writeNext(out *strings.Builder, ch byte) bool {
	switch {
	case s.inLineComment:
		if ch == '\n' {
			s.inLineComment = false

			out.WriteByte(ch)
		}

		return true
	case s.inBlockComment:
		return true
	case s.inString:
		s.writeStringByte(out, ch)
		return true
	case ch == '"':
		s.inString = true

		out.WriteByte(ch)

		return true
	default:
		return false
	}
}

func (s *jsonStripState) writeStringByte(out *strings.Builder, ch byte) {
	out.WriteByte(ch)

	if s.escaped {
		s.escaped = false
		return
	}

	if ch == '\\' {
		s.escaped = true
		return
	}

	if ch == '"' {
		s.inString = false
	}
}

func (s *jsonStripState) skipComment(data []byte, i int) bool {
	if s.inBlockComment {
		return false
	}

	if i+1 >= len(data) {
		return false
	}

	if data[i] != '/' {
		return false
	}

	switch data[i+1] {
	case '/':
		s.inLineComment = true
		return true
	case '*':
		s.inBlockComment = true
		return true
	default:
		return false
	}
}

func (s *jsonStripState) skipBlockCommentEnd(data []byte, i int) bool {
	if !s.inBlockComment || i+1 >= len(data) {
		return false
	}

	if data[i] == '*' && data[i+1] == '/' {
		s.inBlockComment = false
		return true
	}

	return false
}

func nextNonSpace(data []byte, start int) byte {
	for i := start; i < len(data); i++ {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return data[i]
		}
	}

	return 0
}

func stripJSONTrailingCommas(data []byte) []byte {
	var out strings.Builder
	out.Grow(len(data))

	inString := false
	escaped := false

	for i := range data {
		ch := data[i]
		if inString {
			out.WriteByte(ch)

			if escaped {
				escaped = false
				continue
			}

			if ch == '\\' {
				escaped = true
				continue
			}

			if ch == '"' {
				inString = false
			}

			continue
		}

		if ch == '"' {
			inString = true

			out.WriteByte(ch)

			continue
		}

		if ch == ',' {
			next := nextNonSpace(data, i+1)
			if next == ']' || next == '}' {
				continue
			}
		}

		out.WriteByte(ch)
	}

	return []byte(out.String())
}

func findOpenCodeFallbackModel(raw map[string]any) string {
	if model := findOpenCodeSectionModel(raw, "categories",
		"deep", "ultrabrain", "quick", "unspecified-high", "unspecified-low"); model != "" {
		return model
	}

	return findOpenCodeSectionModel(raw, "agents", "oracle", "atlas", "explore")
}

func findOpenCodeSectionModel(raw map[string]any, section string, preferredNames ...string) string {
	nested, ok := raw[section].(map[string]any)
	if !ok {
		return ""
	}

	for _, name := range preferredNames {
		if model := findNamedModel(nested, name); model != "" {
			return model
		}
	}

	return findFirstNestedModel(nested)
}

func findNamedModel(raw map[string]any, name string) string {
	value, ok := raw[name]
	if !ok {
		return ""
	}

	nested, ok := value.(map[string]any)
	if !ok {
		return ""
	}

	return findString(nested, "model")
}

func findFirstNestedModel(raw map[string]any) string {
	for _, value := range raw {
		nested, ok := value.(map[string]any)
		if !ok {
			continue
		}

		if model := findString(nested, "model"); model != "" {
			return model
		}
	}

	return ""
}

func homePath(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}

	return filepath.Join(append([]string{home}, parts...)...)
}

func configHomePath(parts ...string) string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(append([]string{dir}, parts...)...)
	}

	return homePath(append([]string{".config"}, parts...)...)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
