package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/shell"
)

// modelsLoadedMsg is sent when one provider's model discovery completes.
type modelsLoadedMsg struct {
	err      error
	provider string
	items    []pickerItem
	fetchID  int
}

// fzfModelSelectedMsg is sent after the external fzf model picker exits.
type fzfModelSelectedMsg struct {
	err      error
	item     pickerItem
	selected bool
}

type modelPreferenceSavedMsg struct {
	err   error
	scope appconfig.ModelScope
}

type promptSuggestionPreferenceSavedMsg struct {
	err   error
	scope appconfig.ModelScope
}

// sessionSavedMsg is sent when a session save attempt completes.
type sessionSavedMsg struct {
	err error
}

type hookMsg struct {
	err  error
	line string // non-empty when the event should be printed by the TUI
}

// pickerItem represents one selectable entry in the model picker. An empty
// reasoning/modelMode means "do not change that override"; otherwise the value
// is one of the picker levels/modes (a mappable value or "default" to clear).
type pickerItem struct {
	provider  string
	model     string
	modelMode string
	reasoning string
}

// modelID returns the provider-qualified model identifier (no reasoning
// suffix). This is what gets stored as the active model and routed through the
// registry.
func (p pickerItem) modelID() string {
	if p.provider == "" {
		return p.model
	}

	return p.provider + "/" + p.model
}

// label returns the picker display string, including the reasoning suffix
// when set so each effort variant has a unique row in the picker.
func (p pickerItem) label() string {
	id := p.modelID()
	if p.reasoning == "" && pickerItemDisplayModelMode(p.modelMode) == "" {
		return id
	}

	if pickerItemDisplayModelMode(p.modelMode) == "" {
		return id + ":" + p.reasoning
	}

	parts := []string{id, "mode=" + pickerItemDisplayModelMode(p.modelMode)}
	if p.reasoning != "" {
		parts = append(parts, "effort="+p.reasoning)
	}

	return strings.Join(parts, ":")
}

func (m model) openModelPicker() (tea.Model, tea.Cmd, bool) {
	if m.waiting {
		return m, nil, false
	}

	// Cancel any in-flight model fetches from a previous picker open.
	if m.pickerCancel != nil {
		m.pickerCancel()
	}

	pickerCtx, pickerCancel := context.WithCancel(m.ctx) //nolint:gosec // cancel stored in m.pickerCancel, called on picker close

	m.pickerOpen = true
	m.pickerCancel = pickerCancel
	m.pickerItems = fallbackModelPickerItems(m.registry)
	m.pickerCursor = 0

	m.modelFetchID++
	if _, ok := findFZF(); ok {
		m.pickerLoading = true
		m.modelFetchesPending = 1

		return m, loadModelsForFZF(pickerCtx, m.registry, m.modelFetchID), true
	}

	providers := m.registry.ListProviders()
	sort.Strings(providers)
	m.modelFetchesPending = len(providers)
	m.pickerLoading = m.modelFetchesPending > 0

	return m, loadModels(pickerCtx, m.registry, providers, m.modelFetchID), true
}

func (m model) updatePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyEsc, keyCtrlC, "ctrl+o":
		m.pickerOpen = false
		m.pickerLoading = false
		m.modelFetchesPending = 0

		if m.pickerCancel != nil {
			m.pickerCancel()
			m.pickerCancel = nil
		}

		return m, nil

	case keyUp, "k":
		if m.pickerCursor > 0 {
			m.pickerCursor--
		}

	case keyDown, "j":
		if m.pickerCursor < len(m.pickerItems)-1 {
			m.pickerCursor++
		}

	case keyEnter:
		if len(m.pickerItems) > 0 {
			item := m.pickerItems[m.pickerCursor]
			m.pickerOpen = false

			return m.openModelScopePicker(item)
		}
	}

	return m, nil
}

func (m model) openModelScopePicker(item pickerItem) (tea.Model, tea.Cmd) {
	m.pendingModel = item
	m.scopePickerOpen = true

	return m, nil
}

func (m model) updateModelScopePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyEsc, keyCtrlC:
		m.scopePickerOpen = false
		m.pendingModel = pickerItem{}

		return m, nil
	case "1", "s", keyEnter:
		return m.selectModel(m.pendingModel, appconfig.ModelScopeSession)
	case "2", "f":
		return m.selectModel(m.pendingModel, appconfig.ModelScopeFolder)
	case "3", "g":
		return m.selectModel(m.pendingModel, appconfig.ModelScopeGlobal)
	}

	return m, nil
}

func (m model) selectModel(item pickerItem, scope appconfig.ModelScope) (tea.Model, tea.Cmd) {
	m.scopePickerOpen = false
	m.pendingModel = pickerItem{}
	m.selectedProvider = item.provider
	m.selectedModel = item.modelID()
	m.fallbackModels = nil
	m.modelLocked = true
	m.sessionState.DefaultModel = m.selectedModel

	reasoningSelected := item.reasoning != ""
	switch item.reasoning {
	case "":
		// No reasoning information attached — leave the override unchanged.
	case llm.ReasoningLevelDefault:
		m.generationOverrides.ReasoningLevel = ""
	default:
		m.generationOverrides.ReasoningLevel = item.reasoning
	}

	if reasoningSelected {
		m.sessionState.DefaultReasoningLevel = strings.TrimSpace(m.generationOverrides.ReasoningLevel)
	}

	modeSelected := item.modelMode != ""
	switch item.modelMode {
	case "":
		// No mode information attached — leave the override unchanged.
	case llm.ModelModeDefault:
		m.generationOverrides.ModelMode = llm.ModelModeDefault
	default:
		m.generationOverrides.ModelMode = item.modelMode
	}

	if modeSelected {
		m.sessionState.DefaultModelMode = strings.TrimSpace(m.generationOverrides.ModelMode)
	}

	persistedReasoningLevel := m.sessionState.DefaultReasoningLevel
	if item.reasoning == llm.ReasoningLevelDefault {
		persistedReasoningLevel = llm.ReasoningLevelDefault
	}

	persistedModelMode := m.sessionState.DefaultModelMode
	if item.modelMode == llm.ModelModeDefault {
		persistedModelMode = llm.ModelModeDefault
	}

	cmds := []tea.Cmd{tea.Println(
		dimStyle.Render("Model set to ") +
			pickerSelectedStyle.Render(pickerSelectionLabel(item)) +
			modelSelectionSuffix(item) +
			dimStyle.Render(" ("+modelScopeLabel(scope)+")"),
	)}
	if scope != appconfig.ModelScopeSession {
		// Persist before printing the final confirmation so a user can quit as
		// soon as they see "folder default" or "global default" without racing
		// the asynchronous Bubble Tea command runner.
		saveMsg := saveModelPreference(
			m.ctx,
			m.stateStore,
			m.cwd,
			m.selectedModel,
			persistedReasoningLevel,
			reasoningSelected,
			persistedModelMode,
			modeSelected,
			scope,
			m.hookRunner,
		)()

		cmds = append(cmds, func() tea.Msg { return saveMsg })
	}

	return m, tea.Batch(cmds...)
}

func pickerSelectionLabel(item pickerItem) string {
	return item.modelID()
}

func modelScopeLabel(scope appconfig.ModelScope) string {
	switch scope {
	case appconfig.ModelScopeFolder:
		return "folder default"
	case appconfig.ModelScopeGlobal:
		return "global default"
	default:
		return "session only"
	}
}

func (m model) viewPicker() string {
	var b strings.Builder

	b.WriteString(pickerHeaderStyle.Render("Select a model") +
		dimStyle.Render("  (j/k to move, Enter to select, Esc to cancel)") + "\n\n")

	if m.pickerLoading {
		b.WriteString(statusStyle.Render("  Refreshing models from providers...") + "\n\n")
	}

	if len(m.pickerItems) == 0 {
		b.WriteString(errStyle.Render("  No models available yet. Check your API keys."))
		return b.String()
	}

	currentProvider := ""
	for i, item := range m.pickerItems {
		// Print provider header when it changes.
		if item.provider != currentProvider {
			if currentProvider != "" {
				b.WriteString("\n")
			}

			currentProvider = item.provider
			b.WriteString("  " + pickerProviderStyle.Render(item.provider) + "\n")
		}

		cursor := "    "
		style := pickerNormalStyle

		if i == m.pickerCursor {
			cursor = "  > "
			style = pickerSelectedStyle
		}

		row := pickerRowLabel(item)

		b.WriteString(cursor + style.Render(row) + "\n")
	}

	return b.String()
}

func (m model) viewModelScopePicker() string {
	var b strings.Builder
	b.WriteString(pickerHeaderStyle.Render("Keep selected model?") + "\n\n")
	b.WriteString("  " + pickerSelectedStyle.Render(m.pendingModel.label()) + "\n\n")
	b.WriteString(pickerNormalStyle.Render("  1 / Enter  Session only") + "\n")
	b.WriteString(pickerNormalStyle.Render("  2 / f      This folder") + "\n")
	b.WriteString(pickerNormalStyle.Render("  3 / g      Globally") + "\n\n")
	b.WriteString(dimStyle.Render("  Esc cancels model selection"))

	return b.String()
}

// contextUsage returns a compact "ctx:~1.2k/200k" string showing the
// estimated token usage relative to the model's context window. Returns ""

func loadModels(ctx context.Context, reg *llm.Registry, providers []string, fetchID int) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(providers))
	for _, provider := range providers {
		cmds = append(cmds, func() tea.Msg {
			catalog, err := reg.ProviderModelCatalog(ctx, provider)
			if err != nil {
				return modelsLoadedMsg{provider: provider, fetchID: fetchID, err: err}
			}

			items := pickerItemsForProviderCatalogWithRegistry(reg, provider, catalog)
			if len(items) == 0 {
				return modelsLoadedMsg{provider: provider, fetchID: fetchID, err: fmt.Errorf("no models available from %s", provider)}
			}

			return modelsLoadedMsg{provider: provider, fetchID: fetchID, items: items}
		})
	}

	return tea.Batch(cmds...)
}

func loadModelsForFZF(ctx context.Context, reg *llm.Registry, fetchID int) tea.Cmd {
	return func() tea.Msg {
		providers := reg.ListProviders()
		sort.Strings(providers)

		var items []pickerItem

		for _, provider := range providers {
			catalog, err := reg.ProviderModelCatalog(ctx, provider)
			if err != nil {
				continue
			}

			items = append(items, pickerItemsForProviderCatalogWithRegistry(reg, provider, catalog)...)
		}

		if len(items) == 0 {
			return modelsLoadedMsg{fetchID: fetchID, err: errors.New("no models available from any provider")}
		}

		return modelsLoadedMsg{fetchID: fetchID, items: sortPickerItems(items)}
	}
}

func fallbackModelPickerItems(reg *llm.Registry) []pickerItem {
	providers := reg.ListProviders()
	sort.Strings(providers)

	items := make([]pickerItem, 0)

	for _, providerName := range providers {
		provider, ok := reg.Provider(providerName)
		if !ok {
			continue
		}

		items = append(items, pickerItemsForProvider(providerName, provider.Models())...)
	}

	return sortPickerItems(items)
}

func pickerItemsForProviderCatalog(provider string, catalog llm.ProviderModelCatalog) []pickerItem {
	return pickerItemsForProviderCatalogWithModeResolver(provider, catalog, llm.ModelModePickerModes)
}

func pickerItemsForProviderCatalogWithRegistry(reg *llm.Registry, provider string, catalog llm.ProviderModelCatalog) []pickerItem {
	return pickerItemsForProviderCatalogWithModeResolver(provider, catalog, func(itemProvider, modelName string) []string {
		if catalog.ModelProvenance[modelName] == llm.ModelProvenanceConfiguredAlias && reg != nil {
			diagnostic := reg.ExplainModelResolution(modelName)
			if diagnostic.Resolved() {
				return llm.ModelModePickerModes(diagnostic.ProviderName, diagnostic.ProviderModel)
			}
		}

		return llm.ModelModePickerModes(itemProvider, modelName)
	})
}

func pickerItemsForProviderCatalogWithModeResolver(
	provider string,
	catalog llm.ProviderModelCatalog,
	modesFor func(itemProvider, modelName string) []string,
) []pickerItem {
	models := append([]string(nil), catalog.Models...)
	sort.Strings(models)

	levels := llm.ReasoningPickerLevels()

	items := make([]pickerItem, 0, len(models)*len(levels))
	for _, modelName := range models {
		if modelName == "" {
			continue
		}

		itemProvider := provider
		if catalog.ModelProvenance[modelName] == llm.ModelProvenanceConfiguredAlias {
			itemProvider = ""
		}

		for _, mode := range modesFor(itemProvider, modelName) {
			for _, level := range levels {
				items = append(items, pickerItem{provider: itemProvider, model: modelName, modelMode: mode, reasoning: level})
			}
		}
	}

	return items
}

func pickerItemsForProvider(provider string, models []string) []pickerItem {
	models = append([]string(nil), models...)
	sort.Strings(models)

	levels := llm.ReasoningPickerLevels()

	items := make([]pickerItem, 0, len(models)*len(levels))
	for _, modelName := range models {
		if modelName == "" {
			continue
		}

		for _, mode := range llm.ModelModePickerModes(provider, modelName) {
			for _, level := range levels {
				items = append(items, pickerItem{provider: provider, model: modelName, modelMode: mode, reasoning: level})
			}
		}
	}

	return items
}

func mergeProviderPickerItems(items []pickerItem, provider string, providerItems []pickerItem) []pickerItem {
	merged := make([]pickerItem, 0, len(items)+len(providerItems))
	for _, item := range items {
		if item.provider != provider {
			merged = append(merged, item)
		}
	}

	merged = append(merged, providerItems...)

	return sortPickerItems(merged)
}

func sortPickerItems(items []pickerItem) []pickerItem {
	sort.Slice(items, func(i, j int) bool {
		if items[i].provider != items[j].provider {
			return items[i].provider < items[j].provider
		}

		if items[i].model != items[j].model {
			return items[i].model < items[j].model
		}

		if llm.ModelModeRank(items[i].modelMode) != llm.ModelModeRank(items[j].modelMode) {
			return llm.ModelModeRank(items[i].modelMode) < llm.ModelModeRank(items[j].modelMode)
		}

		return llm.ReasoningEffortRank(items[i].reasoning) < llm.ReasoningEffortRank(items[j].reasoning)
	})

	return items
}

var execLookPath = exec.LookPath

func findFZF() (string, bool) {
	path, err := execLookPath("fzf")
	if err != nil {
		return "", false
	}

	return path, true
}

func runFZFModelPicker(ctx context.Context, fzfPath string, items []pickerItem) tea.Cmd {
	var stdout bytes.Buffer

	input := fzfInput(items)

	cmd, invocation, err := shell.CommandContext(ctx, shell.CommandOptions{
		Program: fzfPath,
		Args: []string{
			"--prompt", "atteler model> ",
			"--height", "80%",
			"--border",
			"--delimiter", "\t",
			"--with-nth", "1",
		},
		Stdin:  strings.NewReader(input),
		Stdout: &stdout,
		Mode:   shell.ModeInteractive,
		Audit:  shell.AuditContext{Caller: "atteler.fzf_model_picker"},
	})
	if err != nil {
		return func() tea.Msg { return fzfModelSelectedMsg{err: err} }
	}

	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if finishErr := invocation.Finish(shell.FinishOptions{Stdout: stdout.String(), Error: err, OutputCapture: shell.OutputCaptured}); finishErr != nil && err == nil {
			err = finishErr
		}

		if item, ok := parseFZFSelection(stdout.String(), items); ok {
			return fzfModelSelectedMsg{item: item, selected: true}
		}

		if err != nil {
			return fzfModelSelectedMsg{}
		}

		return fzfModelSelectedMsg{}
	})
}

func fzfInput(items []pickerItem) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString(item.label())
		b.WriteString("\t")
		b.WriteString(item.provider)
		b.WriteString("\t")
		b.WriteString(item.model)
		b.WriteString("\t")
		b.WriteString(item.reasoning)
		b.WriteString("\t")
		b.WriteString(item.modelMode)
		b.WriteString("\n")
	}

	return b.String()
}

func parseFZFSelection(selection string, items []pickerItem) (pickerItem, bool) {
	selection = strings.TrimSpace(selection)
	if selection == "" {
		return pickerItem{}, false
	}

	fields := strings.Split(selection, "\t")
	label := fields[0]

	for _, item := range items {
		if item.label() == label {
			return item, true
		}
	}

	if len(fields) >= 3 {
		provider := strings.TrimSpace(fields[1])
		model := strings.TrimSpace(fields[2])
		reasoning := ""

		if len(fields) >= 4 {
			reasoning = strings.TrimSpace(fields[3])
		}

		modelMode := ""
		if len(fields) >= 5 {
			modelMode = strings.TrimSpace(fields[4])
		}

		if item, ok := findPickerItemByColumns(items, provider, model, reasoning, modelMode); ok {
			return item, true
		}
	}

	return pickerItem{}, false
}

func findPickerItemByColumns(items []pickerItem, provider, model, reasoning, modelMode string) (pickerItem, bool) {
	search := pickerItemColumnSearch{
		provider:  provider,
		model:     model,
		reasoning: reasoning,
		modelMode: modelMode,
	}

	if item, ok := findPickerItem(items, search.exact); ok {
		return item, true
	}

	if modelMode != "" {
		return pickerItem{}, false
	}

	if reasoning != "" {
		return findPickerItem(items, search.defaultMode)
	}

	if item, ok := findPickerItem(items, search.defaultReasoning); ok {
		return item, true
	}

	return findPickerItem(items, search.legacyNoReasoning)
}

func findPickerItem(items []pickerItem, matches func(pickerItem) bool) (pickerItem, bool) {
	for _, item := range items {
		if matches(item) {
			return item, true
		}
	}

	return pickerItem{}, false
}

type pickerItemColumnSearch struct {
	provider  string
	model     string
	reasoning string
	modelMode string
}

func (s pickerItemColumnSearch) exact(item pickerItem) bool {
	return s.sameModel(item) && item.reasoning == s.reasoning && item.modelMode == s.modelMode
}

func (s pickerItemColumnSearch) defaultMode(item pickerItem) bool {
	return s.sameModel(item) && item.reasoning == s.reasoning && item.modelMode == llm.ModelModeDefault
}

func (s pickerItemColumnSearch) defaultReasoning(item pickerItem) bool {
	if !s.sameModel(item) || item.reasoning != llm.ReasoningLevelDefault {
		return false
	}

	return item.modelMode == llm.ModelModeDefault || item.modelMode == ""
}

func (s pickerItemColumnSearch) legacyNoReasoning(item pickerItem) bool {
	return s.sameModel(item) && item.reasoning == ""
}

func (s pickerItemColumnSearch) sameModel(item pickerItem) bool {
	return item.provider == s.provider && item.model == s.model
}

func pickerItemDisplayModelMode(mode string) string {
	if mode == llm.ModelModeDefault {
		return ""
	}

	return strings.TrimSpace(mode)
}

func pickerRowLabel(item pickerItem) string {
	row := item.model
	if mode := pickerItemDisplayModelMode(item.modelMode); mode != "" {
		row += ":mode=" + mode
		if item.reasoning != "" {
			row += ":effort=" + item.reasoning
		}

		return row
	}

	if item.reasoning != "" {
		row += ":" + item.reasoning
	}

	return row
}

func modelSelectionSuffix(item pickerItem) string {
	parts := make([]string, 0, 2)
	if mode := pickerItemDisplayModelMode(item.modelMode); mode != "" {
		parts = append(parts, "mode="+mode)
	}

	if item.reasoning != "" {
		parts = append(parts, "effort="+item.reasoning)
	}

	if len(parts) == 0 {
		return ""
	}

	return dimStyle.Render(":") + pickerSelectedStyle.Render(strings.Join(parts, ":"))
}
