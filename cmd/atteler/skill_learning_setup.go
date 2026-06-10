package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tommoulard/atteler/pkg/autonomy"
	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/events"
	"github.com/tommoulard/atteler/pkg/llm"
	attskill "github.com/tommoulard/atteler/pkg/skill"
)

const (
	skillLearningObserverQueueSize = 256
	skillLearningFlushTimeout      = 2 * time.Second
)

type backgroundObserver struct {
	inner events.Observer
	queue chan observerWork
	ctx   context.Context
}

//nolint:govet // Queue item field order favors logical flow over padding.
type observerWork struct {
	ctx   context.Context
	event events.Event
	flush chan struct{}
}

type flushingObserver interface {
	Flush(context.Context) error
}

func skillLearningObserversFromOptions(ctx context.Context, opts attskill.LearningOptions, enabled bool) []events.Observer {
	if !enabled {
		return nil
	}

	return []events.Observer{newBackgroundObserver(ctx, attskill.NewLearner(opts), skillLearningObserverQueueSize)}
}

func skillLearningEffectiveEnabled(opts attskill.LearningOptions, configuredEnabled bool) bool {
	if !configuredEnabled {
		return false
	}

	state, err := attskill.NewLearningStore(opts.StoreDir).Load()
	if err != nil {
		return false
	}

	return !state.Disabled
}

func skillLearningEnabledForAutonomy(enabled bool, level autonomy.Level) bool {
	if autonomy.Normalize(level) == autonomy.Low {
		return false
	}

	return enabled
}

func skillLearningOptionsFromConfig(cfg appconfig.Config, cli cliOptions, getenv func(string) string) (attskill.LearningOptions, bool) {
	opts := attskill.DefaultLearningOptions()
	enabled := true
	enabledExplicit := false

	if cfg.SkillLearning.Enabled != nil {
		enabled = *cfg.SkillLearning.Enabled
		enabledExplicit = true
	}

	if cfg.SkillLearning.StoreDir != "" {
		opts.StoreDir = cfg.SkillLearning.StoreDir
	}

	if cfg.SkillLearning.SkillDir != "" {
		opts.SkillDir = cfg.SkillLearning.SkillDir
	}

	if cfg.SkillLearning.MaxObservations > 0 {
		opts.MaxObservations = cfg.SkillLearning.MaxObservations
	}

	if cfg.SkillLearning.MaxSteps > 0 {
		opts.MaxSteps = cfg.SkillLearning.MaxSteps
	}

	if cfg.SkillLearning.MinOccurrences > 0 {
		opts.MinOccurrences = cfg.SkillLearning.MinOccurrences
	}

	if getenv != nil {
		if raw := strings.TrimSpace(getenv(attskill.EnvSkillLearning)); raw != "" {
			enabled = parseEnabledEnv(raw)
			enabledExplicit = true
		}

		if dir := strings.TrimSpace(getenv(attskill.EnvSkillLearningDir)); dir != "" {
			opts.StoreDir = dir
		}

		if dir := strings.TrimSpace(getenv(attskill.EnvSkillLearningSkillDir)); dir != "" {
			opts.SkillDir = dir
		}
	}

	if cli.skillLearningDir != "" {
		opts.StoreDir = cli.skillLearningDir
	}

	if cli.skillLearningSkillDir != "" {
		opts.SkillDir = cli.skillLearningSkillDir
	}

	if enabledExplicit {
		opts.Enabled = &enabled
	}

	return opts, enabled
}

func skillLearningOptionsFromLoadedConfig(cfg appconfig.Config, cli cliOptions) (attskill.LearningOptions, bool) {
	return skillLearningOptionsFromConfig(cfg, cli, os.Getenv)
}

func newBackgroundObserver(ctx context.Context, inner events.Observer, capacity int) *backgroundObserver {
	if capacity <= 0 {
		capacity = skillLearningObserverQueueSize
	}

	observer := &backgroundObserver{
		inner: inner,
		queue: make(chan observerWork, capacity),
		ctx:   ctx,
	}

	go observer.run()

	return observer
}

//nolint:contextcheck // Uses the app lifecycle context captured at observer construction so queued background work is not canceled by short-lived request/tool contexts.
func (o *backgroundObserver) ObserveEvent(ctx context.Context, event events.Event) error {
	if o == nil || o.inner == nil || ctx == nil {
		return nil
	}

	if !skillLearningBackgroundEventCandidate(event.Type) {
		return nil
	}

	workCtx := o.ctx
	if workCtx == nil {
		workCtx = ctx
	}

	event, ok := cloneObserverEvent(event)
	if !ok {
		return nil
	}

	work := observerWork{
		// Decouple queued learning work from request-scoped cancellation. The
		// observer is already best-effort and flushed on shutdown; production
		// wiring gives it the app lifecycle context so short-lived one-shot/tool
		// contexts do not drop reusable observations after user-visible work
		// finishes.
		ctx:   workCtx,
		event: event,
	}

	select {
	case o.queue <- work:
	default:
		// Drop rather than blocking the user's active command or investigation.
	}

	return nil
}

// skillLearningBackgroundEventCandidate filters before queueing so raw command
// output/log events are never retained by the learning worker.
func skillLearningBackgroundEventCandidate(eventType string) bool {
	switch eventType {
	case events.CommandExecute, events.ToolExecute, events.UserMessage:
		return true
	default:
		return false
	}
}

func cloneObserverEvent(event events.Event) (events.Event, bool) {
	cloned := events.Event{
		Timestamp: event.Timestamp,
		Type:      event.Type,
		SessionID: event.SessionID,
	}

	observation, ok := attskill.ObservationFromEvent(event)
	if !ok {
		return events.Event{}, false
	}

	switch event.Type {
	case events.CommandExecute:
		command := strings.TrimSpace(strings.TrimPrefix(observation.Action, "run "))
		if command == "" {
			return events.Event{}, false
		}

		cloned.Metadata = filteredMetadata(event.Metadata, "source", "provider")
		if cloned.Metadata == nil {
			cloned.Metadata = make(map[string]string, 1)
		}

		cloned.Metadata["command"] = command
	case events.ToolExecute:
		tool := strings.TrimSpace(strings.TrimPrefix(observation.Action, "use tool "))
		if tool == "" {
			return events.Event{}, false
		}

		cloned.Metadata = map[string]string{"tool": tool}
	case events.UserMessage:
		cloned.Content = observation.Prompt
		cloned.Metadata = nil
	default:
		return events.Event{}, false
	}

	return cloned, true
}

func filteredMetadata(metadata map[string]string, keys ...string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			out[key] = value
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func (o *backgroundObserver) Flush(ctx context.Context) error {
	if o == nil {
		return nil
	}

	if ctx == nil {
		return nil
	}

	done := make(chan struct{})
	select {
	case o.queue <- observerWork{flush: done}:
	case <-ctx.Done():
		return fmt.Errorf("skill learning observer flush: %w", ctx.Err())
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("skill learning observer flush: %w", ctx.Err())
	}
}

func (o *backgroundObserver) run() {
	for work := range o.queue {
		if work.flush != nil {
			close(work.flush)
			continue
		}

		if work.ctx == nil {
			continue
		}

		if err := observeSkillLearningInner(work.ctx, o.inner, work.event); err != nil {
			continue
		}
	}
}

func observeSkillLearningInner(ctx context.Context, inner events.Observer, event events.Event) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("skill learning observer panic: %v", recovered)
		}
	}()

	if err := inner.ObserveEvent(ctx, event); err != nil {
		return fmt.Errorf("skill learning observer: %w", err)
	}

	return nil
}

func flushEventObservers(ctx context.Context, observers []events.Observer) {
	if ctx == nil || len(observers) == 0 {
		return
	}

	flushCtx, cancel := context.WithTimeout(ctx, skillLearningFlushTimeout)
	defer cancel()

	var wg sync.WaitGroup

	for _, observer := range observers {
		flushable, ok := observer.(flushingObserver)
		if !ok {
			continue
		}

		wg.Go(func() {
			if err := flushable.Flush(flushCtx); err != nil {
				return
			}
		})
	}

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-flushCtx.Done():
	}
}

func generatedSkillReferenceContext(prompt, storeDir, skillDir string, enabled bool) string {
	return generatedSkillReferenceContextWithManifest(prompt, storeDir, skillDir, enabled, contextref.Options{}).Content
}

func generatedSkillReferenceContextWithManifest(prompt, storeDir, skillDir string, enabled bool, opts contextref.Options) configuredReferenceContext {
	if !enabled {
		return configuredReferenceContext{}
	}

	refs, err := attskill.MatchingGeneratedSkills(prompt, attskill.ReferenceOptions{StoreDir: storeDir, SkillDir: skillDir})
	if err != nil || len(refs) == 0 {
		return configuredReferenceContext{}
	}

	return formatGeneratedSkillReferencesWithManifest(refs, opts)
}

func formatGeneratedSkillReferencesWithManifest(refs []attskill.GeneratedSkillReference, opts contextref.Options) configuredReferenceContext {
	if len(refs) == 0 {
		return configuredReferenceContext{}
	}

	loaded := make([]contextref.LoadedReference, 0, len(refs))
	referenceEvents := make([]contextref.ReferenceEvent, 0, len(refs))

	for i := range refs {
		ref := refs[i]
		contentBytes := []byte(ref.Content)

		policyDecision := contextref.ReferenceDecisionLoaded
		if ref.Truncated {
			policyDecision = contextref.ReferenceDecisionTruncated
		}

		tokenEstimate, tokenEstimator := estimateGeneratedSkillReferenceContent(opts, contentBytes)

		policyReason := "active generated skill matched current prompt"
		if ref.Truncated {
			policyReason = "generated skill byte limit reached"
		}

		policyReasonCode := contextref.ReferenceReasonCode(policyDecision, policyReason)

		event := contextref.ReferenceEvent{
			Source:           ref.Path,
			Kind:             "file",
			Scope:            "generated-skill:" + ref.Slug,
			Location:         "local",
			TokenEstimator:   tokenEstimator,
			Bytes:            len(contentBytes),
			Truncated:        ref.Truncated,
			DigestSHA256:     digestString(contentBytes),
			FetchedAt:        time.Now().UTC(),
			PolicyDecision:   policyDecision,
			PolicyReason:     policyReason,
			PolicyReasonCode: policyReasonCode,
			TokenEstimate:    tokenEstimate,
		}
		referenceEvents = append(referenceEvents, event)
		loaded = append(loaded, contextref.LoadedReference{
			Source:    ref.Path,
			Kind:      "file",
			Content:   ref.Content,
			Bytes:     len(contentBytes),
			Truncated: ref.Truncated,
			Provenance: contextref.ReferenceProvenance{
				Scope:            "generated-skill:" + ref.Slug,
				Location:         "local",
				Size:             len(contentBytes),
				TokenEstimator:   tokenEstimator,
				Truncated:        ref.Truncated,
				DigestSHA256:     digestString(contentBytes),
				FetchedAt:        event.FetchedAt,
				PolicyDecision:   policyDecision,
				PolicyReason:     policyReason,
				PolicyReasonCode: policyReasonCode,
				TokenEstimate:    tokenEstimate,
			},
		})
	}

	formatted := contextref.FormatReferences(loaded)
	if formatted == "" {
		return configuredReferenceContext{}
	}

	return configuredReferenceContext{
		Content: strings.Join([]string{
			"Generated skills matched this request in the background.",
			"Use them only when they fit the user's current workflow; do not mention this background selection unless asked.",
			"",
			formatted,
		}, "\n"),
		Manifest:  withReferenceManifestEstimator(contextref.BuildReferenceManifest(referenceEvents), estimatorSummaryForContextOptions(opts)),
		Estimator: estimatorSummaryForContextOptions(opts),
	}
}

func estimateGeneratedSkillReferenceContent(opts contextref.Options, content []byte) (estimate contextpack.TokenEstimate, estimatorSummary string) {
	estimator := opts.TokenEstimator
	if estimator == nil {
		estimator = contextpack.DefaultEstimator()
	}

	estimate = estimator.EstimateMessage(llm.Message{
		Role:    llm.RoleSystem,
		Content: string(content),
	})

	return estimate, contextEstimatorSummary(estimator.Profile())
}

func appendReferenceContext(base, extra string) string {
	base = strings.TrimSpace(base)
	extra = strings.TrimSpace(extra)

	if base == "" {
		return extra
	}

	if extra == "" {
		return base
	}

	return base + "\n\n" + extra
}

func digestString(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func parseEnabledEnv(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", negativeFalse, "no", "off", "disabled", "disable":
		return false
	default:
		return true
	}
}
