package symphony

import (
	"context"
	"errors"
	"time"
)

// Run loads WORKFLOW.md, builds the configured tracker/runner stack, and runs
// the Symphony orchestrator until ctx is canceled.
func Run(ctx context.Context, opts Options) error {
	if ctx == nil {
		return errors.New("symphony: context is required")
	}

	logger := loggerOrDefault(opts.Logger)

	manager, err := workflowManagerFromOptions(opts)
	if err != nil {
		return err
	}

	snapshot, err := manager.Load(ctx)
	if err != nil {
		return err
	}

	tracker, err := NewTrackerClient(snapshot.Config)
	if err != nil {
		return err
	}

	runner := NewDefaultAgentRunner(tracker, logger)

	orchestrator, err := NewOrchestrator(manager, tracker, runner, logger)
	if err != nil {
		return err
	}

	debugServer, err := StartDebugServer(ctx, snapshot.Config.Debug, orchestrator, logger)
	if err != nil {
		return err
	}
	defer func(parent context.Context) {
		if shutdownErr := debugServer.stop(parent, 5*time.Second); shutdownErr != nil {
			logger.Warn("symphony debug server shutdown failed", "error", shutdownErr)
		}
	}(ctx)

	logger.Info(
		"symphony starting",
		"workflow_path", snapshot.Config.WorkflowPath,
		"tracker_kind", snapshot.Config.Tracker.Kind,
		"workspace_root", snapshot.Config.Workspace.Root,
		"autonomy", snapshot.Config.Autonomy.String(),
		"debug_enabled", snapshot.Config.Debug.Enabled,
		"debug_address", snapshot.Config.Debug.Address,
	)

	return orchestrator.Run(ctx)
}

// ValidateWorkflow loads and validates the selected workflow without starting
// the scheduler.
func ValidateWorkflow(ctx context.Context, workDir, workflowPath string) (Config, error) {
	return ValidateWorkflowWithOptions(ctx, Options{WorkDir: workDir, WorkflowPath: workflowPath})
}

// ValidateWorkflowWithOptions loads and validates the selected workflow using
// the same option overrides as Run, without starting the scheduler.
func ValidateWorkflowWithOptions(ctx context.Context, opts Options) (Config, error) {
	if ctx == nil {
		return Config{}, errors.New("symphony validate: context is required")
	}

	manager, err := workflowManagerFromOptions(opts)
	if err != nil {
		return Config{}, err
	}

	snapshot, err := manager.Load(ctx)
	if err != nil {
		return Config{}, err
	}

	return snapshot.Config, nil
}

func workflowManagerFromOptions(opts Options) (*WorkflowManager, error) {
	manager, err := NewWorkflowManager(opts.WorkDir, opts.WorkflowPath)
	if err != nil {
		return nil, err
	}

	if opts.Autonomy != "" {
		if err := manager.SetAutonomyOverride(opts.Autonomy); err != nil {
			return nil, err
		}
	}

	return manager, nil
}
