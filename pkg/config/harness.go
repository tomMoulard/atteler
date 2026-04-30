package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		mergeConfig(&cfg, next)
		loaded = append(loaded, path)
	}

	if len(cfg.Providers) == 0 {
		cfg.Providers = nil
	}
	return cfg, loaded
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
	paths := []string{
		configHomePath("opencode", "opencode.json"),
		configHomePath("opencode", "config.json"),
		homePath(".opencode.json"),
	}
	if cwd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(cwd, "opencode.json"))
	}

	for _, path := range paths {
		data, ok := readOptional(path)
		if !ok {
			continue
		}
		cfg := parseGenericJSONHarness(data, "")
		return cfg, path, !cfg.empty()
	}
	return Config{}, "", false
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
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
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
