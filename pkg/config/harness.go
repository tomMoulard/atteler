package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
	"gopkg.in/yaml.v3"
)

const (
	providerAnthropic  = "anthropic"
	providerClaudeCode = "claude-code"
	providerCodex      = "codex"
	providerOpenAI     = "openai"
)

var (
	// Importer schemas list fields that this importer actually consumes or
	// needs as structure. Any other parsed field is surfaced as an ignored
	// unsupported field instead of being silently dropped.
	codexTopLevelSchema = stringSet(
		"$schema",
		"model",
		"model_provider",
		"provider",
		"default_provider",
		"service_tier",
		"serviceTier",
		"model_providers",
		"profile",
		"profiles",
	)
	codexProviderSchema = stringSet(
		"base_url",
		"baseURL",
	)

	claudeTopLevelSchema = stringSet(
		"$schema",
		"model",
		"default_model",
		"defaultModel",
		"provider",
		"model_provider",
		"default_provider",
		"defaultProvider",
		"base_url",
		"baseURL",
		"api_base",
		"apiBase",
		"api_url",
		"apiURL",
		"llm",
	)
	claudeLLMSchema = stringSet(
		"model",
		"default_model",
		"defaultModel",
		"provider",
		"model_provider",
		"default_provider",
		"defaultProvider",
		"base_url",
		"baseURL",
		"api_base",
		"apiBase",
		"api_url",
		"apiURL",
	)

	openCodeTopLevelSchema = stringSet(
		"$schema",
		"model",
		"default_model",
		"defaultModel",
		"default_provider",
		"defaultProvider",
		"base_url",
		"baseURL",
		"api_base",
		"apiBase",
		"api_url",
		"apiURL",
		"provider",
		"agent",
		"agents",
		"categories",
	)
	openCodeProviderSchema = stringSet(
		"base_url",
		"baseURL",
		"api_base",
		"apiBase",
		"api_url",
		"apiURL",
	)
	openCodeAgentSchema = stringSet(
		"description",
		"model",
		"mode",
		"model_mode",
		"modelMode",
		"prompt",
		"temperature",
		"top_p",
		"topP",
		"max_tokens",
		"maxTokens",
		"hidden",
		"tools",
		"disable",
	)
	openCodeAgentFileSchema = stringSet(
		"description",
		"model",
		"mode",
		"model_mode",
		"modelMode",
		"prompt",
		"temperature",
		"top_p",
		"topP",
		"max_tokens",
		"maxTokens",
		"hidden",
		"tools",
		"disable",
	)

	forgeTopLevelSchema = stringSet(
		"$schema",
		"model_id",
		"model",
		"provider_id",
		"provider",
		"base_url",
		"session",
	)
	forgeSessionSchema = stringSet(
		"model_id",
		"model",
		"provider_id",
		"provider",
		"base_url",
	)
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
	cfg, loaded, origins, _ = LoadHarnessDefaultsWithDiagnostics()

	return cfg, loaded, origins
}

// LoadHarnessDefaultsWithDiagnostics imports best-effort defaults from other
// local coding harnesses, records per-field origins, and returns non-fatal
// diagnostics for ignored or unsupported harness input.
func LoadHarnessDefaultsWithDiagnostics() (cfg Config, loaded []string, origins OriginMap, diagnostics []Diagnostic) {
	origins = OriginMap{}

	for _, importer := range []func() harnessImportResult{
		importCodexConfigDetailed,
		importClaudeConfigDetailed,
		importOpencodeConfigDetailed,
		importForgeConfigDetailed,
	} {
		result := importer()
		mergeConfigFromOrigins(&cfg, result.Config, origins, result.Origins)
		loaded = append(loaded, result.Loaded...)
		diagnostics = append(diagnostics, result.Diagnostics...)
	}

	normalizeEmptyMaps(&cfg)

	return cfg, loaded, origins, diagnostics
}

type harnessImportResult struct {
	Loaded      []string
	Origins     OriginMap
	Diagnostics []Diagnostic
	Config      Config
}

func newHarnessImportResult() harnessImportResult {
	return harnessImportResult{Origins: OriginMap{}}
}

func (r *harnessImportResult) mergeSource(path string, cfg Config) {
	if cfg.empty() {
		return
	}

	mergeConfigFromSource(&r.Config, cfg, newOriginRecorder(r.Origins), originSource{
		kind:   OriginHarnessImport,
		source: path,
	})

	r.Loaded = append(r.Loaded, path)
}

func importCodexConfigDetailed() harnessImportResult {
	result := newHarnessImportResult()

	for _, path := range codexConfigPaths() {
		data, readDiagnostics, ok := readHarnessSource(path, "codex")
		result.Diagnostics = append(result.Diagnostics, readDiagnostics...)

		if !ok {
			continue
		}

		cfg, diagnostics := parseCodexConfigWithDiagnostics(path, data)
		result.Diagnostics = append(result.Diagnostics, diagnostics...)
		result.mergeSource(path, cfg)

		return result
	}

	return result
}

func codexConfigPaths() []string {
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		return []string{filepath.Join(dir, "config.toml"), homePath(".codex", "config.toml")}
	}

	return []string{homePath(".codex", "config.toml")}
}

func parseCodexConfig(data []byte) Config {
	cfg, _ := parseCodexConfigWithDiagnostics("", data)

	return cfg
}

func parseCodexConfigWithDiagnostics(source string, data []byte) (Config, []Diagnostic) {
	raw, diagnostics, ok := decodeTOMLMap(source, "codex", data)
	if !ok {
		return Config{}, diagnostics
	}

	collector := newDiagnosticCollector("codex", source)
	warnUnsupportedKeys(raw, codexTopLevelSchema, collector, "")

	provider := normalizeProvider(firstNonEmpty(
		stringAt(raw, collector, "model_provider"),
		stringAt(raw, collector, "provider"),
		stringAt(raw, collector, "default_provider"),
	))
	model := stringAt(raw, collector, "model")

	if provider == "" && model != "" {
		provider = providerCodex
	}

	cfg := Config{
		DefaultModel: model,
	}

	if provider != "" {
		cfg.DefaultProvider = provider
	}

	if serviceTier := strings.TrimSpace(stringAt(raw, collector, "service_tier", "service_tier", "serviceTier")); serviceTier != "" {
		if modelMode, ok := modelModeFromOpenAIServiceTier(serviceTier); ok {
			cfg.Generation.ModelMode = modelMode
		} else {
			collector.warnf("service_tier", "ignored value: unsupported service tier %q for model_mode import", serviceTier)
		}
	}

	modelProviders, _ := mapAt(raw, collector, "model_providers")
	for _, name := range sortedMapKeys(modelProviders) {
		value := modelProviders[name]

		providerName := normalizeProvider(name)
		if providerName == "" {
			continue
		}

		values, ok := value.(map[string]any)
		if !ok {
			collector.warnf(joinPath("model_providers", name), "ignored provider section: expected table, got %s", typeName(value))

			continue
		}

		providerPath := joinPath("model_providers", name)
		warnUnsupportedKeys(values, codexProviderSchema, collector, providerPath)

		baseURL := firstNonEmpty(
			stringAt(values, collector, joinPath(providerPath, "base_url"), "base_url"),
			stringAt(values, collector, joinPath(providerPath, "baseURL"), "baseURL"),
		)
		if baseURL != "" {
			setProvider(&cfg, providerName, ProviderConfig{BaseURL: baseURL})
		}
	}

	if _, ok := raw["profile"]; ok {
		collector.warnf("profile", "ignored active Codex profile; only top-level defaults and model_providers base_url values are imported")
	}

	if _, ok := raw["profiles"]; ok {
		collector.warnf("profiles", "ignored Codex profile-specific settings")
	}

	diagnostics = append(diagnostics, collector.all()...)

	return cfg, diagnostics
}

func modelModeFromOpenAIServiceTier(serviceTier string) (string, bool) {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(serviceTier), "_", "-")) {
	case "", "auto", "default", "standard":
		return "", true
	case "priority", "fast":
		return "fast", true
	default:
		return "", false
	}
}

func importClaudeConfigDetailed() harnessImportResult {
	result := newHarnessImportResult()

	for _, path := range []string{
		homePath(".claude", "settings.json"),
		homePath(".claude.json"),
	} {
		data, readDiagnostics, ok := readHarnessSource(path, "claude")
		result.Diagnostics = append(result.Diagnostics, readDiagnostics...)

		if !ok {
			continue
		}

		cfg, diagnostics := parseClaudeConfigWithDiagnostics(path, data)
		result.Diagnostics = append(result.Diagnostics, diagnostics...)
		result.mergeSource(path, cfg)

		return result
	}

	return result
}

func importOpencodeConfig() (Config, string, bool) {
	result := importOpencodeConfigDetailed()
	if result.Config.empty() {
		return Config{}, "", false
	}

	return result.Config, strings.Join(result.Loaded, ", "), true
}

func importOpencodeConfigDetailed() harnessImportResult {
	result := newHarnessImportResult()
	customConfig := strings.TrimSpace(os.Getenv("OPENCODE_CONFIG"))

	for _, path := range opencodeConfigPaths() {
		if customConfig != "" && path == customConfig {
			continue
		}

		data, readDiagnostics, ok := readHarnessSource(path, "opencode")
		result.Diagnostics = append(result.Diagnostics, readDiagnostics...)

		if !ok {
			continue
		}

		cfg, diagnostics := parseOpencodeConfigWithDiagnostics(path, data)
		result.Diagnostics = append(result.Diagnostics, diagnostics...)
		result.mergeSource(path, cfg)
	}

	for _, dir := range opencodeAgentDirs() {
		_, agents, diagnostics, ok := loadOpencodeAgentImportsWithDiagnostics(dir)
		result.Diagnostics = append(result.Diagnostics, diagnostics...)

		if !ok {
			continue
		}

		for i := range agents {
			agent := &agents[i]
			agentCfg := Config{}
			setAgent(&agentCfg, agent.Name, agent.Config)
			result.mergeSource(agent.Path, agentCfg)
		}
	}

	if customConfig != "" {
		data, readDiagnostics, ok := readHarnessSource(customConfig, "opencode")
		result.Diagnostics = append(result.Diagnostics, readDiagnostics...)

		if ok {
			cfg, diagnostics := parseOpencodeConfigWithDiagnostics(customConfig, data)
			result.Diagnostics = append(result.Diagnostics, diagnostics...)
			result.mergeSource(customConfig, cfg)
		}
	}

	return result
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

func importForgeConfigDetailed() harnessImportResult {
	result := newHarnessImportResult()

	for _, path := range forgeConfigPaths() {
		data, readDiagnostics, ok := readHarnessSource(path, "forge")
		result.Diagnostics = append(result.Diagnostics, readDiagnostics...)

		if !ok {
			continue
		}

		cfg, diagnostics := parseForgeConfigWithDiagnostics(path, data)
		result.Diagnostics = append(result.Diagnostics, diagnostics...)
		result.mergeSource(path, cfg)

		return result
	}

	return result
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
	cfg, _ := parseForgeConfigWithDiagnostics("", data)

	return cfg
}

func parseForgeConfigWithDiagnostics(source string, data []byte) (Config, []Diagnostic) {
	raw, diagnostics, ok := decodeTOMLMap(source, "forge", data)
	if !ok {
		return Config{}, diagnostics
	}

	collector := newDiagnosticCollector("forge", source)
	warnUnsupportedKeys(raw, forgeTopLevelSchema, collector, "")

	session, _ := mapAt(raw, collector, "session")
	warnUnsupportedKeys(session, forgeSessionSchema, collector, "session")

	model := firstNonEmpty(
		stringAt(session, collector, "session.model_id", "model_id"),
		stringAt(session, collector, "session.model", "model"),
		stringAt(raw, collector, "model_id"),
		stringAt(raw, collector, "model"),
	)

	provider := normalizeProvider(firstNonEmpty(
		stringAt(session, collector, "session.provider_id", "provider_id"),
		stringAt(session, collector, "session.provider", "provider"),
		stringAt(raw, collector, "provider_id"),
		stringAt(raw, collector, "provider"),
	))
	if provider == "" {
		provider = inferProvider(model)
	}

	cfg := Config{
		DefaultProvider: provider,
		DefaultModel:    model,
	}

	if provider != "" {
		baseURL := firstNonEmpty(
			stringAt(session, collector, "session.base_url", "base_url"),
			stringAt(raw, collector, "base_url"),
		)
		if baseURL != "" {
			setProvider(&cfg, provider, ProviderConfig{BaseURL: baseURL})
		}
	}

	diagnostics = append(diagnostics, collector.all()...)

	return cfg, diagnostics
}

func parseGenericJSONHarness(data []byte, fallbackProvider string) Config {
	cfg, _ := parseGenericJSONHarnessWithDiagnostics("", data, fallbackProvider)

	return cfg
}

func parseClaudeConfigWithDiagnostics(source string, data []byte) (Config, []Diagnostic) {
	return parseGenericJSONHarnessWithDiagnostics(source, data, providerClaudeCode)
}

func parseGenericJSONHarnessWithDiagnostics(source string, data []byte, fallbackProvider string) (Config, []Diagnostic) {
	raw, diagnostics, ok := decodeJSONCMap(source, "claude", data)
	if !ok {
		return Config{}, diagnostics
	}

	collector := newDiagnosticCollector("claude", source)
	warnUnsupportedKeys(raw, claudeTopLevelSchema, collector, "")

	if llm, ok := mapAt(raw, collector, "llm"); ok {
		warnUnsupportedKeys(llm, claudeLLMSchema, collector, "llm")
	}

	model := firstNonEmpty(
		stringAt(raw, collector, "model"),
		stringAt(raw, collector, "default_model"),
		stringAt(raw, collector, "defaultModel"),
		stringAtPath(raw, collector, "llm.model", "llm", "model"),
		stringAtPath(raw, collector, "llm.default_model", "llm", "default_model"),
		stringAtPath(raw, collector, "llm.defaultModel", "llm", "defaultModel"),
	)

	provider := normalizeProvider(firstNonEmpty(
		stringAt(raw, collector, "provider"),
		stringAt(raw, collector, "model_provider"),
		stringAt(raw, collector, "default_provider"),
		stringAt(raw, collector, "defaultProvider"),
		stringAtPath(raw, collector, "llm.provider", "llm", "provider"),
		stringAtPath(raw, collector, "llm.model_provider", "llm", "model_provider"),
		stringAtPath(raw, collector, "llm.default_provider", "llm", "default_provider"),
		stringAtPath(raw, collector, "llm.defaultProvider", "llm", "defaultProvider"),
	))
	if provider == "" && model != "" {
		provider = normalizeProvider(fallbackProvider)
	}

	if provider == "" {
		provider = inferProvider(model)
	}

	cfg := Config{
		DefaultModel: model,
	}
	if provider != "" {
		cfg.DefaultProvider = provider
	}

	baseURL := firstNonEmpty(
		stringAt(raw, collector, "base_url"),
		stringAt(raw, collector, "baseURL"),
		stringAt(raw, collector, "api_base"),
		stringAt(raw, collector, "apiBase"),
		stringAt(raw, collector, "api_url"),
		stringAt(raw, collector, "apiURL"),
		stringAtPath(raw, collector, "llm.base_url", "llm", "base_url"),
		stringAtPath(raw, collector, "llm.baseURL", "llm", "baseURL"),
		stringAtPath(raw, collector, "llm.api_base", "llm", "api_base"),
		stringAtPath(raw, collector, "llm.apiBase", "llm", "apiBase"),
		stringAtPath(raw, collector, "llm.api_url", "llm", "api_url"),
		stringAtPath(raw, collector, "llm.apiURL", "llm", "apiURL"),
	)
	if baseURL != "" {
		providerForBaseURL := provider
		if providerForBaseURL == "" {
			providerForBaseURL = normalizeProvider(fallbackProvider)
		}

		if providerForBaseURL != "" {
			setProvider(&cfg, providerForBaseURL, ProviderConfig{BaseURL: baseURL})
		}
	}

	diagnostics = append(diagnostics, collector.all()...)

	return cfg, diagnostics
}

func parseOpencodeConfig(path string, data []byte) Config {
	cfg, _ := parseOpencodeConfigWithDiagnostics(path, data)

	return cfg
}

func parseOpencodeConfigWithDiagnostics(path string, data []byte) (Config, []Diagnostic) {
	raw, diagnostics, ok := decodeJSONCMap(path, "opencode", data)
	if !ok {
		return Config{}, diagnostics
	}

	collector := newDiagnosticCollector("opencode", path)
	warnUnsupportedKeys(raw, openCodeTopLevelSchema, collector, "")

	model := firstNonEmpty(
		stringAt(raw, collector, "model"),
		stringAt(raw, collector, "default_model"),
		stringAt(raw, collector, "defaultModel"),
	)
	if model == "" {
		model = findOpenCodeFallbackModelWithDiagnostics(raw, collector)
	}

	provider := normalizeProvider(firstNonEmpty(
		stringAt(raw, collector, "default_provider"),
		stringAt(raw, collector, "defaultProvider"),
	))
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

	addOpenCodeProvidersWithDiagnostics(&cfg, raw, provider, collector)
	addOpenCodeAgentsWithDiagnostics(&cfg, filepath.Dir(path), raw, collector)

	if _, ok := raw["agents"]; ok {
		collector.warnf("agents", "legacy agents section is used only as a default-model fallback; agent definitions in this section are not imported")
	}

	if _, ok := raw["categories"]; ok {
		collector.warnf("categories", "categories section is used only as a default-model fallback")
	}

	diagnostics = append(diagnostics, collector.all()...)

	return cfg, diagnostics
}

func addOpenCodeProvidersWithDiagnostics(
	cfg *Config,
	raw map[string]any,
	defaultProvider string,
	collector *diagnosticCollector,
) {
	if providers, ok := mapAt(raw, collector, "provider"); ok {
		for _, name := range sortedMapKeys(providers) {
			value := providers[name]
			providerName := normalizeProvider(name)
			providerPath := joinPath("provider", name)

			nested, ok := value.(map[string]any)
			if !ok {
				collector.warnf(providerPath, "ignored provider section: expected object, got %s", typeName(value))

				continue
			}

			if providerName == "" {
				collector.warnf(providerPath, "ignored provider section with empty provider name")

				continue
			}

			warnUnsupportedKeys(nested, openCodeProviderSchema, collector, providerPath)

			baseURL := firstNonEmpty(
				stringAt(nested, collector, joinPath(providerPath, "base_url"), "base_url"),
				stringAt(nested, collector, joinPath(providerPath, "baseURL"), "baseURL"),
				stringAt(nested, collector, joinPath(providerPath, "api_base"), "api_base"),
				stringAt(nested, collector, joinPath(providerPath, "apiBase"), "apiBase"),
				stringAt(nested, collector, joinPath(providerPath, "api_url"), "api_url"),
				stringAt(nested, collector, joinPath(providerPath, "apiURL"), "apiURL"),
			)
			if baseURL != "" {
				setProvider(cfg, providerName, ProviderConfig{BaseURL: baseURL})
			}
		}
	}

	baseURL := firstNonEmpty(
		stringAt(raw, collector, "base_url"),
		stringAt(raw, collector, "baseURL"),
		stringAt(raw, collector, "api_base"),
		stringAt(raw, collector, "apiBase"),
		stringAt(raw, collector, "api_url"),
		stringAt(raw, collector, "apiURL"),
	)
	if defaultProvider != "" && baseURL != "" {
		setProvider(cfg, defaultProvider, ProviderConfig{BaseURL: baseURL})
	}
}

func addOpenCodeAgentsWithDiagnostics(cfg *Config, baseDir string, raw map[string]any, collector *diagnosticCollector) {
	agents, ok := mapAt(raw, collector, "agent")
	if !ok {
		return
	}

	for _, name := range sortedMapKeys(agents) {
		value := agents[name]

		nested, ok := value.(map[string]any)
		if !ok {
			collector.warnf(joinPath("agent", name), "ignored agent definition: expected object, got %s", typeName(value))

			continue
		}

		if agentCfg, ok := parseOpenCodeAgentMapWithDiagnostics(baseDir, name, nested, collector); ok {
			setAgent(cfg, name, agentCfg)
		}
	}
}

func loadOpencodeAgentDir(dir string) (Config, string, bool) {
	cfg, paths, _, ok := loadOpencodeAgentDirWithDiagnostics(dir)
	if !ok {
		return Config{}, "", false
	}

	return cfg, strings.Join(paths, ", "), true
}

func loadOpencodeAgentDirWithDiagnostics(dir string) (Config, []string, []Diagnostic, bool) {
	cfg, agents, diagnostics, ok := loadOpencodeAgentImportsWithDiagnostics(dir)
	if !ok {
		return Config{}, nil, diagnostics, false
	}

	paths := make([]string, 0, len(agents))
	for i := range agents {
		agent := &agents[i]
		paths = append(paths, agent.Path)
	}

	return cfg, paths, diagnostics, true
}

type openCodeAgentImport struct {
	Path   string
	Name   string
	Config AgentConfig
}

func loadOpencodeAgentImportsWithDiagnostics(dir string) (Config, []openCodeAgentImport, []Diagnostic, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil, nil, false
		}

		collector := newDiagnosticCollector("opencode", dir)
		collector.warnf("", "ignored agent directory: read directory: %v", err)

		return Config{}, nil, collector.all(), false
	}

	cfg := Config{}

	agents := make([]openCodeAgentImport, 0, len(entries))
	diagnostics := make([]Diagnostic, 0)

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

		agentCfg, fileDiagnostics, ok := parseOpenCodeAgentFileWithDiagnostics(path)
		diagnostics = append(diagnostics, fileDiagnostics...)

		if !ok {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		setAgent(&cfg, name, agentCfg)

		agents = append(agents, openCodeAgentImport{
			Path:   path,
			Name:   name,
			Config: agentCfg,
		})
	}

	if len(agents) == 0 {
		return Config{}, nil, diagnostics, false
	}

	return cfg, agents, diagnostics, true
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
	cfg, _, ok := parseOpenCodeAgentFileWithDiagnostics(path)

	return cfg, ok
}

func parseOpenCodeAgentFileWithDiagnostics(path string) (AgentConfig, []Diagnostic, bool) {
	collector := newDiagnosticCollector("opencode", path)

	// #nosec G304,G703 -- agent import intentionally reads discovered markdown agent files.
	data, err := os.ReadFile(path)
	if err != nil {
		collector.warnf("", "ignored agent file: read file: %v", err)

		return AgentConfig{}, collector.all(), false
	}

	frontmatter, body := splitFrontmatter(string(data))

	if strings.TrimSpace(frontmatter) == "" {
		if prompt := strings.TrimSpace(body); prompt != "" {
			return AgentConfig{SystemPrompt: prompt}, collector.all(), true
		}

		return AgentConfig{}, collector.all(), false
	}

	warnUnsupportedYAMLKeys([]byte(frontmatter), openCodeAgentFileSchema, collector, "frontmatter")

	var meta map[string]any
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		collector.warnf("frontmatter", "ignored agent file: parse frontmatter: %v", err)

		return AgentConfig{}, collector.all(), false
	}

	cfg, ok := parseOpenCodeAgentFrontmatterWithDiagnostics(filepath.Dir(path), meta, collector)
	if !ok {
		return AgentConfig{}, collector.all(), false
	}

	if prompt := strings.TrimSpace(body); prompt != "" {
		cfg.SystemPrompt = prompt
	}

	return cfg, collector.all(), !agentConfigEmpty(cfg)
}

func parseOpenCodeAgentFrontmatterWithDiagnostics(
	baseDir string,
	raw map[string]any,
	collector *diagnosticCollector,
) (AgentConfig, bool) {
	if disabled, ok := boolAt(raw, collector, "frontmatter.disable", "disable"); ok && disabled {
		collector.warnf("frontmatter.disable", "ignored disabled agent file")

		return AgentConfig{}, false
	}

	cfg := AgentConfig{
		Description:     strings.TrimSpace(stringAt(raw, collector, "frontmatter.description", "description")),
		Model:           strings.TrimSpace(stringAt(raw, collector, "frontmatter.model", "model")),
		Mode:            strings.TrimSpace(stringAt(raw, collector, "frontmatter.mode", "mode")),
		ModelMode:       strings.TrimSpace(stringAt(raw, collector, "frontmatter.model_mode", "model_mode", "modelMode")),
		ToolPermissions: toolPermissionsAt(raw, collector, "frontmatter.tools"),
	}

	if maxTokens, ok := nonNegativeIntAt(raw, collector, "frontmatter.max_tokens", "max_tokens", "maxTokens"); ok {
		cfg.MaxTokens = maxTokens
	}

	prompt, ok := resolvePromptReference(baseDir, stringAt(raw, collector, "frontmatter.prompt", "prompt"))
	if !ok {
		collector.warnf("frontmatter.prompt", "ignored agent file: prompt reference could not be resolved safely")

		return AgentConfig{}, false
	}

	cfg.SystemPrompt = prompt
	if hidden, ok := boolAt(raw, collector, "frontmatter.hidden", "hidden"); ok {
		cfg.Hidden = hidden
		cfg.hiddenSet = true
	}

	if value, ok := numberAt(raw, collector, "frontmatter.temperature", "temperature"); ok {
		cfg.Temperature = &value
	}

	if value, ok := numberAt(raw, collector, "frontmatter.top_p", "top_p", "topP"); ok {
		cfg.TopP = &value
	}

	return cfg, true
}

func parseOpenCodeAgentMapWithDiagnostics(
	baseDir, name string,
	raw map[string]any,
	collector *diagnosticCollector,
) (AgentConfig, bool) {
	agentPath := joinPath("agent", name)
	warnUnsupportedKeys(raw, openCodeAgentSchema, collector, agentPath)

	if disabled, ok := boolAt(raw, collector, joinPath(agentPath, "disable"), "disable"); ok && disabled {
		collector.warnf(joinPath(agentPath, "disable"), "ignored disabled agent definition")

		return AgentConfig{}, false
	}

	cfg := AgentConfig{
		Description: strings.TrimSpace(stringAt(raw, collector, joinPath(agentPath, "description"), "description")),
		Model:       strings.TrimSpace(stringAt(raw, collector, joinPath(agentPath, "model"), "model")),
		Mode:        strings.TrimSpace(stringAt(raw, collector, joinPath(agentPath, "mode"), "mode")),
		ModelMode:   strings.TrimSpace(stringAt(raw, collector, joinPath(agentPath, "model_mode"), "model_mode", "modelMode")),
	}

	if maxTokens, ok := nonNegativeIntAt(raw, collector, joinPath(agentPath, "max_tokens"), "max_tokens", "maxTokens"); ok {
		cfg.MaxTokens = maxTokens
	}

	cfg.ToolPermissions = toolPermissionsAt(raw, collector, joinPath(agentPath, "tools"))

	prompt, ok := resolvePromptReference(baseDir, stringAt(raw, collector, joinPath(agentPath, "prompt"), "prompt"))
	if !ok {
		collector.warnf(joinPath(agentPath, "prompt"), "ignored agent definition: prompt reference could not be resolved safely")

		return AgentConfig{}, false
	}

	cfg.SystemPrompt = prompt
	if hidden, ok := boolAt(raw, collector, joinPath(agentPath, "hidden"), "hidden"); ok {
		cfg.Hidden = hidden
		cfg.hiddenSet = true
	}

	if value, ok := numberAt(raw, collector, joinPath(agentPath, "temperature"), "temperature"); ok {
		cfg.Temperature = &value
	}

	if value, ok := numberAt(raw, collector, joinPath(agentPath, "top_p"), "top_p", "topP"); ok {
		cfg.TopP = &value
	}

	if strings.TrimSpace(name) == "" || agentConfigEmpty(cfg) {
		return AgentConfig{}, false
	}

	return cfg, true
}

func toolPermissionsAt(raw map[string]any, collector *diagnosticCollector, path string) map[string]bool {
	toolsRaw, ok := raw["tools"]
	if !ok {
		return nil
	}

	toolsMap, ok := toolsRaw.(map[string]any)
	if !ok {
		collector.warnf(path, "ignored tool permissions: expected object, got %s", typeName(toolsRaw))

		return nil
	}

	permissions := make(map[string]bool, len(toolsMap))
	for _, key := range sortedMapKeys(toolsMap) {
		value := toolsMap[key]

		boolValue, ok := value.(bool)
		if !ok {
			collector.warnf(joinPath(path, key), "ignored tool permission: expected boolean, got %s", typeName(value))

			continue
		}

		permissions[key] = boolValue
	}

	if len(toolsMap) > 0 && len(permissions) == 0 {
		return nil
	}

	return permissions
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
	return !agentConfigHasText(cfg) &&
		!agentConfigHasGeneration(cfg) &&
		!agentConfigHasCollections(cfg) &&
		!cfg.Hidden &&
		!cfg.hiddenSet
}

func agentConfigHasText(cfg AgentConfig) bool {
	return firstNonEmpty(
		cfg.Model,
		cfg.Mode,
		cfg.ModelMode,
		cfg.ReasoningLevel,
		cfg.Description,
		cfg.Personality,
		cfg.SystemPrompt,
	) != ""
}

func agentConfigHasGeneration(cfg AgentConfig) bool {
	return cfg.Temperature != nil ||
		cfg.TopP != nil ||
		cfg.Seed != nil ||
		cfg.MaxTokens != 0
}

func agentConfigHasCollections(cfg AgentConfig) bool {
	return cfg.ToolPermissions != nil ||
		cfg.FallbackModels != nil ||
		cfg.Capabilities != nil ||
		cfg.Triggers != nil ||
		cfg.References != nil
}

func decodeTOMLMap(source, importer string, data []byte) (map[string]any, []Diagnostic, bool) {
	collector := newDiagnosticCollector(importer, source)

	raw := map[string]any{}

	if err := toml.Unmarshal(data, &raw); err != nil {
		collector.warnf("", "ignored harness config: parse TOML: %v", err)

		return nil, collector.all(), false
	}

	return raw, collector.all(), true
}

func decodeJSONCMap(source, importer string, data []byte) (map[string]any, []Diagnostic, bool) {
	collector := newDiagnosticCollector(importer, source)

	ast, err := hujson.Parse(data)
	if err != nil {
		collector.warnf("", "ignored harness config: parse JSON/JSONC: %v", err)

		return nil, collector.all(), false
	}

	warnDuplicateJSONKeys(ast, collector, "")

	ast.Standardize()
	standard := ast.Pack()
	raw := map[string]any{}

	if err := json.Unmarshal(standard, &raw); err != nil {
		collector.warnf("", "ignored harness config: decode JSON object: %v", err)

		return nil, collector.all(), false
	}

	return raw, collector.all(), true
}

func warnDuplicateJSONKeys(value hujson.Value, collector *diagnosticCollector, prefix string) {
	switch typed := value.Value.(type) {
	case *hujson.Object:
		seen := map[string]struct{}{}

		for i := range typed.Members {
			member := &typed.Members[i]

			name, ok := jsonObjectMemberName(member.Name)
			if !ok {
				continue
			}

			path := joinPath(prefix, name)
			if _, exists := seen[name]; exists {
				collector.warnf(path, "duplicate object field: later value overrides earlier value")
			}

			seen[name] = struct{}{}

			warnDuplicateJSONKeys(member.Value, collector, path)
		}
	case *hujson.Array:
		for _, element := range typed.Elements {
			warnDuplicateJSONKeys(element, collector, prefix)
		}
	}
}

func jsonObjectMemberName(value hujson.Value) (string, bool) {
	literal, ok := value.Value.(hujson.Literal)
	if !ok {
		return "", false
	}

	var name string
	if err := json.Unmarshal(literal, &name); err != nil {
		return "", false
	}

	return name, true
}

func stringSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}

	return out
}

func warnUnsupportedKeys(raw map[string]any, allowed map[string]struct{}, collector *diagnosticCollector, prefix string) {
	if collector == nil || raw == nil {
		return
	}

	for _, key := range sortedMapKeys(raw) {
		if _, ok := allowed[key]; ok {
			continue
		}

		collector.warnf(joinPath(prefix, key), "ignored unsupported field")
	}
}

func warnUnsupportedYAMLKeys(data []byte, allowed map[string]struct{}, collector *diagnosticCollector, prefix string) {
	if collector == nil {
		return
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return
	}

	node := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		node = doc.Content[0]
	}

	if node.Kind != yaml.MappingNode {
		collector.warnf(prefix, "ignored frontmatter schema: expected mapping, got %s", yamlNodeKind(node.Kind))

		return
	}

	seen := map[string]struct{}{}

	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		path := joinPath(prefix, key)

		if _, ok := seen[key]; ok {
			collector.warnf(path, "duplicate frontmatter field")
		}

		seen[key] = struct{}{}

		if _, ok := allowed[key]; ok {
			continue
		}

		collector.warnf(path, "ignored unsupported field")
	}
}

func yamlNodeKind(kind yaml.Kind) string {
	switch kind {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return "unknown"
	}
}

func stringAt(raw map[string]any, collector *diagnosticCollector, keyOrPath string, keys ...string) string {
	key := keyOrPath
	path := keyOrPath

	if len(keys) > 0 {
		key = keys[0]
	}

	if raw == nil {
		return ""
	}

	value, ok := raw[key]
	if !ok {
		return ""
	}

	text, ok := value.(string)
	if ok {
		return strings.TrimSpace(text)
	}

	collector.warnf(path, "ignored value: expected string, got %s", typeName(value))

	return ""
}

func stringAtPath(raw map[string]any, collector *diagnosticCollector, path string, parts ...string) string {
	if len(parts) == 0 {
		return ""
	}

	current := raw
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			return ""
		}

		current = next
	}

	return stringAt(current, collector, path, parts[len(parts)-1])
}

func mapAt(raw map[string]any, collector *diagnosticCollector, key string) (map[string]any, bool) {
	if raw == nil {
		return nil, false
	}

	value, ok := raw[key]
	if !ok {
		return nil, false
	}

	nested, ok := value.(map[string]any)
	if !ok {
		collector.warnf(key, "ignored section: expected object/table, got %s", typeName(value))

		return nil, false
	}

	return nested, true
}

func joinPath(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		cleaned = append(cleaned, part)
	}

	return strings.Join(cleaned, ".")
}

func typeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case int, int8, int16, int32, int64, float32, float64, json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func findStringWithDiagnostics(raw map[string]any, collector *diagnosticCollector, prefix string, keys ...string) string {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			if s, ok := value.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					return s
				}

				continue
			}

			collector.warnf(joinPath(prefix, key), "ignored value: expected string, got %s", typeName(value))
		}
	}

	for _, name := range sortedMapKeys(raw) {
		value := raw[name]

		nested, ok := value.(map[string]any)
		if !ok {
			continue
		}

		if found := findStringWithDiagnostics(nested, collector, joinPath(prefix, name), keys...); found != "" {
			return found
		}
	}

	return ""
}

func boolAt(raw map[string]any, collector *diagnosticCollector, path string, keys ...string) (value, ok bool) {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}

		boolValue, ok := value.(bool)
		if !ok {
			collector.warnf(aliasDiagnosticPath(path, key, keys), "ignored value: expected boolean, got %s", typeName(value))

			return false, false
		}

		return boolValue, true
	}

	return false, false
}

func nonNegativeIntAt(raw map[string]any, collector *diagnosticCollector, path string, keys ...string) (value int, ok bool) {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}

		diagnosticPath := aliasDiagnosticPath(path, key, keys)

		var intValue int

		switch typed := value.(type) {
		case float64:
			if math.Trunc(typed) != typed {
				collector.warnf(diagnosticPath, "ignored value: expected integer, got %s", typeName(value))

				return 0, false
			}

			intValue = int(typed)
		case int:
			intValue = typed
		case int64:
			intValue = int(typed)
		default:
			collector.warnf(diagnosticPath, "ignored value: expected integer, got %s", typeName(value))

			return 0, false
		}

		if intValue < 0 {
			collector.warnf(diagnosticPath, "ignored value: expected non-negative integer, got %d", intValue)

			return 0, false
		}

		return intValue, true
	}

	return 0, false
}

func numberAt(raw map[string]any, collector *diagnosticCollector, path string, keys ...string) (value float64, ok bool) {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}

		switch typed := value.(type) {
		case float64:
			return typed, true
		case int:
			return float64(typed), true
		case int64:
			return float64(typed), true
		default:
			collector.warnf(aliasDiagnosticPath(path, key, keys), "ignored value: expected number, got %s", typeName(value))

			return 0, false
		}
	}

	return 0, false
}

func aliasDiagnosticPath(path, matchedKey string, keys []string) string {
	if len(keys) <= 1 || matchedKey == keys[0] {
		return path
	}

	canonicalKey := keys[0]
	if path == canonicalKey {
		return matchedKey
	}

	canonicalSuffix := "." + canonicalKey
	if prefix, ok := strings.CutSuffix(path, canonicalSuffix); ok {
		return prefix + "." + matchedKey
	}

	return path
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

	if provider.Retry.hasFields() {
		current.Retry = provider.Retry
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

func readHarnessSource(path, importer string) ([]byte, []Diagnostic, bool) {
	if path == "" {
		return nil, nil, false
	}

	// #nosec G304,G703 -- harness import intentionally reads conventional or caller-selected paths.
	data, err := os.ReadFile(path)
	if err == nil {
		return data, nil, true
	}

	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false
	}

	collector := newDiagnosticCollector(importer, path)
	collector.warnf("", "ignored harness config: read file: %v", err)

	return nil, collector.all(), false
}

func findOpenCodeFallbackModelWithDiagnostics(raw map[string]any, collector *diagnosticCollector) string {
	if model := findOpenCodeSectionModel(raw, collector, "categories",
		"deep", "ultrabrain", "quick", "unspecified-high", "unspecified-low"); model != "" {
		return model
	}

	if value, exists := raw["categories"]; exists {
		if _, ok := value.(map[string]any); !ok {
			collector.warnf("categories", "ignored default-model fallback section: expected object, got %s", typeName(value))
		}
	}

	model := findOpenCodeSectionModel(raw, collector, "agents", "oracle", "atlas", "explore")
	if model != "" {
		return model
	}

	if value, exists := raw["agents"]; exists {
		if _, ok := value.(map[string]any); !ok {
			collector.warnf("agents", "ignored default-model fallback section: expected object, got %s", typeName(value))
		}
	}

	return ""
}

func findOpenCodeSectionModel(
	raw map[string]any,
	collector *diagnosticCollector,
	section string,
	preferredNames ...string,
) string {
	nested, ok := raw[section].(map[string]any)
	if !ok {
		return ""
	}

	for _, name := range preferredNames {
		if model := findNamedModel(nested, collector, section, name); model != "" {
			return model
		}
	}

	return findFirstNestedModel(nested, collector, section, stringSet(preferredNames...))
}

func findNamedModel(raw map[string]any, collector *diagnosticCollector, prefix, name string) string {
	value, ok := raw[name]
	if !ok {
		return ""
	}

	nested, ok := value.(map[string]any)
	if !ok {
		collector.warnf(joinPath(prefix, name), "ignored default-model fallback entry: expected object, got %s", typeName(value))

		return ""
	}

	return findStringWithDiagnostics(nested, collector, joinPath(prefix, name), "model")
}

func findFirstNestedModel(raw map[string]any, collector *diagnosticCollector, prefix string, skip map[string]struct{}) string {
	for _, name := range sortedMapKeys(raw) {
		if _, ok := skip[name]; ok {
			continue
		}

		value := raw[name]

		nested, ok := value.(map[string]any)
		if !ok {
			collector.warnf(joinPath(prefix, name), "ignored default-model fallback entry: expected object, got %s", typeName(value))

			continue
		}

		if model := findStringWithDiagnostics(nested, collector, joinPath(prefix, name), "model"); model != "" {
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
