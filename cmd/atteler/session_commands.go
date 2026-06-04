package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/autonomy"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/permission"
	"github.com/tommoulard/atteler/pkg/session"
)

const (
	headlessStreamPollInterval    = time.Second
	headlessTerminalDrainInterval = 100 * time.Millisecond
	headlessRetryRedactedValue    = "[REDACTED]"
)

func listSessions(ctx context.Context, store *session.Store, tag string) error {
	if err := authorizeSessionStoreRead(ctx, store, "", "list sessions"); err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	summaries, err := listSessionSummaries(store, tag)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	if len(summaries) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatSessionSummary(summaries[i]))
	}

	return nil
}

func headlessCommandRequested(opts cliOptions) bool {
	return opts.listHeadless ||
		opts.recoverHeadless ||
		opts.cleanupHeadless ||
		opts.statusHeadlessID != "" ||
		opts.cancelHeadlessID != "" ||
		opts.retryHeadlessID != "" ||
		opts.streamHeadlessID != ""
}

func runHeadlessCommand(ctx context.Context, opts cliOptions, store *session.Store) error {
	return runHeadlessCommandWithAutonomy(ctx, opts, store, autonomy.DefaultLevel)
}

func runHeadlessCommandWithAutonomy(ctx context.Context, opts cliOptions, store *session.Store, level autonomy.Level) error {
	if headlessCommandCount(opts) > 1 {
		return errors.New("headless command: choose only one of list, recover, cleanup, status, cancel, retry, or stream")
	}

	if headlessCommandMayWrite(opts) && !autonomy.Normalize(level).Allows(autonomy.ActionFileWrite) {
		return fmt.Errorf("%s", autonomy.DenialMessage(level, autonomy.ActionFileWrite, headlessAutonomyContext(opts)))
	}

	switch {
	case opts.statusHeadlessID != "":
		return statusHeadlessRun(ctx, store, opts.statusHeadlessID)
	case opts.cancelHeadlessID != "":
		return cancelHeadlessRun(ctx, store, opts.cancelHeadlessID)
	case opts.retryHeadlessID != "":
		return retryHeadlessRun(ctx, store, opts.retryHeadlessID, opts.retryHeadlessNewID)
	case opts.streamHeadlessID != "":
		return streamHeadlessLog(ctx, store, opts.streamHeadlessID)
	case opts.cleanupHeadless:
		return cleanupHeadlessRuns(ctx, store, opts.headlessMaxAge)
	case opts.recoverHeadless:
		return recoverHeadlessRuns(ctx, store)
	case opts.listHeadless:
		return listHeadlessRuns(ctx, store, opts.headlessStatusFilter, opts.headlessMaxAge)
	default:
		return nil
	}
}

func headlessCommandMayWrite(opts cliOptions) bool {
	return opts.listHeadless ||
		opts.recoverHeadless ||
		opts.cleanupHeadless ||
		opts.statusHeadlessID != "" ||
		opts.cancelHeadlessID != "" ||
		opts.retryHeadlessID != ""
}

func headlessAutonomyContext(opts cliOptions) string {
	switch {
	case opts.listHeadless:
		return "--list-headless"
	case opts.recoverHeadless:
		return "--recover-headless"
	case opts.statusHeadlessID != "":
		return "--status-headless"
	case opts.cancelHeadlessID != "":
		return "--cancel-headless"
	case opts.retryHeadlessID != "":
		return "--retry-headless"
	case opts.cleanupHeadless:
		return "--cleanup-headless"
	default:
		return "headless command"
	}
}

func headlessCommandCount(opts cliOptions) int {
	count := 0
	if opts.listHeadless {
		count++
	}

	if opts.recoverHeadless {
		count++
	}

	if opts.cleanupHeadless {
		count++
	}

	if opts.statusHeadlessID != "" {
		count++
	}

	if opts.cancelHeadlessID != "" {
		count++
	}

	if opts.retryHeadlessID != "" {
		count++
	}

	if opts.streamHeadlessID != "" {
		count++
	}

	return count
}

func listHeadlessRuns(ctx context.Context, store *session.Store, statusFilter, maxAgeFilter string) error {
	if err := authorizeHeadlessPermission(ctx, "list headless runs", store, "", permission.OperationRead); err != nil {
		return fmt.Errorf("list headless runs: %w", err)
	}

	filter, err := parseHeadlessListFilter(statusFilter, maxAgeFilter)
	if err != nil {
		return err
	}

	runs, err := store.ListHeadlessRuns()
	if err != nil {
		return fmt.Errorf("list headless runs: %w", err)
	}

	filtered := make([]session.HeadlessRun, 0, len(runs))
	for i := range runs {
		run := &runs[i]
		if filter.matches(*run) {
			filtered = append(filtered, *run)
		}
	}

	if len(filtered) == 0 {
		if filter.enabled() {
			fmt.Println("No headless runs matched filters.")
			return nil
		}

		fmt.Println("No headless runs found.")

		return nil
	}

	for i := range filtered {
		fmt.Println(formatHeadlessRun(filtered[i]))
	}

	return nil
}

type headlessListFilter struct {
	statuses map[session.HeadlessStatus]struct{}
	maxAge   time.Duration
}

func parseHeadlessListFilter(statusFilter, maxAgeFilter string) (headlessListFilter, error) {
	filter := headlessListFilter{}

	statusFilter = strings.TrimSpace(statusFilter)
	if statusFilter != "" {
		statuses := strings.Split(statusFilter, ",")
		filter.statuses = make(map[session.HeadlessStatus]struct{}, len(statuses))

		for _, raw := range statuses {
			status, err := session.ParseHeadlessStatus(raw)
			if err != nil {
				return headlessListFilter{}, fmt.Errorf("list headless runs: %w", err)
			}

			filter.statuses[status] = struct{}{}
		}
	}

	maxAgeFilter = strings.TrimSpace(maxAgeFilter)
	if maxAgeFilter != "" {
		maxAge, err := time.ParseDuration(maxAgeFilter)
		if err != nil {
			return headlessListFilter{}, fmt.Errorf("list headless runs: parse --headless-max-age: %w", err)
		}

		if maxAge <= 0 {
			return headlessListFilter{}, errors.New("list headless runs: --headless-max-age must be greater than zero")
		}

		filter.maxAge = maxAge
	}

	return filter, nil
}

func (f headlessListFilter) enabled() bool {
	return len(f.statuses) > 0 || f.maxAge > 0
}

func (f headlessListFilter) matches(run session.HeadlessRun) bool {
	if len(f.statuses) > 0 {
		if _, ok := f.statuses[run.Status]; !ok {
			return false
		}
	}

	if f.maxAge > 0 {
		ageTime := headlessListAgeTime(run)
		if ageTime.IsZero() || time.Since(ageTime) > f.maxAge {
			return false
		}
	}

	return true
}

func headlessListAgeTime(run session.HeadlessRun) time.Time {
	for _, at := range []*time.Time{
		run.ExpiredAt,
		run.RetriedAt,
		run.CanceledAt,
		run.CompletedAt,
	} {
		if at != nil && !at.IsZero() {
			return at.UTC()
		}
	}

	if !run.LastHeartbeatAt.IsZero() {
		return run.LastHeartbeatAt.UTC()
	}

	if !run.UpdatedAt.IsZero() {
		return run.UpdatedAt.UTC()
	}

	return run.StartedAt.UTC()
}

func recoverHeadlessRuns(ctx context.Context, store *session.Store) error {
	if err := authorizeHeadlessPermission(ctx, "recover headless runs", store, "", permission.OperationRead, permission.OperationWrite); err != nil {
		return fmt.Errorf("recover headless runs: %w", err)
	}

	recovered, err := store.RecoverStaleHeadlessRuns(0)
	if err != nil {
		return fmt.Errorf("recover headless runs: %w", err)
	}

	if len(recovered) == 0 {
		fmt.Println("No recoverable stale/orphaned/expired headless runs found.")
		return nil
	}

	for i := range recovered {
		fmt.Println(formatHeadlessRun(recovered[i]))
	}

	return nil
}

func statusHeadlessRun(ctx context.Context, store *session.Store, id string) error {
	if id == "" {
		return errors.New("status headless: id is required")
	}

	if err := authorizeHeadlessPermission(ctx, "status headless run", store, id, permission.OperationRead); err != nil {
		return fmt.Errorf("status headless: %w", err)
	}

	run, err := store.HeadlessRunStatus(id)
	if err != nil {
		return fmt.Errorf("status headless: %w", err)
	}

	fmt.Println(formatHeadlessRun(run))

	return nil
}

func cancelHeadlessRun(ctx context.Context, store *session.Store, id string) error {
	if id == "" {
		return errors.New("cancel headless: id is required")
	}

	if err := authorizeHeadlessPermission(ctx, "cancel headless run", store, id, permission.OperationExecute, permission.OperationWrite, permission.OperationMergeDelete); err != nil {
		return fmt.Errorf("cancel headless: %w", err)
	}

	run, err := store.CancelHeadlessRun(id, "canceled by atteler session cancel-headless")
	if err != nil {
		return fmt.Errorf("cancel headless: %w", err)
	}

	fmt.Println(formatHeadlessRun(run))

	return nil
}

func authorizeHeadlessPermission(
	ctx context.Context,
	action string,
	store *session.Store,
	id string,
	kinds ...permission.OperationKind,
) error {
	if len(kinds) == 0 {
		return nil
	}

	target := "headless runs"
	if store != nil {
		target = filepath.Join(store.Dir(), "headless")
		if strings.TrimSpace(id) != "" {
			target = filepath.Join(target, strings.TrimSpace(id))
		}
	}

	operations := make([]permission.Operation, 0, len(kinds))
	for _, kind := range kinds {
		operations = append(operations, permission.Operation{
			Kind:   kind,
			Action: action,
			Source: "atteler.session.headless",
			Target: target,
		})
	}

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action:     action,
		Source:     "atteler.session.headless",
		Target:     target,
		Operations: operations,
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}

//nolint:govet // Field order follows retry output readability; padding is irrelevant for tiny CLI metadata.
type headlessRetryProcess struct {
	ID         string
	Executable string
	PID        int
	Args       []string
}

var startHeadlessRetryProcess = startHeadlessRetryProcessDefault

func retryHeadlessRun(ctx context.Context, store *session.Store, id, requestedNewID string) error {
	if id == "" {
		return errors.New("retry headless: id is required")
	}

	if err := session.ValidateHeadlessID(id); err != nil {
		return fmt.Errorf("retry headless: %w", err)
	}

	newID := requestedNewID
	if newID == "" {
		newID = newHeadlessRetryID(id)
	}

	if validateErr := session.ValidateHeadlessID(newID); validateErr != nil {
		return fmt.Errorf("retry headless: %w", validateErr)
	}

	if err := authorizeHeadlessPermission(ctx, "retry headless run", store, id, permission.OperationRead, permission.OperationExecute, permission.OperationWrite); err != nil {
		return fmt.Errorf("retry headless: %w", err)
	}

	var started headlessRetryProcess

	reason := "retried by atteler session retry-headless as " + newID

	retried, err := store.MarkHeadlessRunRetriedAfterStart(id, newID, reason, func(parent session.HeadlessRun) error {
		process, startErr := startHeadlessRetryProcess(ctx, store, parent, newID)
		if startErr != nil {
			return startErr
		}

		started = process

		return nil
	})
	if err != nil {
		return fmt.Errorf("retry headless: %w", err)
	}

	fmt.Println(formatHeadlessRun(retried))
	fmt.Println(formatHeadlessRetryProcess(started))

	return nil
}

func newHeadlessRetryID(parentID string) string {
	return parentID + "-retry-" + time.Now().UTC().Format("20060102T150405.000000000Z")
}

func startHeadlessRetryProcessDefault(ctx context.Context, store *session.Store, parent session.HeadlessRun, newID string) (headlessRetryProcess, error) {
	if err := ctx.Err(); err != nil {
		return headlessRetryProcess{}, fmt.Errorf("retry context: %w", err)
	}

	executable, err := headlessRetryExecutable(parent)
	if err != nil {
		return headlessRetryProcess{}, err
	}

	args, err := headlessRetryArgs(store, parent, newID)
	if err != nil {
		return headlessRetryProcess{}, err
	}

	// The retry is intentionally detached from this CLI invocation after
	// Start/Release. Do not use exec.CommandContext here: the command context
	// belongs to the short-lived retry command and must not be able to kill the
	// newly launched headless job after this process exits.
	//nolint:noctx // The retry process must outlive this short-lived command context.
	cmd := exec.Command(executable, args...)
	if dir := headlessRetryWorkingDir(parent); dir != "" {
		cmd.Dir = dir
	}

	cmd.Env = headlessRetryEnv(os.Environ(), parent)

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err == nil {
		defer devNull.Close()

		cmd.Stdin = devNull
		cmd.Stdout = devNull
		cmd.Stderr = devNull
	}

	configureHeadlessRetryCommand(cmd)

	if err := cmd.Start(); err != nil {
		return headlessRetryProcess{}, fmt.Errorf("start retry process: %w", err)
	}

	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return headlessRetryProcess{}, fmt.Errorf("release retry process %d: %w", pid, err)
	}

	return headlessRetryProcess{
		ID:         newID,
		Executable: executable,
		PID:        pid,
		Args:       args,
	}, nil
}

func headlessRetryExecutable(parent session.HeadlessRun) (string, error) {
	if executable := strings.TrimSpace(parent.Executable); headlessRetryRecordedFileUsable(executable) {
		return executable, nil
	}

	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}

	return executable, nil
}

func headlessRetryWorkingDir(parent session.HeadlessRun) string {
	cwd := strings.TrimSpace(parent.CWD)
	if !headlessRetryRecordedDirUsable(cwd) {
		return ""
	}

	return cwd
}

func headlessRetryRecordedFileUsable(path string) bool {
	if !headlessRetryRecordedValueUsable(path) {
		return false
	}

	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return !info.IsDir()
}

func headlessRetryRecordedDirUsable(path string) bool {
	if !headlessRetryRecordedValueUsable(path) {
		return false
	}

	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return info.IsDir()
}

func headlessRetryRecordedValueUsable(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.Contains(value, headlessRetryRedactedValue)
}

type headlessEnvOverride struct {
	key   string
	value string
}

func headlessRetryEnv(base []string, parent session.HeadlessRun) []string {
	return overrideEnv(base,
		headlessEnvOverride{key: headlessParentRunIDEnv, value: parent.ID},
		headlessEnvOverride{key: headlessRetryOfRunIDEnv, value: parent.ID},
		headlessEnvOverride{key: headlessRetryCountEnv, value: strconv.Itoa(parent.RetryCount + 1)},
	)
}

func overrideEnv(base []string, overrides ...headlessEnvOverride) []string {
	overrideByKey := make(map[string]string, len(overrides))

	overrideOrder := make([]string, 0, len(overrides))
	for _, override := range overrides {
		if override.key == "" {
			continue
		}

		if _, ok := overrideByKey[override.key]; !ok {
			overrideOrder = append(overrideOrder, override.key)
		}

		overrideByKey[override.key] = override.value
	}

	out := make([]string, 0, len(base)+len(overrideOrder))
	wroteOverride := make(map[string]struct{}, len(overrideOrder))

	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}

		value, shouldOverride := overrideByKey[key]
		if !shouldOverride {
			out = append(out, entry)
			continue
		}

		if _, alreadyWrote := wroteOverride[key]; alreadyWrote {
			continue
		}

		out = append(out, key+"="+value)
		wroteOverride[key] = struct{}{}
	}

	for _, key := range overrideOrder {
		if _, alreadyWrote := wroteOverride[key]; alreadyWrote {
			continue
		}

		out = append(out, key+"="+overrideByKey[key])
	}

	return out
}

func headlessRetryArgs(store *session.Store, parent session.HeadlessRun, newID string) ([]string, error) {
	if args, ok := headlessRetryArgsFromRecordedCommand(store, parent, newID); ok {
		return args, nil
	}

	if strings.TrimSpace(parent.Prompt) == "" {
		return nil, fmt.Errorf("headless run %q has no stored prompt to retry", parent.ID)
	}

	args := []string{
		"--session-dir", store.Dir(),
		"--headless",
		"--headless-id", newID,
	}

	if parent.PrivateLogs {
		args = append(args, "--headless-private-log")
	}

	if parent.Model != "" {
		args = append(args, "--model", parent.Model)
	}

	if parent.Agent != "" {
		args = append(args, "--agent", parent.Agent)
	}

	args = append(args, "chat", "once", "--", parent.Prompt)

	return args, nil
}

func headlessRetryArgsFromRecordedCommand(store *session.Store, parent session.HeadlessRun, newID string) ([]string, bool) {
	if len(parent.CommandArgs) <= 1 {
		return nil, false
	}

	args := append([]string(nil), parent.CommandArgs[1:]...)
	if !parent.PrivateLogs && headlessCommandArgsContainRedaction(args) {
		return nil, false
	}

	args, hasHeadless := replaceHeadlessRetryBoolFlag(args, "--headless", true)
	args, hasPrivateLogs := replaceHeadlessRetryBoolFlag(args, "--headless-private-log", parent.PrivateLogs)
	args, hasHeadlessID := replaceHeadlessRetryFlag(args, "--headless-id", newID)
	args, hasSessionDir := replaceHeadlessRetryFlag(args, "--session-dir", store.Dir())

	prepend := make([]string, 0, 5)
	if !hasSessionDir {
		prepend = append(prepend, "--session-dir", store.Dir())
	}

	if !hasHeadless {
		prepend = append(prepend, "--headless")
	}

	if !hasHeadlessID {
		prepend = append(prepend, "--headless-id", newID)
	}

	if parent.PrivateLogs && !hasPrivateLogs {
		prepend = append(prepend, "--headless-private-log")
	}

	if len(prepend) > 0 {
		args = append(prepend, args...)
	}

	return args, true
}

func headlessCommandArgsContainRedaction(args []string) bool {
	for _, arg := range args {
		if strings.Contains(arg, headlessRetryRedactedValue) {
			return true
		}
	}

	return false
}

func replaceHeadlessRetryFlag(args []string, name, value string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false

	aliases := []string{name}
	if suffix, ok := strings.CutPrefix(name, "--"); ok {
		aliases = append(aliases, "-"+suffix)
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out = append(out, args[i:]...)
			break
		}

		if retryFlagAliasMatches(arg, aliases) {
			found = true

			if name == "--headless" {
				out = append(out, name)
				continue
			}

			out = append(out, name)
			if i+1 < len(args) {
				out = append(out, value)
				i++
			} else {
				out = append(out, value)
			}

			continue
		}

		if retryFlagAliasHasValue(arg, aliases) {
			found = true

			if name == "--headless" {
				out = append(out, name+"=true")
			} else {
				out = append(out, name+"="+value)
			}

			continue
		}

		out = append(out, arg)
	}

	return out, found
}

func replaceHeadlessRetryBoolFlag(args []string, name string, enabled bool) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false

	aliases := []string{name}
	if suffix, ok := strings.CutPrefix(name, "--"); ok {
		aliases = append(aliases, "-"+suffix)
	}

	for i, arg := range args {
		if arg == "--" {
			out = append(out, args[i:]...)
			break
		}

		if retryFlagAliasMatches(arg, aliases) {
			found = true

			if enabled {
				out = append(out, name)
			}

			continue
		}

		if retryFlagAliasHasValue(arg, aliases) {
			found = true

			if enabled {
				out = append(out, name+"=true")
			}

			continue
		}

		out = append(out, arg)
	}

	return out, found
}

func retryFlagAliasMatches(arg string, aliases []string) bool {
	return slices.Contains(aliases, arg)
}

func retryFlagAliasHasValue(arg string, aliases []string) bool {
	for _, alias := range aliases {
		if strings.HasPrefix(arg, alias+"=") {
			return true
		}
	}

	return false
}

func formatHeadlessRetryProcess(process headlessRetryProcess) string {
	parts := []string{
		"retry_run=" + process.ID,
		"pid=" + strconv.Itoa(process.PID),
		"executable=" + formatHeadlessFieldValue(process.Executable),
		"command_args=" + formatHeadlessArgs(process.Args),
	}

	return strings.Join(parts, "\t")
}

func cleanupHeadlessRuns(ctx context.Context, store *session.Store, maxAge string) error {
	maxAge = strings.TrimSpace(maxAge)
	if maxAge == "" {
		return errors.New("cleanup headless runs: --headless-max-age is required")
	}

	parsed, err := time.ParseDuration(maxAge)
	if err != nil {
		return fmt.Errorf("cleanup headless runs: parse --headless-max-age: %w", err)
	}

	if authErr := authorizeHeadlessPermission(ctx, "cleanup headless runs", store, "", permission.OperationRead, permission.OperationWrite, permission.OperationMergeDelete); authErr != nil {
		return fmt.Errorf("cleanup headless runs: %w", authErr)
	}

	removed, err := store.CleanupHeadlessRuns(session.HeadlessRetentionPolicy{MaxAge: parsed})
	if err != nil {
		return fmt.Errorf("cleanup headless runs: %w", err)
	}

	if len(removed) == 0 {
		fmt.Println("No expired terminal headless runs found.")
		return nil
	}

	for i := range removed {
		fmt.Println(formatHeadlessRun(removed[i]))
	}

	return nil
}

func streamHeadlessLog(ctx context.Context, store *session.Store, id string) error {
	if id == "" {
		return errors.New("stream headless: id is required")
	}

	if err := authorizeHeadlessPermission(ctx, "stream headless run log", store, id, permission.OperationRead); err != nil {
		return fmt.Errorf("stream headless: %w", err)
	}

	offset := session.HeadlessLogOffset{}

	for {
		tail, err := store.TailHeadlessLog(id, session.HeadlessLogTailOptions{Offset: offset})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stream headless: %w", err)
		}

		if tail.Text != "" {
			fmt.Print(tail.Text)
		}

		offset = tail.NextOffset

		run, err := store.HeadlessRunStatus(id)
		if err != nil {
			return fmt.Errorf("stream headless: %w", err)
		}

		if headlessRunRecordingIsTerminal(run.Status) {
			return drainTerminalHeadlessLogTail(ctx, store, id, offset)
		}

		timer := time.NewTimer(headlessStreamPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("stream headless: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func drainTerminalHeadlessLogTail(ctx context.Context, store *session.Store, id string, offset session.HeadlessLogOffset) error {
	nextOffset := offset

	for {
		drainedOffset, wrote, err := drainHeadlessLogTail(store, id, nextOffset)
		if err != nil {
			return err
		}

		nextOffset = drainedOffset

		if !wrote {
			break
		}
	}

	timer := time.NewTimer(headlessTerminalDrainInterval)
	select {
	case <-ctx.Done():
		timer.Stop()
		return fmt.Errorf("stream headless: %w", ctx.Err())
	case <-timer.C:
	}

	for {
		drainedOffset, wrote, err := drainHeadlessLogTail(store, id, nextOffset)
		if err != nil {
			return err
		}

		nextOffset = drainedOffset

		if !wrote {
			return nil
		}
	}
}

func drainHeadlessLogTail(store *session.Store, id string, offset session.HeadlessLogOffset) (session.HeadlessLogOffset, bool, error) {
	tail, err := store.TailHeadlessLog(id, session.HeadlessLogTailOptions{Offset: offset})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return offset, false, fmt.Errorf("stream headless: %w", err)
	}

	if tail.Text != "" {
		fmt.Print(tail.Text)
	}

	return tail.NextOffset, tail.Text != "", nil
}

func listSessionSummaries(store *session.Store, tag string) ([]session.Summary, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		summaries, err := store.List()
		if err != nil {
			return nil, fmt.Errorf("list all sessions: %w", err)
		}

		return summaries, nil
	}

	summaries, err := store.ListByTag(tag)
	if err != nil {
		return nil, fmt.Errorf("list sessions by tag %q: %w", tag, err)
	}

	return summaries, nil
}

func listAgentPerformance(ctx context.Context, store *session.Store) error {
	if err := authorizeSessionStoreRead(ctx, store, "", "summarize agent performance"); err != nil {
		return fmt.Errorf("agent performance summary: %w", err)
	}

	summaries, err := store.AgentPerformanceSummary()
	if err != nil {
		return fmt.Errorf("agent performance summary: %w", err)
	}

	if len(summaries) == 0 {
		fmt.Println("No agent performance records found.")
		return nil
	}

	for i := range summaries {
		fmt.Println(formatAgentPerformanceSummary(summaries[i]))
	}

	return nil
}

func formatAgentPerformanceSummary(summary session.AgentPerformanceSummary) string {
	parts := []string{
		"agent=" + summary.Agent,
		"evaluations=" + strconv.Itoa(summary.EvaluationCount),
		"failures=" + strconv.Itoa(summary.FailureCount),
		"negative_knowledge=" + strconv.Itoa(summary.NegativeKnowledgeCount),
		"default_agent_sessions=" + strconv.Itoa(summary.DefaultAgentSessionCount),
		"routing_eligible=" + strconv.FormatBool(summary.Validity.RoutingEligible),
		"recency_window_days=" + strconv.Itoa(summary.RecentWindowDays),
	}
	if len(summary.EvaluationProvenance) > 0 {
		parts = append(parts, "provenance="+formatProvenanceCounts(summary.EvaluationProvenance))
	}

	if len(summary.RubricVersions) > 0 {
		parts = append(parts, "rubrics="+formatRubricVersionCounts(summary.RubricVersions))
	}

	if len(summary.Evaluators) > 0 {
		parts = append(parts, "evaluators="+formatEvaluatorCounts(summary.Evaluators))
	}

	if len(summary.ScoreBuckets) > 0 {
		parts = append(parts, "score_buckets="+formatScoreBuckets(summary.ScoreBuckets))
	}

	if summary.ScoredEvaluationCount > 0 && len(summary.ScoreBuckets) == 1 {
		parts = append(
			parts,
			"scored="+strconv.Itoa(summary.ScoredEvaluationCount),
			fmt.Sprintf("avg_score=%.2f", summary.AverageScore),
			"min_score="+strconv.Itoa(summary.MinScore),
			"max_score="+strconv.Itoa(summary.MaxScore),
		)
	}

	parts = appendPerformanceEvalMetrics(parts, summary)

	if len(summary.Outcomes) > 0 {
		parts = append(parts, "outcomes="+formatOutcomeCounts(summary.Outcomes))
	}

	if len(summary.NegativeKnowledgeBreakdown) > 0 {
		parts = append(parts, "negative_knowledge_breakdown="+formatNegativeKnowledgeBreakdown(summary.NegativeKnowledgeBreakdown))
	}

	parts = append(parts, formatPerformanceValidity(summary.Validity)...)

	if len(summary.Validity.Checks) > 0 {
		parts = append(parts, "validity_checks="+strings.Join(summary.Validity.Checks, ","))
	}

	if len(summary.Validity.Reasons) > 0 {
		parts = append(parts, "validity_reasons="+strings.Join(summary.Validity.Reasons, ","))
	}

	if !summary.LatestActivity.IsZero() {
		parts = append(parts, "latest="+summary.LatestActivity.UTC().Format(time.RFC3339))
	}

	return strings.Join(parts, "\t")
}

func appendPerformanceEvalMetrics(parts []string, summary session.AgentPerformanceSummary) []string {
	if summary.PassRateSampleCount > 0 {
		parts = append(parts,
			"pass_rate_samples="+strconv.Itoa(summary.PassRateSampleCount),
			fmt.Sprintf("avg_pass_rate=%.2f", summary.AveragePassRate),
		)
	}

	if summary.FlakeCount > 0 {
		parts = append(parts,
			"flake_count="+strconv.Itoa(summary.FlakeCount),
			"flaky_evaluations="+strconv.Itoa(summary.FlakyEvaluationCount),
		)
	}

	if summary.TokenSampleCount > 0 {
		parts = append(parts,
			"token_samples="+strconv.Itoa(summary.TokenSampleCount),
			"input_tokens="+strconv.Itoa(summary.InputTokens),
			"output_tokens="+strconv.Itoa(summary.OutputTokens),
			"total_tokens="+strconv.Itoa(summary.TotalTokens),
		)
	}

	if summary.DurationSampleCount > 0 {
		parts = append(parts,
			"duration_samples="+strconv.Itoa(summary.DurationSampleCount),
			fmt.Sprintf("avg_duration_ms=%.2f", summary.AverageDurationMillis),
		)
	}

	if summary.CostSampleCount > 0 {
		parts = append(parts,
			"cost_samples="+strconv.Itoa(summary.CostSampleCount),
			fmt.Sprintf("total_cost=%.6f", summary.TotalCost),
			fmt.Sprintf("avg_cost=%.6f", summary.AverageCost),
		)
	}

	return parts
}

func formatPerformanceValidity(validity session.PerformanceValidity) []string {
	if validity.MinimumSampleSize == 0 &&
		validity.MinimumRecentSamples == 0 &&
		validity.MaximumStandardError == 0 &&
		validity.MinimumMeanConfidence == 0 {
		return nil
	}

	return []string{
		"validity_eligible_buckets=" + strconv.Itoa(validity.EligibleScoreBuckets),
		"validity_min_sample_size=" + strconv.Itoa(validity.MinimumSampleSize),
		"validity_min_recent_samples=" + strconv.Itoa(validity.MinimumRecentSamples),
		fmt.Sprintf("validity_max_stderr=%.2f", validity.MaximumStandardError),
		fmt.Sprintf("validity_min_confidence=%.2f", validity.MinimumMeanConfidence),
	}
}

func formatProvenanceCounts(counts []session.ProvenanceCount) string {
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, count.Source+":"+strconv.Itoa(count.Count))
	}

	return strings.Join(parts, ",")
}

func formatRubricVersionCounts(counts []session.RubricVersionCount) string {
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, count.RubricVersion+":"+strconv.Itoa(count.Count))
	}

	return strings.Join(parts, ",")
}

func formatEvaluatorCounts(counts []session.EvaluatorCount) string {
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, count.Evaluator+":"+strconv.Itoa(count.Count))
	}

	return strings.Join(parts, ",")
}

func formatScoreBuckets(buckets []session.ScoreBucketSummary) string {
	parts := make([]string, 0, len(buckets))
	for i := range buckets {
		bucket := &buckets[i]

		fields := []string{
			"source=" + bucket.Source,
			"rubric=" + bucket.RubricVersion,
			"task=" + bucket.TaskType,
			"difficulty=" + bucket.Difficulty,
		}
		if bucket.Provider != "" {
			fields = append(fields, "provider="+bucket.Provider)
		}

		fields = append(fields, "model="+bucket.Model)
		if bucket.FixtureVersion != "" {
			fields = append(fields, "fixture_version="+bucket.FixtureVersion)
		}

		fields = append(fields,
			"agent_version="+bucket.AgentVersion,
			"routing_eligible="+strconv.FormatBool(bucket.RoutingEligible),
			"sample="+strconv.Itoa(bucket.SampleSize),
			fmt.Sprintf("avg=%.2f", bucket.AverageScore),
			fmt.Sprintf("ci95=%.2f..%.2f", bucket.ConfidenceIntervalLow, bucket.ConfidenceIntervalHigh),
			fmt.Sprintf("stderr=%.2f", bucket.StandardError),
			"uncertainty="+bucket.Uncertainty,
			"recent_sample="+strconv.Itoa(bucket.RecentSampleSize),
			fmt.Sprintf("recent_avg=%.2f", bucket.RecentAverageScore),
			"previous_sample="+strconv.Itoa(bucket.PreviousSampleSize),
			fmt.Sprintf("previous_avg=%.2f", bucket.PreviousAverageScore),
			"regression="+bucket.RegressionStatus,
		)
		if bucket.RecentSampleSize > 0 && bucket.PreviousSampleSize > 0 {
			fields = append(fields, fmt.Sprintf("regression_delta=%.2f", bucket.RegressionDelta))
		}

		if !bucket.LatestScoreAt.IsZero() {
			fields = append(fields, "latest_score="+bucket.LatestScoreAt.UTC().Format(time.RFC3339))
		}

		if !bucket.RecentWindowStart.IsZero() {
			fields = append(fields, "recent_since="+bucket.RecentWindowStart.UTC().Format(time.RFC3339))
		}

		fields = appendScoreBucketEvalMetrics(fields, bucket)

		if len(bucket.ValidityReasons) > 0 {
			fields = append(fields, "validity_reasons="+strings.Join(bucket.ValidityReasons, "|"))
		}

		parts = append(parts, strings.Join(fields, "/"))
	}

	return strings.Join(parts, ";")
}

func appendScoreBucketEvalMetrics(fields []string, bucket *session.ScoreBucketSummary) []string {
	if bucket.ConfidenceSampleCount > 0 {
		fields = append(fields,
			"confidence_sample="+strconv.Itoa(bucket.ConfidenceSampleCount),
			fmt.Sprintf("avg_confidence=%.2f", bucket.AverageConfidence),
		)
	}

	if bucket.PassRateSampleCount > 0 {
		fields = append(fields,
			"pass_rate_sample="+strconv.Itoa(bucket.PassRateSampleCount),
			fmt.Sprintf("avg_pass_rate=%.2f", bucket.AveragePassRate),
		)
	}

	if bucket.FlakeCount > 0 {
		fields = append(fields,
			"flake_count="+strconv.Itoa(bucket.FlakeCount),
			"flaky_evaluations="+strconv.Itoa(bucket.FlakyEvaluationCount),
		)
	}

	if bucket.DurationSampleCount > 0 {
		fields = append(fields,
			"duration_sample="+strconv.Itoa(bucket.DurationSampleCount),
			fmt.Sprintf("avg_duration_ms=%.2f", bucket.AverageDurationMillis),
		)
	}

	if bucket.CostSampleCount > 0 {
		fields = append(fields,
			"cost_sample="+strconv.Itoa(bucket.CostSampleCount),
			fmt.Sprintf("total_cost=%.6f", bucket.TotalCost),
			fmt.Sprintf("avg_cost=%.6f", bucket.AverageCost),
		)
	}

	if bucket.TokenSampleCount > 0 {
		fields = append(fields,
			"token_sample="+strconv.Itoa(bucket.TokenSampleCount),
			"input_tokens="+strconv.Itoa(bucket.InputTokens),
			"output_tokens="+strconv.Itoa(bucket.OutputTokens),
			"total_tokens="+strconv.Itoa(bucket.TotalTokens),
		)
	}

	return fields
}

func formatNegativeKnowledgeBreakdown(counts []session.NegativeKnowledgeCategoryCount) string {
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		parts = append(parts, count.TaskType+"/"+count.Severity+":"+strconv.Itoa(count.Count))
	}

	return strings.Join(parts, ",")
}

func formatOutcomeCounts(outcomes []session.OutcomeCount) string {
	parts := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		parts = append(parts, outcome.Outcome+":"+strconv.Itoa(outcome.Count))
	}

	return strings.Join(parts, ",")
}

func listSessionTags(ctx context.Context, store *session.Store) error {
	if err := authorizeSessionStoreRead(ctx, store, "", "list session tags"); err != nil {
		return fmt.Errorf("list session tags: %w", err)
	}

	tags, err := store.Tags()
	if err != nil {
		return fmt.Errorf("list session tags: %w", err)
	}

	if len(tags) == 0 {
		fmt.Println("No session tags found.")
		return nil
	}

	for _, tag := range tags {
		fmt.Println(formatTagSummary(tag))
	}

	return nil
}

func formatTagSummary(tag session.TagSummary) string {
	return fmt.Sprintf("%s\t%d sessions", tag.Tag, tag.Sessions)
}

func listHookEvents(jsonOutput bool) error {
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(events.SupportedEventTypes()); err != nil {
			return fmt.Errorf("list hook events: encode JSON: %w", err)
		}

		return nil
	}

	for _, eventType := range events.SupportedEventTypes() {
		fmt.Println(formatHookEventType(eventType))
	}

	return nil
}

func formatHookEventType(eventType events.SupportedEventType) string {
	return eventType.Type + "\t" + eventType.Description
}

func searchSessions(ctx context.Context, store *session.Store, query string) error {
	if err := authorizeSessionSearchSideEffects(ctx, store); err != nil {
		return fmt.Errorf("search sessions: %w", err)
	}

	results, err := store.Search(query)
	if err != nil {
		return fmt.Errorf("search sessions: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No matching sessions found.")
		return nil
	}

	for i := range results {
		result := &results[i]
		fmt.Println(formatSessionSummary(result.Summary))

		for snippetIndex := range result.Snippets {
			fmt.Println(formatSearchSnippet(result.Snippets[snippetIndex]))
		}
	}

	return nil
}

func searchSessionsWithAutonomy(store *session.Store, query string, level autonomy.Level) error {
	if !autonomy.Normalize(level).Allows(autonomy.ActionFileWrite) {
		return fmt.Errorf("%s", autonomy.DenialMessage(level, autonomy.ActionFileWrite, "--search"))
	}

	return searchSessions(rootContext(), store, query)
}

func authorizeSessionSearchSideEffects(ctx context.Context, store *session.Store) error {
	action := "search sessions"
	target := sessionReadTarget(store, "")

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Action: action,
		Source: "atteler.session",
		Target: target,
		Operations: []permission.Operation{
			{
				Kind:   permission.OperationRead,
				Action: action,
				Source: "atteler.session",
				Target: target,
			},
			{
				Kind:   permission.OperationWrite,
				Action: "update session search index",
				Source: "atteler.session",
				Target: target,
			},
		},
	})
	if decision.Allowed {
		return nil
	}

	return &permission.Error{Decision: decision}
}

func formatSessionSummary(summary session.Summary) string {
	updated := "-"
	if !summary.UpdatedAt.IsZero() {
		updated = summary.UpdatedAt.UTC().Format(time.RFC3339)
	}

	agentName := "-"
	if summary.DefaultAgent != "" {
		agentName = summary.DefaultAgent
	}

	modelName := "-"
	if summary.DefaultModel != "" {
		modelName = summary.DefaultModel
	}

	parts := []string{
		summary.ID,
		updated,
		fmt.Sprintf("%d messages", summary.Messages),
		"agent=" + agentName,
		"model=" + modelName,
	}
	if budget := formatAgentLoopBudgetCompact(summary.AgentLoopBudget); budget != "" {
		parts = append(parts, "budget="+budget)
	}

	if strings.TrimSpace(summary.Autonomy) != "" {
		parts = append(parts, "autonomy="+summary.Autonomy)
	}

	if summary.Title != "" {
		parts = append(parts, "title="+summary.Title)
	}

	if len(summary.Tags) > 0 {
		parts = append(parts, "tags="+strings.Join(summary.Tags, ","))
	}

	parts = append(parts, summary.Path)

	return strings.Join(parts, "\t")
}

func formatHeadlessRun(run session.HeadlessRun) string {
	started := "-"
	if !run.StartedAt.IsZero() {
		started = run.StartedAt.UTC().Format(time.RFC3339)
	}

	updated := "-"
	if !run.UpdatedAt.IsZero() {
		updated = run.UpdatedAt.UTC().Format(time.RFC3339)
	}

	heartbeat := "-"
	if !run.LastHeartbeatAt.IsZero() {
		heartbeat = run.LastHeartbeatAt.UTC().Format(time.RFC3339)
	}

	leaseExpires := "-"
	if !run.LeaseExpiresAt.IsZero() {
		leaseExpires = run.LeaseExpiresAt.UTC().Format(time.RFC3339)
	}

	agentName := fallbackDash(run.Agent)
	modelName := fallbackDash(run.Model)

	parts := []string{
		formatHeadlessFieldValue(run.ID),
		"status=" + string(run.Status),
		"session=" + fallbackDash(run.SessionID),
		"agent=" + agentName,
		"model=" + modelName,
		"autonomy=" + fallbackDash(run.Autonomy),
		"started=" + started,
		"updated=" + updated,
		"heartbeat=" + heartbeat,
		"lease_expires=" + leaseExpires,
		"log=" + fallbackDash(run.LogPath),
	}

	parts = appendHeadlessRunTimeDetails(parts, run)
	parts = appendHeadlessRunProcessDetails(parts, run)
	parts = appendHeadlessRunStorageDetails(parts, run)
	parts = appendHeadlessRunTerminalDetails(parts, run)

	return strings.Join(parts, "\t")
}

func appendHeadlessRunTimeDetails(parts []string, run session.HeadlessRun) []string {
	if run.EventsPath != "" {
		parts = append(parts, "events="+formatHeadlessFieldValue(run.EventsPath))
	}

	if run.CompletedAt != nil {
		parts = append(parts, "completed="+run.CompletedAt.UTC().Format(time.RFC3339))
	}

	if run.CanceledAt != nil {
		parts = append(parts, "canceled="+run.CanceledAt.UTC().Format(time.RFC3339))
	}

	if run.RetriedAt != nil {
		parts = append(parts, "retried="+run.RetriedAt.UTC().Format(time.RFC3339))
	}

	if run.ExpiredAt != nil {
		parts = append(parts, "expired="+run.ExpiredAt.UTC().Format(time.RFC3339))
	}

	return parts
}

func appendHeadlessRunProcessDetails(parts []string, run session.HeadlessRun) []string {
	parts = appendHeadlessRunPIDDetails(parts, run)
	parts = appendHeadlessRunRelationshipDetails(parts, run)
	parts = appendHeadlessRunCommandDetails(parts, run)

	return appendHeadlessRunHostDetails(parts, run)
}

func appendHeadlessRunPIDDetails(parts []string, run session.HeadlessRun) []string {
	if run.PID != 0 {
		parts = append(parts, "pid="+strconv.Itoa(run.PID))
	}

	if run.ParentPID != 0 {
		parts = append(parts, "ppid="+strconv.Itoa(run.ParentPID))
	}

	if run.ProcessGroupID != 0 {
		parts = append(parts, "pgid="+strconv.Itoa(run.ProcessGroupID))
	}

	if run.ExitCode != nil {
		parts = append(parts, "exit_code="+strconv.Itoa(*run.ExitCode))
	}

	return parts
}

func appendHeadlessRunRelationshipDetails(parts []string, run session.HeadlessRun) []string {
	if run.ParentRunID != "" {
		parts = append(parts, "parent_run="+formatHeadlessFieldValue(run.ParentRunID))
	}

	if run.RetryOfRunID != "" {
		parts = append(parts, "retry_of="+formatHeadlessFieldValue(run.RetryOfRunID))
	}

	if run.SupersededByRunID != "" {
		parts = append(parts, "superseded_by="+formatHeadlessFieldValue(run.SupersededByRunID))
	}

	if len(run.ChildRunIDs) > 0 {
		parts = append(parts, "child_runs="+formatHeadlessArgs(run.ChildRunIDs))
	}

	if run.RetryCount > 0 {
		parts = append(parts, "retry_count="+strconv.Itoa(run.RetryCount))
	}

	return parts
}

func appendHeadlessRunCommandDetails(parts []string, run session.HeadlessRun) []string {
	if run.StartMethod != "" {
		parts = append(parts, "start_method="+formatHeadlessFieldValue(run.StartMethod))
	}

	if run.Executable != "" {
		parts = append(parts, "executable="+formatHeadlessFieldValue(run.Executable))
	}

	if run.Version != "" {
		parts = append(parts, "version="+formatHeadlessFieldValue(run.Version))
	}

	if run.StartedCommand != "" {
		parts = append(parts, "command="+formatHeadlessFieldValue(run.StartedCommand))
	}

	if len(run.CommandArgs) > 0 {
		parts = append(parts, "command_args="+formatHeadlessArgs(run.CommandArgs))
	}

	return parts
}

func appendHeadlessRunHostDetails(parts []string, run session.HeadlessRun) []string {
	if run.Owner != "" {
		parts = append(parts, "owner="+formatHeadlessFieldValue(run.Owner))
	}

	if run.Hostname != "" {
		parts = append(parts, "host="+formatHeadlessFieldValue(run.Hostname))
	}

	if run.CWD != "" {
		parts = append(parts, "cwd="+formatHeadlessFieldValue(run.CWD))
	}

	return parts
}

func appendHeadlessRunStorageDetails(parts []string, run session.HeadlessRun) []string {
	if run.LogPath != "" {
		parts = append(parts, "log_chunk_pattern="+formatHeadlessFieldValue(run.LogPath)+".NNNNNN")
	}

	if run.ArtifactDir != "" {
		parts = append(parts, "artifacts="+formatHeadlessFieldValue(run.ArtifactDir))
	}

	if run.LogMaxChunkBytes != 0 {
		parts = append(parts, "log_max_chunk_bytes="+strconv.FormatInt(run.LogMaxChunkBytes, 10))
	}

	if run.LogMaxChunks != 0 {
		parts = append(parts, "log_max_chunks="+strconv.Itoa(run.LogMaxChunks))
	}

	return parts
}

func appendHeadlessRunTerminalDetails(parts []string, run session.HeadlessRun) []string {
	if run.StaleReason != "" {
		parts = append(parts, "stale_reason="+formatHeadlessFieldValue(run.StaleReason))
	}

	if run.OrphanedReason != "" {
		parts = append(parts, "orphaned_reason="+formatHeadlessFieldValue(run.OrphanedReason))
	}

	if run.CancellationReason != "" {
		parts = append(parts, "cancellation_reason="+formatHeadlessFieldValue(run.CancellationReason))
	}

	if run.TerminalReason != "" {
		parts = append(parts, "terminal_reason="+formatHeadlessFieldValue(run.TerminalReason))
	}

	if run.Status == session.HeadlessStatusStale || run.Status == session.HeadlessStatusOrphaned {
		parts = append(parts, "recover=atteler session recover-headless")
	}

	if run.Error != "" {
		parts = append(parts, "error="+formatHeadlessFieldValue(run.Error))
	}

	return parts
}

func fallbackDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}

	return formatHeadlessFieldValue(value)
}

func formatHeadlessFieldValue(value string) string {
	return strings.NewReplacer(
		"\t", " ",
		"\r", "\\r",
		"\n", "\\n",
	).Replace(value)
}

func formatHeadlessArgs(args []string) string {
	data, err := json.Marshal(args)
	if err != nil {
		return formatHeadlessFieldValue(strings.Join(args, " "))
	}

	return formatHeadlessFieldValue(string(data))
}

func formatSearchSnippet(snippet session.SearchSnippet) string {
	role := string(snippet.Role)
	if role == "" {
		role = "message"
	}

	if snippet.Text == "" {
		return "  " + role + ":"
	}

	return "  " + role + ": " + snippet.Text
}
