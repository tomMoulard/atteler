//nolint:wsl_v5 // Sequential state assertions are clearer grouped by operation.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
	attshell "github.com/tommoulard/atteler/pkg/shell"
	attskill "github.com/tommoulard/atteler/pkg/skill"
)

type blockingLearningObserver struct {
	done    chan struct{}
	release chan struct{}
	ctxErr  error
	event   events.Event
}

func (o *blockingLearningObserver) ObserveEvent(ctx context.Context, event events.Event) error {
	<-o.release
	o.ctxErr = ctx.Err()
	o.event = event
	close(o.done)

	return nil
}

type panickingThenRecordingLearningObserver struct {
	events []events.Event
}

func (o *panickingThenRecordingLearningObserver) ObserveEvent(_ context.Context, event events.Event) error {
	if len(o.events) == 0 {
		o.events = append(o.events, event)
		panic("learning observer failed")
	}

	o.events = append(o.events, event)

	return nil
}

type recordingLearningObserver struct {
	events []events.Event
}

func (o *recordingLearningObserver) ObserveEvent(_ context.Context, event events.Event) error {
	o.events = append(o.events, event)

	return nil
}

func TestSkillLearningOptionsFromConfigAndEnv(t *testing.T) {
	t.Parallel()

	enabled := false
	cfg := appconfig.Config{SkillLearning: appconfig.SkillLearningConfig{
		Enabled:         &enabled,
		StoreDir:        "config-store",
		SkillDir:        "config-skills",
		MaxObservations: 25,
		MaxSteps:        4,
		MinOccurrences:  3,
	}}

	opts, ok := skillLearningOptionsFromConfig(cfg, cliOptions{
		skillLearningDir:      "cli-store",
		skillLearningSkillDir: "cli-skills",
	}, func(name string) string {
		switch name {
		case attskill.EnvSkillLearning:
			return affirmativeTrue
		case attskill.EnvSkillLearningDir:
			return "env-store"
		case attskill.EnvSkillLearningSkillDir:
			return "env-skills"
		default:
			return ""
		}
	})

	require.True(t, ok)
	require.NotNil(t, opts.Enabled)
	assert.True(t, *opts.Enabled)
	assert.Equal(t, "cli-store", opts.StoreDir)
	assert.Equal(t, "cli-skills", opts.SkillDir)
	assert.Equal(t, 25, opts.MaxObservations)
	assert.Equal(t, 4, opts.MaxSteps)
	assert.Equal(t, 3, opts.MinOccurrences)

	opts, ok = skillLearningOptionsFromConfig(cfg, cliOptions{}, func(name string) string {
		switch name {
		case attskill.EnvSkillLearning:
			return affirmativeTrue
		case attskill.EnvSkillLearningDir:
			return "env-store"
		case attskill.EnvSkillLearningSkillDir:
			return "env-skills"
		default:
			return ""
		}
	})
	require.True(t, ok)
	require.NotNil(t, opts.Enabled)
	assert.True(t, *opts.Enabled)
	assert.Equal(t, "env-store", opts.StoreDir)
	assert.Equal(t, "env-skills", opts.SkillDir)

	opts, ok = skillLearningOptionsFromConfig(cfg, cliOptions{
		skillLearningDir:      "cli-store",
		skillLearningSkillDir: "cli-skills",
	}, func(string) string { return "" })
	require.False(t, ok)
	assert.Equal(t, "cli-store", opts.StoreDir)
	assert.Equal(t, "cli-skills", opts.SkillDir)
}

func TestSkillLearningOptionsFromConfigEnvFalseOverridesConfigTrue(t *testing.T) {
	t.Parallel()

	enabled := true
	cfg := appconfig.Config{SkillLearning: appconfig.SkillLearningConfig{Enabled: &enabled}}

	opts, ok := skillLearningOptionsFromConfig(cfg, cliOptions{}, func(name string) string {
		if name == attskill.EnvSkillLearning {
			return negativeFalse
		}

		return ""
	})

	require.False(t, ok)
	require.NotNil(t, opts.Enabled)
	assert.False(t, *opts.Enabled)
}

func TestSkillLearningEffectiveEnabledRespectsPersistedDisableAll(t *testing.T) {
	t.Parallel()

	opts := attskill.DefaultLearningOptions()
	opts.StoreDir = filepath.Join(t.TempDir(), "learning")
	require.True(t, skillLearningEffectiveEnabled(opts, true))
	require.False(t, skillLearningEffectiveEnabled(opts, false))

	store := attskill.NewLearningStore(opts.StoreDir)
	require.NoError(t, store.SetEnabled(false))
	require.False(t, skillLearningEffectiveEnabled(opts, true))

	require.Empty(t, skillLearningObserversFromOptions(t.Context(), opts, skillLearningEffectiveEnabled(opts, true)))

	require.NoError(t, store.SetEnabled(true))
	require.True(t, skillLearningEffectiveEnabled(opts, true))
	require.NotEmpty(t, skillLearningObserversFromOptions(t.Context(), opts, skillLearningEffectiveEnabled(opts, true)))

	unreadableOpts := attskill.DefaultLearningOptions()
	unreadableStore := attskill.NewLearningStore(filepath.Join(t.TempDir(), "learning"))
	unreadableOpts.StoreDir = unreadableStore.StoreDir()
	require.NoError(t, os.MkdirAll(unreadableOpts.StoreDir, 0o750))
	require.NoError(t, os.WriteFile(unreadableStore.StatePath(), []byte("{"), 0o600))
	require.False(t, skillLearningEffectiveEnabled(unreadableOpts, true))
	require.Empty(t, skillLearningObserversFromOptions(t.Context(), unreadableOpts, skillLearningEffectiveEnabled(unreadableOpts, true)))
}

func TestSkillLearningEnabledForAutonomyDisablesLow(t *testing.T) {
	t.Parallel()

	assert.False(t, skillLearningEnabledForAutonomy(true, autonomy.Low))
	assert.False(t, skillLearningEnabledForAutonomy(false, autonomy.High))
	assert.True(t, skillLearningEnabledForAutonomy(true, autonomy.Medium))
	assert.True(t, skillLearningEnabledForAutonomy(true, autonomy.Full))
}

func TestRunSkillLearningCommandLowAutonomyBlocksStateWrites(t *testing.T) {
	t.Parallel()

	err := runSkillLearningCommandWithAutonomy(t.Context(), skillLearningCommandInput{
		Dir:        t.TempDir(),
		DisableAll: true,
	}, autonomy.Low)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "autonomy low blocks file writes")
	assert.Contains(t, err.Error(), "--skill-learning-disable-all")
}

func TestRunSkillLearningCommandManagesState(t *testing.T) {
	t.Parallel()

	err := runSkillLearningCommand(t.Context(), skillLearningCommandInput{List: true, SuggestSteps: []string{"run tests"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--skill-step")

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	skillRoot := filepath.Join(skillDir, "plan-code")
	skillPath := filepath.Join(skillRoot, "SKILL.md")
	require.NoError(t, os.MkdirAll(skillRoot, 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Plan Code Skill\n"), 0o600))

	store := attskill.NewLearningStore(storeDir)
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Name:        "Plan Code Skill",
		Slug:        "plan-code",
		Status:      attskill.LearningSkillStatusActive,
		SkillPath:   skillPath,
		Occurrences: 2,
	}}}))

	require.NoError(t, runSkillLearningCommand(t.Context(), skillLearningCommandInput{Dir: storeDir, Disable: "plan-code"}))
	state, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, attskill.LearningSkillStatusDisabled, state.Skills[0].Status)

	require.NoError(t, runSkillLearningCommand(t.Context(), skillLearningCommandInput{Dir: storeDir, Enable: "plan-code"}))
	state, err = store.Load()
	require.NoError(t, err)
	require.Equal(t, attskill.LearningSkillStatusActive, state.Skills[0].Status)

	require.NoError(t, runSkillLearningCommand(t.Context(), skillLearningCommandInput{Dir: storeDir, DisableAll: true}))
	state, err = store.Load()
	require.NoError(t, err)
	require.True(t, state.Disabled)

	require.NoError(t, runSkillLearningCommand(t.Context(), skillLearningCommandInput{Dir: storeDir, EnableAll: true}))
	state, err = store.Load()
	require.NoError(t, err)
	require.False(t, state.Disabled)

	require.NoError(t, runSkillLearningCommand(t.Context(), skillLearningCommandInput{Dir: storeDir, SkillDir: skillDir, Delete: "plan-code"}))
	state, err = store.Load()
	require.NoError(t, err)
	require.Empty(t, state.Skills)
	require.NoFileExists(t, skillPath)
}

func TestRunSkillLearningCommandPermissionPolicyDeniesDisableAllWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := attskill.NewLearningStore(filepath.Join(root, "learning"))

	policy := permission.ReadOnlyPolicy()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, filepath.Join(root, "audit"))

	err := runSkillLearningCommand(ctx, skillLearningCommandInput{Dir: store.StoreDir(), DisableAll: true})
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.write.deny")
	require.NoFileExists(t, store.StatePath())
}

func TestRunSkillLearningCommandPermissionPolicyDeniesListRead(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := attskill.NewLearningStore(filepath.Join(root, "learning"))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, filepath.Join(root, "audit"))

	err := runSkillLearningCommand(ctx, skillLearningCommandInput{Dir: store.StoreDir(), List: true})

	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")
	assert.Contains(t, err.Error(), "skill learning: list")
	require.NoDirExists(t, store.StoreDir())
}

func TestRunSkillLearningCommandPermissionPolicyDeniesShowRead(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	skillRoot := filepath.Join(skillDir, "plan-code")
	skillPath := filepath.Join(skillRoot, "SKILL.md")
	require.NoError(t, os.MkdirAll(skillRoot, 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Plan Code Skill\n"), 0o600))

	store := attskill.NewLearningStore(storeDir)
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Name:        "Plan Code Skill",
		Slug:        "plan-code",
		Status:      attskill.LearningSkillStatusActive,
		SkillPath:   skillPath,
		Occurrences: 2,
	}}}))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, filepath.Join(root, "audit"))

	err := runSkillLearningCommand(ctx, skillLearningCommandInput{Dir: storeDir, SkillDir: skillDir, Show: "plan-code"})
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")
}

func TestRunSkillLearningCommandPermissionPolicyDeniesEditReadBeforeStat(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	skillPath := filepath.Join(skillDir, "plan-code", "SKILL.md")

	store := attskill.NewLearningStore(storeDir)
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Name:        "Plan Code Skill",
		Slug:        "plan-code",
		Status:      attskill.LearningSkillStatusActive,
		SkillPath:   skillPath,
		Occurrences: 2,
	}}}))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, filepath.Join(root, "audit"))

	err := runSkillLearningCommand(ctx, skillLearningCommandInput{
		Dir:      storeDir,
		SkillDir: skillDir,
		Edit:     "plan-code",
		Editor:   os.Args[0] + " -test.run=TestSkillLearningEditorHelper --",
	})
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")
	assert.NotContains(t, err.Error(), "generated skill file")
	require.NoFileExists(t, skillPath)
}

func TestRunSkillLearningCommandPermissionPolicyDeniesEnableReadBeforeStat(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	skillPath := filepath.Join(skillDir, "plan-code", "SKILL.md")

	store := attskill.NewLearningStore(storeDir)
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Name:        "Plan Code Skill",
		Slug:        "plan-code",
		Status:      attskill.LearningSkillStatusDisabled,
		SkillPath:   skillPath,
		Occurrences: 2,
	}}}))

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationRead, permission.ModeDeny)

	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, filepath.Join(root, "audit"))

	err := runSkillLearningCommand(ctx, skillLearningCommandInput{Dir: storeDir, SkillDir: skillDir, Enable: "plan-code"})
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.read.deny")
	assert.NotContains(t, err.Error(), "generated skill file")
	require.NoFileExists(t, skillPath)
}

func TestRunSkillLearningCommandPermissionPolicyDeniesDelete(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	skillRoot := filepath.Join(skillDir, "plan-code")
	skillPath := filepath.Join(skillRoot, "SKILL.md")
	require.NoError(t, os.MkdirAll(skillRoot, 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Plan Code Skill\n"), 0o600))

	store := attskill.NewLearningStore(storeDir)
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Name:        "Plan Code Skill",
		Slug:        "plan-code",
		Status:      attskill.LearningSkillStatusActive,
		SkillPath:   skillPath,
		Occurrences: 2,
	}}}))

	policy := permission.ReadOnlyPolicy()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, filepath.Join(root, "audit"))

	err := runSkillLearningCommand(ctx, skillLearningCommandInput{Dir: storeDir, SkillDir: skillDir, Delete: "plan-code"})
	require.Error(t, err)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.merge_delete.deny")
	require.FileExists(t, skillPath)

	state, loadErr := store.Load()
	require.NoError(t, loadErr)
	require.Len(t, state.Skills, 1)
	assert.Equal(t, "plan-code", state.Skills[0].Slug)
}

func TestSkillLearningObserverPermissionPolicyDeniesAutomaticWrites(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	opts := attskill.DefaultLearningOptions()
	opts.StoreDir = filepath.Join(root, "learning")
	opts.SkillDir = filepath.Join(root, "skills")

	policy := permission.ReadOnlyPolicy()
	auditDir := filepath.Join(root, "audit")
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	observers := skillLearningObserversFromOptions(ctx, opts, true)
	require.Len(t, observers, 1)

	require.NoError(t, observers[0].ObserveEvent(t.Context(), events.Event{
		Type:     events.CommandExecute,
		Metadata: map[string]string{"command": "kubectl get pods"},
	}))

	flushCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	flushable, ok := observers[0].(flushingObserver)
	require.True(t, ok)
	require.NoError(t, flushable.Flush(flushCtx))
	require.NoFileExists(t, attskill.NewLearningStore(opts.StoreDir).StatePath())

	audit, err := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(audit), "permission.write.deny")
}

func TestRunSkillLearningCommandEditLaunchesEditorWithoutAcceptingBaseline(t *testing.T) {
	t.Setenv("ATTELER_SKILL_LEARNING_EDITOR_HELPER", "1")

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	skillRoot := filepath.Join(skillDir, "plan-code")
	skillPath := filepath.Join(skillRoot, "SKILL.md")
	require.NoError(t, os.MkdirAll(skillRoot, 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Plan Code Skill\n"), 0o600))
	auditDir := filepath.Join(root, "audit")
	t.Setenv(attshell.EnvAuditDir, auditDir)

	store := attskill.NewLearningStore(storeDir)
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Name:        "Plan Code Skill",
		Slug:        "plan-code",
		Status:      attskill.LearningSkillStatusActive,
		SkillPath:   skillPath,
		SkillHash:   "tracked-hash",
		Occurrences: 2,
	}}}))

	require.NoError(t, runSkillLearningCommand(t.Context(), skillLearningCommandInput{
		Dir:      storeDir,
		SkillDir: skillDir,
		Edit:     "plan-code",
		Editor:   os.Args[0] + " -test.run=TestSkillLearningEditorHelper --",
	}))

	data, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "# edited by helper")

	state, err := store.Load()
	require.NoError(t, err)
	require.Len(t, state.Skills, 1)
	require.Equal(t, "tracked-hash", state.Skills[0].SkillHash)

	records := readCommandAuditRecords(t, auditDir)
	require.Len(t, records, 2)

	for _, record := range records {
		require.Equal(t, "atteler.skill_learning.editor", record.Caller)
		require.Equal(t, "medium", record.Autonomy)
	}
}

func TestRunSkillLearningCommandEditRequiresEditor(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	skillRoot := filepath.Join(skillDir, "plan-code")
	skillPath := filepath.Join(skillRoot, "SKILL.md")
	require.NoError(t, os.MkdirAll(skillRoot, 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Plan Code Skill\n"), 0o600))

	store := attskill.NewLearningStore(storeDir)
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Name:        "Plan Code Skill",
		Slug:        "plan-code",
		Status:      attskill.LearningSkillStatusActive,
		SkillPath:   skillPath,
		Occurrences: 2,
	}}}))

	err := runSkillLearningCommand(t.Context(), skillLearningCommandInput{
		Dir:      storeDir,
		SkillDir: skillDir,
		Edit:     "plan-code",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no editor configured")
}

func TestEditorInvocationParsesQuotedEditorCommand(t *testing.T) {
	t.Parallel()

	name, args, err := editorInvocation(`"/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code" --wait --reuse-window`, "/tmp/SKILL.md")
	require.NoError(t, err)
	require.Equal(t, "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code", name)
	require.Equal(t, []string{"--wait", "--reuse-window", "/tmp/SKILL.md"}, args)

	name, args, err = editorInvocation(`vim\ diff '+set ft=markdown'`, "/tmp/SKILL.md")
	require.NoError(t, err)
	require.Equal(t, "vim diff", name)
	require.Equal(t, []string{"+set ft=markdown", "/tmp/SKILL.md"}, args)

	name, args, err = editorInvocation(`"C:\Program Files\Notepad++\notepad++.exe" -multiInst`, `C:\tmp\SKILL.md`)
	require.NoError(t, err)
	require.Equal(t, `C:\Program Files\Notepad++\notepad++.exe`, name)
	require.Equal(t, []string{"-multiInst", `C:\tmp\SKILL.md`}, args)

	_, _, err = editorInvocation(`"unterminated editor`, "/tmp/SKILL.md")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unterminated quote")
}

//nolint:paralleltest // Captures process-global stdout.
func TestRunSkillLearningCommandListShowsEffectiveOptOut(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	configEnabled := false

	out := captureStdout(t, func() {
		require.NoError(t, runSkillLearningCommand(t.Context(), skillLearningCommandInput{
			EffectiveEnabled: &configEnabled,
			Dir:              storeDir,
			List:             true,
		}))
	})

	require.Contains(t, out, "enabled: false\n")
	require.Contains(t, out, "state_enabled: true\n")
	require.Contains(t, out, "configuration_enabled: false\n")
}

//nolint:paralleltest // Captures process-global stdout.
func TestRunSkillLearningCommandListOmitsImplicitConfigurationStatus(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")

	out := captureStdout(t, func() {
		require.NoError(t, runSkillLearningCommand(t.Context(), skillLearningCommandInput{
			Dir:  storeDir,
			List: true,
		}))
	})

	require.Contains(t, out, "enabled: true\n")
	require.NotContains(t, out, "state_enabled:")
	require.NotContains(t, out, "configuration_enabled:")
}

func TestRunSkillLearningCommandRefusesToShowOutsideGeneratedSkillDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillPath := filepath.Join(root, "outside", "plan-code", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Plan Code Skill\n"), 0o600))

	store := attskill.NewLearningStore(storeDir)
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Name:        "Plan Code Skill",
		Slug:        "plan-code",
		Status:      attskill.LearningSkillStatusActive,
		SkillPath:   skillPath,
		Occurrences: 2,
	}}}))

	err := runSkillLearningCommand(t.Context(), skillLearningCommandInput{
		Dir:      storeDir,
		SkillDir: filepath.Join(root, "skills"),
		Show:     "plan-code",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside generated skill directory")

	err = runSkillLearningCommand(t.Context(), skillLearningCommandInput{
		Dir:      storeDir,
		SkillDir: filepath.Join(root, "skills"),
		Edit:     "plan-code",
		Editor:   "unused-editor",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside generated skill directory")

	err = runSkillLearningCommand(t.Context(), skillLearningCommandInput{
		Dir:      storeDir,
		SkillDir: filepath.Join(root, "skills"),
		Enable:   "plan-code",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside generated skill directory")

	require.NoError(t, runSkillLearningCommand(t.Context(), skillLearningCommandInput{
		Dir:     storeDir,
		Disable: "plan-code",
	}))

	require.NoError(t, runSkillLearningCommand(t.Context(), skillLearningCommandInput{
		Dir:      storeDir,
		SkillDir: filepath.Join(root, "skills"),
		Delete:   "plan-code",
	}))
	state, loadErr := store.Load()
	require.NoError(t, loadErr)
	require.Empty(t, state.Skills)
	require.FileExists(t, skillPath)
}

func TestRunSkillLearningCommandEnableRefusesMissingGeneratedSkillFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	skillPath := filepath.Join(skillDir, "plan-code", "SKILL.md")

	store := attskill.NewLearningStore(storeDir)
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Name:        "Plan Code Skill",
		Slug:        "plan-code",
		Status:      attskill.LearningSkillStatusDisabled,
		SkillPath:   skillPath,
		Occurrences: 2,
	}}}))

	err := runSkillLearningCommand(t.Context(), skillLearningCommandInput{
		Dir:      storeDir,
		SkillDir: skillDir,
		Enable:   "plan-code",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not readable")

	state, loadErr := store.Load()
	require.NoError(t, loadErr)
	require.Equal(t, attskill.LearningSkillStatusDisabled, state.Skills[0].Status)
}

func TestGeneratedSkillReferenceContextFormatsMatchingSkill(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillPath := filepath.Join(root, "skills", "k8s-investigation", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# K8s Investigation\nUse kubectl safely.\n"), 0o600))

	store := attskill.NewLearningStore(storeDir)
	now := time.Now().UTC()
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   now,
		UpdatedAt:   now,
		Name:        "K8s Investigation Skill",
		Slug:        "k8s-investigation",
		Status:      attskill.LearningSkillStatusActive,
		SkillPath:   skillPath,
		Steps:       []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
		Occurrences: 3,
	}}}))

	got := generatedSkillReferenceContext("Investigate this Kubernetes incident.", storeDir, filepath.Join(root, "skills"), true)
	require.Contains(t, got, "Generated skills matched this request")
	require.Contains(t, got, "generated-skill:k8s-investigation")
	require.Contains(t, got, "K8s Investigation")

	refCtx := generatedSkillReferenceContextWithManifest(
		"Investigate this Kubernetes incident.",
		storeDir,
		filepath.Join(root, "skills"),
		true,
		contextOptionsForProviderModel(contextref.Options{}, "anthropic", "claude-test"),
	)
	require.NotEmpty(t, refCtx.Content)
	assert.Contains(t, refCtx.Estimator, "anthropic-calibrated")
	assert.Equal(t, 1, refCtx.Manifest.IncludedCount)
	assert.Equal(t, 0, refCtx.Manifest.RejectedCount)
	assert.Contains(t, refCtx.Manifest.TokenEstimator, "anthropic-calibrated")
	require.Len(t, refCtx.Manifest.Entries, 1)
	assert.Equal(t, "generated-skill:k8s-investigation", refCtx.Manifest.Entries[0].Scope)
	assert.Equal(t, contextref.ReferenceDecisionLoaded, refCtx.Manifest.Entries[0].PolicyDecision)
	assert.Equal(t, "loaded.generated_skill", refCtx.Manifest.Entries[0].PolicyReasonCode)
	assert.Contains(t, refCtx.Manifest.Entries[0].TokenEstimator, "model=claude-test")
	assert.Positive(t, refCtx.Manifest.Entries[0].TokenEstimate.UpperBoundTokens)

	require.Empty(t, generatedSkillReferenceContext("Investigate this Kubernetes incident.", storeDir, filepath.Join(root, "skills"), false))
	require.Empty(t, generatedSkillReferenceContext("Summarize the README.", storeDir, filepath.Join(root, "skills"), true))
}

func TestGeneratedSkillReferenceContextAuditsTruncationReason(t *testing.T) {
	t.Parallel()

	refCtx := formatGeneratedSkillReferencesWithManifest(
		[]attskill.GeneratedSkillReference{{
			Slug:      "k8s-investigation",
			Name:      "K8s Investigation",
			Path:      filepath.Join(t.TempDir(), "skills", "k8s-investigation", "SKILL.md"),
			Content:   strings.Repeat("use kubectl safely\n", 4),
			Truncated: true,
		}},
		contextOptionsForProviderModel(contextref.Options{}, "anthropic", "claude-test"),
	)

	require.Len(t, refCtx.Manifest.Entries, 1)
	entry := refCtx.Manifest.Entries[0]
	assert.Equal(t, contextref.ReferenceDecisionTruncated, entry.PolicyDecision)
	assert.Equal(t, "truncated.byte_limit", entry.PolicyReasonCode)
	assert.Contains(t, entry.PolicyReason, "generated skill")
	assert.Contains(t, entry.PolicyReason, "byte limit")
	assert.True(t, entry.Truncated)
	assert.Equal(t, 1, refCtx.Manifest.TruncatedCount)
	assert.Equal(t, 1, refCtx.Manifest.IncludedCount)
}

func TestRunWithState_HeadlessGeneratedSkillAppearsInContextManifest(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	require.NoError(t, os.WriteFile(filepath.Join(root, "style.md"), []byte("Prefer concise reports.\n"), 0o600))

	skillPath := filepath.Join(skillDir, "k8s-investigation", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# K8s Investigation\nUse kubectl safely.\n"), 0o600))

	store := attskill.NewLearningStore(storeDir)
	now := time.Now().UTC()
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   now,
		UpdatedAt:   now,
		Name:        "K8s Investigation Skill",
		Slug:        "k8s-investigation",
		Status:      attskill.LearningSkillStatusActive,
		SkillPath:   skillPath,
		Steps:       []string{"run kubectl get pods"},
		Occurrences: 3,
	}}}))

	replayPath := filepath.Join(root, "response.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "claude-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "Investigate this Kubernetes incident."}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "claude-test"},
	))

	sessionStore := session.NewStore(filepath.Join(root, "sessions"))
	headlessID := "generated-skill-manifest"
	contextOptions := contextOptionsForProviderModel(contextref.Options{Root: root}, "anthropic", "claude-test")
	err := runWithState(t.Context(), cliOptions{
		oncePrompt:         "Investigate this Kubernetes incident.",
		headless:           true,
		headlessID:         headlessID,
		outputFormat:       outputFormatText,
		replayResponsePath: replayPath,
	}, appState{
		registry:                  llm.NewRegistry(),
		agentRegistry:             agent.NewRegistry(nil),
		sessionStore:              sessionStore,
		sessionState:              session.New("claude-test", nil),
		contextOptions:            contextOptions,
		configuredReferences:      []string{"style.md"},
		selectedModel:             "claude-test",
		skillLearningStoreDir:     storeDir,
		skillLearningSkillDir:     skillDir,
		skillLearningEnabled:      true,
		referenceContextEstimator: "stale-estimator-forces-configured-reference-reload",
	})
	require.NoError(t, err)

	log, err := sessionStore.ReadHeadlessLog(headlessID)
	require.NoError(t, err)

	manifest := decodeHeadlessContextManifest(t, log)
	require.Len(t, manifest.ConfiguredReferences.Entries, 2)
	assert.Equal(t, 2, manifest.IncludedReferenceCount)
	assertManifestContainsGeneratedSkillReference(t, manifest, "k8s-investigation")
	assertManifestContainsSource(t, manifest, "style.md")
	assert.Positive(t, manifest.ReferenceEstimatedUpperBound)
}

func assertManifestContainsGeneratedSkillReference(t *testing.T, manifest requestContextManifest, slug string) {
	t.Helper()

	for i := range manifest.ConfiguredReferences.Entries {
		entry := &manifest.ConfiguredReferences.Entries[i]
		if entry.Scope != "generated-skill:"+slug {
			continue
		}

		assert.Equal(t, contextref.ReferenceDecisionLoaded, entry.PolicyDecision)
		assert.Equal(t, "loaded.generated_skill", entry.PolicyReasonCode)
		assert.Contains(t, entry.TokenEstimator, "model=claude-test")
		assert.Positive(t, entry.TokenEstimate.UpperBoundTokens)

		return
	}

	require.FailNow(t, "generated skill reference missing from manifest", "slug=%s manifest=%+v", slug, manifest.ConfiguredReferences.Entries)
}

func assertManifestContainsSource(t *testing.T, manifest requestContextManifest, source string) {
	t.Helper()

	for i := range manifest.ConfiguredReferences.Entries {
		entry := &manifest.ConfiguredReferences.Entries[i]
		if entry.Source == source {
			return
		}
	}

	require.FailNow(t, "source missing from manifest", "source=%s manifest=%+v", source, manifest.ConfiguredReferences.Entries)
}

func TestBackgroundObserverDoesNotBlockEventEmissionAndFlushes(t *testing.T) {
	t.Parallel()

	inner := &blockingLearningObserver{
		done:    make(chan struct{}),
		release: make(chan struct{}),
	}
	observer := newBackgroundObserver(t.Context(), inner, 1)

	errCh := make(chan error, 1)
	go func() {
		errCh <- observer.ObserveEvent(t.Context(), events.Event{
			Type:     events.CommandExecute,
			Metadata: map[string]string{"command": "kubectl get pods"},
		})
	}()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(100 * time.Millisecond):
		require.Fail(t, "background observer blocked event emission")
	}

	close(inner.release)

	flushCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	require.NoError(t, observer.Flush(flushCtx))

	select {
	case <-inner.done:
	case <-time.After(time.Second):
		require.Fail(t, "background observer did not flush queued work")
	}
}

func TestBackgroundObserverDecouplesQueuedWorkFromCallerCancellation(t *testing.T) {
	t.Parallel()

	inner := &blockingLearningObserver{
		done:    make(chan struct{}),
		release: make(chan struct{}),
	}
	observer := newBackgroundObserver(t.Context(), inner, 1)
	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, observer.ObserveEvent(ctx, events.Event{
		Type:     events.CommandExecute,
		Metadata: map[string]string{"command": "kubectl get pods"},
	}))
	cancel()
	close(inner.release)

	flushCtx, flushCancel := context.WithTimeout(t.Context(), time.Second)
	defer flushCancel()

	require.NoError(t, observer.Flush(flushCtx))

	select {
	case <-inner.done:
	case <-time.After(time.Second):
		require.Fail(t, "background observer did not process queued work")
	}
	require.NoError(t, inner.ctxErr)
}

func TestBackgroundObserverClonesQueuedEvent(t *testing.T) {
	t.Parallel()

	inner := &blockingLearningObserver{
		done:    make(chan struct{}),
		release: make(chan struct{}),
	}
	observer := newBackgroundObserver(t.Context(), inner, 1)
	metadata := map[string]string{"command": "kubectl get pods"}
	require.NoError(t, observer.ObserveEvent(t.Context(), events.Event{
		Type:     events.CommandExecute,
		Metadata: metadata,
	}))

	metadata["command"] = "kubectl get secret production-token"
	close(inner.release)

	flushCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	require.NoError(t, observer.Flush(flushCtx))
	require.Equal(t, "kubectl get pods", inner.event.Metadata["command"])
}

func TestBackgroundObserverMinimizesQueuedLearningMetadata(t *testing.T) {
	t.Parallel()

	inner := &recordingLearningObserver{}
	observer := newBackgroundObserver(t.Context(), inner, 5)

	require.NoError(t, observer.ObserveEvent(t.Context(), events.Event{
		Type:    events.CommandExecute,
		Content: "kubectl get pods",
		Metadata: map[string]string{
			"command":  "kubectl get pods",
			"source":   "llm_tool",
			"provider": "codex",
			"cwd":      "/private/workspace",
			"input":    "kubectl get secret production-token",
			"stdout":   "raw pod logs",
		},
	}))
	require.NoError(t, observer.ObserveEvent(t.Context(), events.Event{
		Type:    events.CommandExecute,
		Content: "kubectl get secret production-token",
		Metadata: map[string]string{
			"source": "user",
			"input":  "kubectl get secret production-token",
		},
	}))
	require.NoError(t, observer.ObserveEvent(t.Context(), events.Event{
		Type: events.ToolExecute,
		Metadata: map[string]string{
			"tool":      "browser.open",
			"arguments": `{"token":"raw-secret"}`,
			"output":    "raw tool output",
		},
	}))
	require.NoError(t, observer.ObserveEvent(t.Context(), events.Event{
		Type:    events.UserMessage,
		Content: "investigate kubernetes pods with token abc123",
		Metadata: map[string]string{
			"raw": "private prompt metadata",
		},
	}))

	flushCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	require.NoError(t, observer.Flush(flushCtx))
	require.Len(t, inner.events, 4)
	assert.Equal(t, map[string]string{
		"command":  "kubectl get pods",
		"source":   "llm_tool",
		"provider": "codex",
	}, inner.events[0].Metadata)
	assert.Empty(t, inner.events[0].Content)
	assert.NotContains(t, inner.events[0].Metadata, "input")
	assert.NotContains(t, inner.events[0].Metadata, "stdout")
	assert.NotContains(t, inner.events[0].Metadata, "cwd")
	assert.Empty(t, inner.events[1].Content)
	assert.Equal(t, map[string]string{"command": "kubectl get secret {{secret}}", "source": "user"}, inner.events[1].Metadata)
	assert.NotContains(t, inner.events[1].Metadata, "input")
	assert.NotContains(t, inner.events[1].Metadata["command"], "production-token")
	assert.Equal(t, map[string]string{"tool": "browser.open"}, inner.events[2].Metadata)
	assert.Empty(t, inner.events[2].Content)
	assert.Nil(t, inner.events[3].Metadata)
	assert.Equal(t, "investigate kubernetes workflow", inner.events[3].Content)
	assert.NotContains(t, inner.events[3].Content, "abc123")
}

func TestBackgroundObserverSkipsNonLearningEventsBeforeQueueing(t *testing.T) {
	t.Parallel()

	inner := &recordingLearningObserver{}
	observer := newBackgroundObserver(t.Context(), inner, 2)
	require.NoError(t, observer.ObserveEvent(t.Context(), events.Event{
		Type:    events.CommandOutput,
		Content: "token=raw-command-output-should-not-enter-learning-queue",
		Metadata: map[string]string{
			"stdout": "raw pod logs",
		},
	}))

	flushCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	require.NoError(t, observer.Flush(flushCtx))
	require.Empty(t, inner.events)
}

func TestBackgroundObserverRecoversInnerPanic(t *testing.T) {
	t.Parallel()

	inner := &panickingThenRecordingLearningObserver{}
	observer := newBackgroundObserver(t.Context(), inner, 2)
	require.NoError(t, observer.ObserveEvent(t.Context(), events.Event{
		Type:     events.CommandExecute,
		Metadata: map[string]string{"command": "first"},
	}))
	require.NoError(t, observer.ObserveEvent(t.Context(), events.Event{
		Type:     events.CommandExecute,
		Metadata: map[string]string{"command": "second"},
	}))

	flushCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	require.NoError(t, observer.Flush(flushCtx))
	require.Len(t, inner.events, 2)
	require.Equal(t, "second", inner.events[1].Metadata["command"])
}

func TestRunWithStateInjectsMatchingGeneratedSkillContext(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	learningDir := filepath.Join(root, "learning")
	skillPath := filepath.Join(root, "skills", "k8s-investigation", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# K8s Investigation\nUse kubectl safely.\n"), 0o600))

	store := attskill.NewLearningStore(learningDir)
	now := time.Now().UTC()
	require.NoError(t, store.Save(attskill.LearningState{Skills: []attskill.GeneratedSkill{{
		CreatedAt:   now,
		UpdatedAt:   now,
		Name:        "K8s Investigation Skill",
		Slug:        "k8s-investigation",
		Status:      attskill.LearningSkillStatusActive,
		SkillPath:   skillPath,
		Steps:       []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
		Occurrences: 3,
	}}}))

	replayPath := filepath.Join(root, "response.json")
	recordPath := filepath.Join(root, "request.json")
	require.NoError(t, saveRecordedResponse(t.Context(),
		replayPath,
		llm.CompleteParams{Model: "gpt-test", Messages: []llm.Message{{Role: llm.RoleUser, Content: "replay"}}},
		nil,
		&llm.Response{Content: "recorded answer", Model: "gpt-test"},
	))

	err := runWithState(t.Context(), cliOptions{
		oncePrompt:         "Investigate this Kubernetes incident.",
		headless:           true,
		replayResponsePath: replayPath,
		recordResponsePath: recordPath,
	}, appState{
		registry:              llm.NewRegistry(),
		agentRegistry:         agent.NewRegistry(nil),
		sessionStore:          session.NewStore(filepath.Join(root, "sessions")),
		sessionState:          session.New("gpt-test", nil),
		contextOptions:        contextref.Options{Root: root},
		selectedModel:         "gpt-test",
		modelLocked:           true,
		skillLearningStoreDir: learningDir,
		skillLearningSkillDir: filepath.Join(root, "skills"),
		skillLearningEnabled:  true,
	})
	require.NoError(t, err)

	data, err := os.ReadFile(recordPath)
	require.NoError(t, err)

	var recorded responseRecordFile
	require.NoError(t, json.Unmarshal(data, &recorded))
	require.NotEmpty(t, recorded.Request.Messages)

	systemContext := strings.Join(systemMessageContents(recorded.Request.Messages), "\n\n")
	require.Contains(t, systemContext, "Generated skills matched this request")
	require.Contains(t, systemContext, "generated-skill:k8s-investigation")
	require.Contains(t, systemContext, "K8s Investigation")
}

func systemMessageContents(messages []llm.Message) []string {
	var out []string
	for i := range messages {
		if messages[i].Role == llm.RoleSystem {
			out = append(out, messages[i].Content)
		}
	}

	return out
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = writer

	t.Cleanup(func() {
		os.Stdout = oldStdout
	})

	fn()

	require.NoError(t, writer.Close())
	os.Stdout = oldStdout

	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	return string(data)
}

//nolint:paralleltest // Helper may os.Exit when invoked as a subprocess by another test.
func TestSkillLearningEditorHelper(_ *testing.T) {
	if os.Getenv("ATTELER_SKILL_LEARNING_EDITOR_HELPER") != "1" {
		return
	}

	path := os.Args[len(os.Args)-1]
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // Test helper edits the path passed by the parent test.
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if _, err := handle.WriteString("\n# edited by helper\n"); err != nil {
		_ = handle.Close()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}

	if err := handle.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}

	os.Exit(0)
}
