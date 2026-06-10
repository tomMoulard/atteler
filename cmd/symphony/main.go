// Package main is the standalone Symphony service command.
//
//nolint:wrapcheck,wsl_v5 // This command is intentionally a thin CLI wrapper over the Symphony package.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/symphony"
)

func main() {
	configureSlog()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		stop()
		os.Exit(1)
	}

	stop()
}

func configureSlog() {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("SLOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("symphony", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	validate := fs.Bool("validate", false, "validate WORKFLOW.md and exit")
	workflowPath := fs.String("workflow", "", "path to WORKFLOW.md; defaults to ./WORKFLOW.md")
	var autonomyFlag symphonyAutonomyFlag
	fs.Var(&autonomyFlag, "autonomy", "override workflow autonomy: low, medium, high, or full")

	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: symphony [--validate] [--workflow path] [--autonomy low|medium|high|full] [path-to-WORKFLOW.md]")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() > 1 {
		return errors.New("expected at most one workflow path argument")
	}

	if *workflowPath == "" && fs.NArg() == 1 {
		*workflowPath = fs.Arg(0)
	}

	if *validate {
		cfg, err := symphony.ValidateWorkflowWithOptions(ctx, symphony.Options{
			WorkflowPath: *workflowPath,
			Autonomy:     autonomyFlag.level(),
		})
		if err != nil {
			return err
		}

		fmt.Printf("Symphony workflow valid: %s (tracker=%s workspace_root=%s autonomy=%s)\n", cfg.WorkflowPath, cfg.Tracker.Kind, cfg.Workspace.Root, cfg.Autonomy.String())
		return nil
	}

	return symphony.Run(ctx, symphony.Options{
		WorkflowPath: *workflowPath,
		Logger:       slog.Default(),
		Autonomy:     autonomyFlag.level(),
	})
}

type symphonyAutonomyFlag struct {
	value autonomy.Level
	set   bool
}

func (f *symphonyAutonomyFlag) Set(raw string) error {
	level, err := autonomy.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse autonomy: %w", err)
	}

	f.value = level
	f.set = true

	return nil
}

func (f *symphonyAutonomyFlag) String() string {
	if f == nil || !f.set {
		return ""
	}

	return f.value.String()
}

func (f *symphonyAutonomyFlag) level() autonomy.Level {
	if f == nil || !f.set {
		return ""
	}

	return f.value
}
