package repository

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

var requiredIssueTemplates = []string{
	"audit_finding.yml",
	"bug.yml",
	"feature.yml",
	"provider_integration.yml",
	"security_sensitive_concern.yml",
	"symphony_dispatch.yml",
}

const (
	issueFieldIDAcceptanceCriteria = "acceptance_criteria"
	issueFieldIDArea               = "area"
	issueFieldIDAutomationDispatch = "automation_dispatch"
	issueFieldIDCurrentBehavior    = "current_behavior"
	issueFieldIDDesiredBehavior    = "desired_behavior"
	issueFieldIDDuplicates         = "duplicates"
	issueFieldIDEvidence           = "evidence"
	issueFieldIDPriority           = "priority"
	issueFieldIDProvider           = "provider"
	issueFieldIDRisk               = "risk"
	issueFieldIDScope              = "scope"
	issueFieldIDStatus             = "status"
	issueFieldIDSummary            = "summary"
	issueFieldIDVerification       = "verification"

	issueFieldTypeCheckboxes = "checkboxes"
	issueFieldTypeDropdown   = "dropdown"
	issueFieldTypeInput      = "input"
	issueFieldTypeMarkdown   = "markdown"
	issueFieldTypeTextarea   = "textarea"
)

var projectFieldIDs = map[string]string{
	"Status":   issueFieldIDStatus,
	"Priority": issueFieldIDPriority,
	"Area":     issueFieldIDArea,
	"Risk":     issueFieldIDRisk,
}

var expectedProjectFieldNames = []string{"Area", "Priority", "Risk", "Status"}

var issueFieldIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var allowedIssueFieldTypes = map[string]struct{}{
	issueFieldTypeCheckboxes: {},
	issueFieldTypeDropdown:   {},
	issueFieldTypeInput:      {},
	issueFieldTypeMarkdown:   {},
	issueFieldTypeTextarea:   {},
}

var supportedIssueLabels = map[string]struct{}{
	"architecture":     {},
	"bug":              {},
	"debt":             {},
	"documentation":    {},
	"duplicate":        {},
	"enhancement":      {},
	"good first issue": {},
	"help wanted":      {},
	"invalid":          {},
	"provider":         {},
	"quality":          {},
	"question":         {},
	"rag":              {},
	"roadmap":          {},
	"security":         {},
	"symphony":         {},
	"ux":               {},
	"wontfix":          {},
}

type projectVocabulary struct {
	Fields  map[string][]string `yaml:"fields"`
	Project string              `yaml:"project"`
	Owner   string              `yaml:"owner"`
	Number  int                 `yaml:"number"`
}

type issueTemplateConfig struct {
	BlankIssuesEnabled *bool `yaml:"blank_issues_enabled"`
	ContactLinks       []struct {
		Name  string `yaml:"name"`
		URL   string `yaml:"url"`
		About string `yaml:"about"`
	} `yaml:"contact_links"`
}

type issueTemplate struct {
	Name        string       `yaml:"name"`
	Description string       `yaml:"description"`
	Title       string       `yaml:"title"`
	Labels      []string     `yaml:"labels"`
	Projects    []string     `yaml:"projects"`
	Body        []issueField `yaml:"body"`
}

type workflow struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Steps []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

type issueField struct {
	Type        string          `yaml:"type"`
	ID          string          `yaml:"id"`
	Attributes  issueAttributes `yaml:"attributes"`
	Validations struct {
		Required bool `yaml:"required"`
	} `yaml:"validations"`
}

type issueAttributes struct {
	Default     *int          `yaml:"default"`
	Description string        `yaml:"description"`
	Label       string        `yaml:"label"`
	Placeholder string        `yaml:"placeholder"`
	Value       string        `yaml:"value"`
	Options     []issueOption `yaml:"options"`
}

type issueOption struct {
	Value    string
	Label    string
	Required bool
}

func TestIssueTemplateCatalogIsComplete(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)
	templateDir := filepath.Join(root, ".github", "ISSUE_TEMPLATE")

	assert.DirExists(t, templateDir)
	assert.FileExists(t, filepath.Join(templateDir, "config.yml"))
	assert.FileExists(t, filepath.Join(root, ".github", "ISSUE_TRIAGE.md"))
	assert.FileExists(t, filepath.Join(root, ".github", "project-fields.yml"))

	paths, err := filepath.Glob(filepath.Join(templateDir, "*.yml"))
	require.NoError(t, err)

	var names []string

	for _, path := range paths {
		name := filepath.Base(path)
		if name == "config.yml" {
			continue
		}

		names = append(names, name)
	}

	sort.Strings(names)
	assert.Equal(t, requiredIssueTemplates, names)

	seenNames := map[string]struct{}{}
	seenDescriptions := map[string]struct{}{}

	for _, name := range requiredIssueTemplates {
		form := loadYAMLFile[issueTemplate](t, filepath.Join(templateDir, name))
		assert.NotEmpty(t, form.Name, name)
		assert.Greater(t, len([]rune(form.Name)), 3, "%s issue-form name must be long enough to appear in GitHub's template chooser", name)
		assert.NotEmpty(t, form.Description, name)
		assert.NotEmpty(t, form.Title, name)
		assert.NotEmpty(t, form.Labels, name)
		assert.NotEmpty(t, form.Body, name)

		_, nameExists := seenNames[form.Name]
		assert.False(t, nameExists, "%s should use a unique template chooser name", name)

		seenNames[form.Name] = struct{}{}

		_, descriptionExists := seenDescriptions[form.Description]
		assert.False(t, descriptionExists, "%s should use a unique template chooser description", name)

		seenDescriptions[form.Description] = struct{}{}
	}
}

func TestIssueTemplatesHaveValidFormShape(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)

	for _, templateName := range requiredIssueTemplates {
		form := loadYAMLFile[issueTemplate](t, filepath.Join(root, ".github", "ISSUE_TEMPLATE", templateName))
		seenIDs := map[string]struct{}{}
		seenLabels := map[string]struct{}{}

		for i := range form.Body {
			field := &form.Body[i]

			require.Contains(t, allowedIssueFieldTypes, field.Type, "%s field %d should use a supported GitHub issue-form type", templateName, i)

			if field.Type == issueFieldTypeMarkdown {
				assert.NotEmpty(t, strings.TrimSpace(field.Attributes.Value), "%s markdown field %d should have body text", templateName, i)

				continue
			}

			require.NotEmpty(t, field.ID, "%s field %d should have an id", templateName, i)
			assert.Regexp(t, issueFieldIDPattern, field.ID, "%s field %d should use a GitHub issue-form-safe id", templateName, i)
			require.NotEmpty(t, field.Attributes.Label, "%s field %q should have a label", templateName, field.ID)

			_, exists := seenIDs[field.ID]
			assert.False(t, exists, "%s field id %q should be unique", templateName, field.ID)
			seenIDs[field.ID] = struct{}{}

			_, labelExists := seenLabels[field.Attributes.Label]
			assert.False(t, labelExists, "%s field label %q should be unique", templateName, field.Attributes.Label)
			seenLabels[field.Attributes.Label] = struct{}{}

			switch field.Type {
			case issueFieldTypeInput, issueFieldTypeTextarea:
				assert.NotEmpty(t, strings.TrimSpace(field.Attributes.Description), "%s %s should explain what evidence to provide", templateName, field.ID)
				assert.NotEmpty(t, strings.TrimSpace(field.Attributes.Placeholder), "%s %s should include a placeholder prompt", templateName, field.ID)
			case issueFieldTypeDropdown:
				assert.NotEmpty(t, strings.TrimSpace(field.Attributes.Description), "%s %s should explain how to choose an option", templateName, field.ID)

				options := scalarOptions(t, field)
				require.NotEmpty(t, options, "%s dropdown %q should have options", templateName, field.ID)

				if field.Attributes.Default != nil {
					assert.GreaterOrEqual(t, *field.Attributes.Default, 0, "%s dropdown %q default should be a valid option index", templateName, field.ID)
					assert.Less(t, *field.Attributes.Default, len(options), "%s dropdown %q default should be a valid option index", templateName, field.ID)
				}
			case issueFieldTypeCheckboxes:
				labels := checkboxLabels(t, field)
				require.NotEmpty(t, labels, "%s checkboxes %q should have options", templateName, field.ID)

				for _, label := range labels {
					assert.NotEmpty(t, strings.TrimSpace(label), "%s checkboxes %q should not contain empty option labels", templateName, field.ID)
				}
			}
		}
	}
}

func TestIssueTemplatesUseProjectVocabulary(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)
	vocabulary := loadYAMLFile[projectVocabulary](t, filepath.Join(root, ".github", "project-fields.yml"))

	assert.Equal(t, "atteler roadmap", vocabulary.Project)
	assert.Equal(t, "tomMoulard", vocabulary.Owner)
	assert.Equal(t, 3, vocabulary.Number)
	assert.Equal(t, expectedProjectFieldNames, sortedKeys(vocabulary.Fields), "project vocabulary should only contain checked routing fields")

	expectedProject := fmt.Sprintf("%s/%d", vocabulary.Owner, vocabulary.Number)

	for _, templateName := range requiredIssueTemplates {
		form := loadYAMLFile[issueTemplate](t, filepath.Join(root, ".github", "ISSUE_TEMPLATE", templateName))
		assert.Contains(t, form.Projects, expectedProject, "%s should add new issues to the supported roadmap project", templateName)
	}

	for fieldName, id := range projectFieldIDs {
		want := vocabulary.Fields[fieldName]
		require.NotEmpty(t, want, "project field %s should have supported values", fieldName)

		for _, templateName := range requiredIssueTemplates {
			form := loadYAMLFile[issueTemplate](t, filepath.Join(root, ".github", "ISSUE_TEMPLATE", templateName))
			field, ok := fieldsByID(form)[id]

			require.True(t, ok, "%s should include %s dropdown", templateName, id)
			require.Equal(t, issueFieldTypeDropdown, field.Type, "%s %s should be a dropdown", templateName, id)
			assert.Equal(t, want, scalarOptions(t, field), "%s %s options must match .github/project-fields.yml", templateName, id)
		}
	}

	guide := readTextFile(t, filepath.Join(root, ".github", "ISSUE_TRIAGE.md"))

	for fieldName, options := range vocabulary.Fields {
		for _, option := range options {
			assert.Contains(t, guide, option, "triage guide should document %s option %q", fieldName, option)
		}
	}
}

func TestMaintainerTriageGuideCoversLabelsDefaultsAndDispatch(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)
	guide := readTextFile(t, filepath.Join(root, ".github", "ISSUE_TRIAGE.md"))

	assert.Contains(t, guide, "## Label triage")
	assert.Contains(t, guide, "Default for new issues")
	assert.Contains(t, guide, "Roadmap membership vs. Symphony dispatch")
	assert.Contains(t, guide, "does not request or authorize automation")
	assert.Contains(t, guide, "Apply `symphony` only")

	for _, templateName := range requiredIssueTemplates {
		form := loadYAMLFile[issueTemplate](t, filepath.Join(root, ".github", "ISSUE_TEMPLATE", templateName))

		for _, label := range form.Labels {
			assert.Contains(t, guide, "`"+label+"`", "triage guide should document template label %q from %s", label, templateName)
		}
	}
}

func TestIssueTemplateLabelsUseSupportedVocabulary(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)

	for _, templateName := range requiredIssueTemplates {
		form := loadYAMLFile[issueTemplate](t, filepath.Join(root, ".github", "ISSUE_TEMPLATE", templateName))

		for _, label := range form.Labels {
			assert.Contains(t, supportedIssueLabels, label, "%s should only use supported repository labels", templateName)
		}
	}
}

func TestIssueTemplatesCollectRoutingAndDispatchData(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)
	requiredPromptIDs := []string{
		issueFieldIDSummary,
		issueFieldIDEvidence,
		issueFieldIDCurrentBehavior,
		issueFieldIDDesiredBehavior,
		issueFieldIDScope,
		issueFieldIDArea,
		issueFieldIDProvider,
		issueFieldIDStatus,
		issueFieldIDPriority,
		issueFieldIDRisk,
		issueFieldIDVerification,
		issueFieldIDAcceptanceCriteria,
		issueFieldIDDuplicates,
		issueFieldIDAutomationDispatch,
	}

	for _, templateName := range requiredIssueTemplates {
		form := loadYAMLFile[issueTemplate](t, filepath.Join(root, ".github", "ISSUE_TEMPLATE", templateName))
		fields := fieldsByID(form)

		for _, id := range requiredPromptIDs {
			field, ok := fields[id]

			require.True(t, ok, "%s should collect %s", templateName, id)
			assert.True(t, fieldIsRequired(field), "%s %s should be required", templateName, id)
		}

		duplicateSearch := fields[issueFieldIDDuplicates]
		require.Equal(t, issueFieldTypeCheckboxes, duplicateSearch.Type, "%s duplicate search should require an explicit checkbox", templateName)
		assert.Contains(t, strings.ToLower(strings.Join(checkboxLabels(t, duplicateSearch), "\n")), "searched existing issues")
		assert.True(t, allCheckboxOptionsRequired(duplicateSearch), "%s duplicate search checkboxes should be required", templateName)

		if templateName == "feature.yml" {
			field, ok := fields["request_type"]
			require.True(t, ok, "feature form should distinguish feature behavior from documentation work")
			require.Equal(t, issueFieldTypeDropdown, field.Type, "feature request_type should be a dropdown")
			assert.True(t, fieldIsRequired(field), "feature request_type should be required")
			assert.Equal(t, []string{
				"Feature behavior",
				"Documentation/help text",
				"Both feature and documentation",
			}, scalarOptions(t, field))
		}

		if templateName == "symphony_dispatch.yml" {
			assert.Contains(t, form.Labels, "symphony", "dispatch form must apply the dispatch label")

			field, ok := fields["dispatch_opt_in"]
			require.True(t, ok, "dispatch form should require explicit opt-in checkboxes")
			require.Equal(t, issueFieldTypeCheckboxes, field.Type, "dispatch opt-in should be checkboxes")
			assert.True(t, fieldIsRequired(field), "dispatch opt-in should be required")
			assert.True(t, allCheckboxOptionsRequired(field), "all dispatch opt-in confirmations should be required")
			assert.Contains(t, strings.Join(checkboxLabels(t, field), "\n"), "I explicitly request Symphony automation")

			dispatchIntent := fields[issueFieldIDAutomationDispatch]
			assert.Equal(t, issueFieldTypeTextarea, dispatchIntent.Type, "dispatch form should require prose safety rationale")

			continue
		}

		assert.NotContains(t, form.Labels, "symphony", "%s should not imply automation dispatch", templateName)

		options := scalarOptions(t, fields[issueFieldIDAutomationDispatch])
		require.NotEmpty(t, options, "%s automation_dispatch should have options", templateName)
		assert.True(t, strings.HasPrefix(options[0], "No"), "%s first automation option should be no dispatch", templateName)
		assert.Equal(t, options[0], selectedDefaultOption(t, fields[issueFieldIDAutomationDispatch]), "%s automation_dispatch should default to no dispatch", templateName)

		for _, option := range options {
			assert.False(t, strings.HasPrefix(option, "Yes"), "%s non-dispatch forms must not offer direct Symphony dispatch", templateName)
		}
	}
}

func TestIssueTemplatesDefaultNewIssuesToTodo(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)

	for _, templateName := range requiredIssueTemplates {
		form := loadYAMLFile[issueTemplate](t, filepath.Join(root, ".github", "ISSUE_TEMPLATE", templateName))
		status := fieldsByID(form)[issueFieldIDStatus]

		require.NotNil(t, status, "%s should include status", templateName)
		assert.Equal(t, "Todo", selectedDefaultOption(t, status), "%s should default new issue status to Todo", templateName)
	}
}

func TestIssueTemplateChooserRoutesSecurityReportsPrivately(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)
	config := loadYAMLFile[issueTemplateConfig](t, filepath.Join(root, ".github", "ISSUE_TEMPLATE", "config.yml"))

	var (
		securityLinkFound    bool
		triageGuideLinkFound bool
	)

	for _, link := range config.ContactLinks {
		if strings.Contains(link.URL, "/security/advisories/new") {
			securityLinkFound = true

			assert.Contains(t, strings.ToLower(link.About), "private")
		}

		if strings.Contains(link.URL, "/.github/ISSUE_TRIAGE.md") {
			triageGuideLinkFound = true

			assert.Contains(t, strings.ToLower(link.Name+" "+link.About), "triage")
		}
	}

	require.NotNil(t, config.BlankIssuesEnabled, "template chooser should explicitly configure blank issues")
	assert.False(t, *config.BlankIssuesEnabled, "blank issues should stay disabled so structured intake is used")
	assert.True(t, securityLinkFound, "template chooser should point sensitive reports to private vulnerability reporting")
	assert.True(t, triageGuideLinkFound, "template chooser should point maintainers to the triage guide")

	form := loadYAMLFile[issueTemplate](t, filepath.Join(root, ".github", "ISSUE_TEMPLATE", "security_sensitive_concern.yml"))
	field, ok := fieldsByID(form)["disclosure_safety"]

	require.True(t, ok, "security-sensitive form should require public disclosure safety confirmation")
	assert.True(t, fieldIsRequired(field), "public disclosure safety confirmation should be required")
	assert.Contains(t, strings.ToLower(strings.Join(checkboxLabels(t, field), "\n")), "safe to discuss publicly")
}

func TestCIWorkflowRunsIssueTemplateCheck(t *testing.T) {
	t.Parallel()

	root := repositoryRoot(t)
	ci := loadYAMLFile[workflow](t, filepath.Join(root, ".github", "workflows", "ci.yml"))

	var found bool

	for _, job := range ci.Jobs {
		for _, step := range job.Steps {
			if step.Name == "Issue template vocabulary check" && step.Run == "go test ./test/repository -count=1" {
				found = true
			}
		}
	}

	assert.True(t, found, "CI should run the repository issue-template vocabulary check")
}

func repositoryRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime caller should locate test file")

	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func loadYAMLFile[T any](t *testing.T, path string) T {
	t.Helper()

	contents, err := os.ReadFile(path)
	require.NoError(t, err)

	var value T
	require.NoError(t, yaml.Unmarshal(contents, &value), path)

	return value
}

func readTextFile(t *testing.T, path string) string {
	t.Helper()

	contents, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(contents)
}

func fieldsByID(form issueTemplate) map[string]*issueField {
	fields := map[string]*issueField{}

	for i := range form.Body {
		field := &form.Body[i]
		if field.ID == "" {
			continue
		}

		fields[field.ID] = field
	}

	return fields
}

func sortedKeys(values map[string][]string) []string {
	keys := make([]string, 0, len(values))

	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

func scalarOptions(t *testing.T, field *issueField) []string {
	t.Helper()

	require.NotEmpty(t, field.Attributes.Options, "%s should define scalar options", field.ID)

	var options []string

	for _, option := range field.Attributes.Options {
		require.NotEmpty(t, option.Value, "%s option should be scalar", field.ID)

		options = append(options, option.Value)
	}

	return options
}

func selectedDefaultOption(t *testing.T, field *issueField) string {
	t.Helper()

	options := scalarOptions(t, field)
	require.NotNil(t, field.Attributes.Default, "%s should define a default selected option", field.ID)
	require.GreaterOrEqual(t, *field.Attributes.Default, 0, "%s default should be non-negative", field.ID)
	require.Less(t, *field.Attributes.Default, len(options), "%s default should reference an option", field.ID)

	return options[*field.Attributes.Default]
}

func checkboxLabels(t *testing.T, field *issueField) []string {
	t.Helper()

	require.NotEmpty(t, field.Attributes.Options, "%s should define checkbox options", field.ID)

	var labels []string

	for _, option := range field.Attributes.Options {
		require.NotEmpty(t, option.Label, "%s checkbox option should be a mapping with a label", field.ID)

		labels = append(labels, option.Label)
	}

	return labels
}

func fieldIsRequired(field *issueField) bool {
	if field.Validations.Required {
		return true
	}

	if field.Type != issueFieldTypeCheckboxes || len(field.Attributes.Options) == 0 {
		return false
	}

	for _, option := range field.Attributes.Options {
		if option.Required {
			return true
		}
	}

	return false
}

func allCheckboxOptionsRequired(field *issueField) bool {
	if field.Type != issueFieldTypeCheckboxes || len(field.Attributes.Options) == 0 {
		return false
	}

	for _, option := range field.Attributes.Options {
		if !option.Required {
			return false
		}
	}

	return true
}

func (option *issueOption) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		option.Value = node.Value
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			value := node.Content[i+1]

			switch key {
			case "label":
				option.Label = value.Value
			case "required":
				option.Required = value.Value == "true"
			}
		}
	}

	return nil
}
