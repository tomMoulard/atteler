package llm

import (
	"errors"
	"fmt"
)

func joinFallbackFailures(failures []error) error {
	if len(failures) == 0 {
		return errors.New("no fallback attempts were made")
	}

	return errors.Join(failures...)
}

func (r *Registry) withReadinessContext(err error) error {
	if err == nil {
		return nil
	}

	r.mu.RLock()
	readiness := r.readinessContextLocked()
	r.mu.RUnlock()
	if readiness == "" {
		return err
	}

	return fmt.Errorf("%w (%s)", err, readiness)
}

func (r *Registry) modelResolutionLabel(model string) string {
	target := r.fallbackAttemptTarget(model)
	if target.label != "" {
		return target.label
	}

	return "unresolved"
}
