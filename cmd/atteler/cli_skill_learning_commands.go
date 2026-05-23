//nolint:wsl_v5 // Compact command-management branching keeps this CLI helper readable.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	attskill "github.com/tommoulard/atteler/pkg/skill"
)

func skillLearningCommandRequested(opts cliOptions) bool {
	return opts.skillLearningList ||
		opts.skillLearningShow != "" ||
		opts.skillLearningEdit != "" ||
		opts.skillLearningEnable != "" ||
		opts.skillLearningDisable != "" ||
		opts.skillLearningDelete != "" ||
		opts.skillLearningEnableAll ||
		opts.skillLearningDisableAll
}

//nolint:cyclop // Each branch maps one explicit management operation to a small handler.
func runSkillLearningCommand(ctx context.Context, input skillLearningCommandInput) error {
	if ctx == nil {
		return errors.New("skill learning: context is required")
	}

	if len(input.SuggestSteps) > 0 {
		return errors.New("skill learning: --skill-step is a skill suggestion input, not a learning management operation")
	}

	operationCount := skillLearningManagementOperationCount(input)
	if operationCount == 0 {
		return errors.New("skill learning: choose one management operation")
	}
	if operationCount > 1 {
		return errors.New("skill learning: choose only one skill suggestion or learning management operation")
	}

	store := attskill.NewLearningStore(input.Dir)

	switch {
	case input.List:
		return listSkillLearning(store, input.EffectiveEnabled)
	case input.Show != "":
		return showSkillLearning(store, input.Show, input.SkillDir)
	case input.Edit != "":
		return editSkillLearning(ctx, store, input.Edit, input.SkillDir, input.Editor)
	case input.Enable != "":
		return setGeneratedSkillStatus(store, input.Enable, attskill.LearningSkillStatusActive, input.SkillDir)
	case input.Disable != "":
		return setGeneratedSkillStatus(store, input.Disable, attskill.LearningSkillStatusDisabled, input.SkillDir)
	case input.Delete != "":
		return deleteGeneratedSkill(store, input.Delete, input.SkillDir)
	case input.EnableAll:
		if err := store.SetEnabled(true); err != nil {
			return fmt.Errorf("skill learning: enable: %w", err)
		}

		fmt.Println("skill learning: enabled")

		return nil
	case input.DisableAll:
		if err := store.SetEnabled(false); err != nil {
			return fmt.Errorf("skill learning: disable: %w", err)
		}

		fmt.Println("skill learning: disabled")

		return nil
	default:
		return nil
	}
}

func skillLearningManagementOperationCount(input skillLearningCommandInput) int {
	count := 0
	for _, requested := range []bool{
		input.List,
		input.Show != "",
		input.Edit != "",
		input.Enable != "",
		input.Disable != "",
		input.Delete != "",
		input.EnableAll,
		input.DisableAll,
	} {
		if requested {
			count++
		}
	}

	return count
}

func listSkillLearning(store *attskill.LearningStore, configurationEnabled *bool) error {
	state, err := store.Load()
	if err != nil {
		return fmt.Errorf("skill learning: list: %w", err)
	}

	stateEnabled := !state.Disabled
	effectiveEnabled := stateEnabled
	if configurationEnabled != nil {
		effectiveEnabled = stateEnabled && *configurationEnabled
	}

	fmt.Printf("enabled: %t\n", effectiveEnabled)
	if configurationEnabled != nil {
		fmt.Printf("state_enabled: %t\n", stateEnabled)
		fmt.Printf("configuration_enabled: %t\n", *configurationEnabled)
	}
	fmt.Println("state: " + store.StatePath())
	fmt.Printf("observations: %d\n", len(state.Observations))
	fmt.Printf("skills: %d\n", len(state.Skills))

	for i := range state.Skills {
		skill := &state.Skills[i]
		fmt.Printf("- %s\t%s\toccurrences=%d\tpath=%s\n", skill.Slug, skill.Status, skill.Occurrences, skill.SkillPath)
	}

	return nil
}

func showSkillLearning(store *attskill.LearningStore, slug, skillDir string) error {
	skill, err := findGeneratedSkill(store, slug)
	if err != nil {
		return err
	}
	if pathErr := attskill.ValidateGeneratedSkillPath(skill, skillDir); pathErr != nil {
		return fmt.Errorf("skill learning: show %s: %w", slug, pathErr)
	}

	data, err := os.ReadFile(skill.SkillPath)
	if err != nil {
		return fmt.Errorf("skill learning: read generated skill %s: %w", skill.SkillPath, err)
	}

	fmt.Print(string(data))
	if len(data) == 0 || data[len(data)-1] != '\n' {
		fmt.Println()
	}
	fmt.Fprintln(os.Stderr, "skill path: "+skill.SkillPath)

	return nil
}

func editSkillLearning(ctx context.Context, store *attskill.LearningStore, slug, skillDir, editor string) error {
	skill, err := findGeneratedSkill(store, slug)
	if err != nil {
		return err
	}
	if pathErr := attskill.ValidateGeneratedSkillPath(skill, skillDir); pathErr != nil {
		return fmt.Errorf("skill learning: edit %s: %w", slug, pathErr)
	}
	if pathErr := requireGeneratedSkillFile(skill.SkillPath); pathErr != nil {
		return fmt.Errorf("skill learning: edit %s: %w", slug, pathErr)
	}

	editor = firstNonEmpty(editor, os.Getenv("VISUAL"), os.Getenv("EDITOR"))
	name, args, err := editorInvocation(editor, skill.SkillPath)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // User explicitly selected editor; generated skill path is passed as an argument without shell expansion.
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("skill learning: edit %s with %q: %w", slug, name, err)
	}

	fmt.Println("edited: " + skill.Slug)
	fmt.Println("skill path: " + skill.SkillPath)

	return nil
}

func setGeneratedSkillStatus(store *attskill.LearningStore, slug, status, skillDir string) error {
	if status == attskill.LearningSkillStatusActive {
		skill, err := findGeneratedSkill(store, slug)
		if err != nil {
			return err
		}
		if pathErr := attskill.ValidateGeneratedSkillPath(skill, skillDir); pathErr != nil {
			return fmt.Errorf("skill learning: enable %s: %w", slug, pathErr)
		}
		if pathErr := requireGeneratedSkillFile(skill.SkillPath); pathErr != nil {
			return fmt.Errorf("skill learning: enable %s: %w", slug, pathErr)
		}
	}

	skill, err := store.SetSkillStatus(slug, status)
	if err != nil {
		return fmt.Errorf("skill learning: set %s: %w", slug, err)
	}

	fmt.Printf("%s: %s\n", skill.Slug, skill.Status)
	fmt.Println("skill path: " + skill.SkillPath)

	return nil
}

func deleteGeneratedSkill(store *attskill.LearningStore, slug, skillDir string) error {
	skill, err := findGeneratedSkill(store, slug)
	if err != nil {
		return err
	}

	if err := attskill.ValidateGeneratedSkillPath(skill, skillDir); err != nil {
		if deleteErr := store.DeleteSkillInDir(slug, false, skillDir); deleteErr != nil {
			return fmt.Errorf("skill learning: delete unsafe record %s: %w", slug, deleteErr)
		}

		fmt.Println("deleted: " + skill.Slug)
		fmt.Fprintln(os.Stderr, "skill learning: skipped generated skill file removal: "+err.Error())

		return nil
	}

	if err := store.DeleteSkillInDir(slug, true, skillDir); err != nil {
		return fmt.Errorf("skill learning: delete %s: %w", slug, err)
	}

	fmt.Println("deleted: " + skill.Slug)
	if strings.TrimSpace(skill.SkillPath) != "" {
		fmt.Println("removed: " + filepath.Dir(skill.SkillPath))
	}

	return nil
}

func findGeneratedSkill(store *attskill.LearningStore, slug string) (attskill.GeneratedSkill, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return attskill.GeneratedSkill{}, errors.New("skill learning: slug is required")
	}

	state, err := store.Load()
	if err != nil {
		return attskill.GeneratedSkill{}, fmt.Errorf("skill learning: load: %w", err)
	}

	for i := range state.Skills {
		skill := &state.Skills[i]
		if skill.Slug == slug {
			return *skill, nil
		}
	}

	return attskill.GeneratedSkill{}, fmt.Errorf("skill learning: generated skill %q not found", slug)
}

func requireGeneratedSkillFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("generated skill file %q is not readable: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("generated skill path %q is a directory", path)
	}

	return nil
}

func editorInvocation(editor, path string) (name string, args []string, err error) {
	editor = strings.TrimSpace(editor)
	if editor == "" {
		return "", nil, errors.New("skill learning: no editor configured; set VISUAL or EDITOR, or use skill-learning-show <slug> and edit the printed skill path")
	}

	fields, err := splitEditorFields(editor)
	if err != nil {
		return "", nil, fmt.Errorf("skill learning: parse editor: %w", err)
	}
	if len(fields) == 0 {
		return "", nil, errors.New("skill learning: no editor configured; set VISUAL or EDITOR")
	}

	args = append([]string(nil), fields[1:]...)
	args = append(args, path)

	return fields[0], args, nil
}

//nolint:cyclop // Small quote-aware parser keeps editor launching shell-free.
func splitEditorFields(value string) ([]string, error) {
	fields := make([]string, 0, 2)
	var current strings.Builder
	inField := false
	var quote rune

	flush := func() {
		if inField {
			fields = append(fields, current.String())
			current.Reset()
			inField = false
		}
	}

	runes := []rune(value)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote != '\'' && r == '\\' && i+1 < len(runes) && editorBackslashEscapes(runes[i+1], quote) {
			current.WriteRune(runes[i+1])
			inField = true
			i++
			continue
		}

		if quote == 0 && (r == ' ' || r == '\t' || r == '\n' || r == '\r') {
			flush()
			continue
		}

		if r == '\'' || r == '"' {
			if quote == 0 {
				quote = r
				inField = true
				continue
			}
			if quote == r {
				quote = 0
				inField = true
				continue
			}
		}

		current.WriteRune(r)
		inField = true
	}

	if quote != 0 {
		return nil, errors.New("unterminated quote in editor command")
	}

	flush()

	return fields, nil
}

func editorBackslashEscapes(next, quote rune) bool {
	if next == '\\' || next == '\'' || next == '"' ||
		next == ' ' || next == '\t' || next == '\n' || next == '\r' {
		return true
	}

	return quote != 0 && (next == '$' || next == '`')
}
