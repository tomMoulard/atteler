package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// LedgerSchemaVersion is the current append-only lifecycle ledger schema.
	LedgerSchemaVersion = 1

	// LedgerPhaseEvent records the redacted lifecycle event before delivery.
	LedgerPhaseEvent = "event"
	// LedgerPhaseHookQueued records a non-blocking hook queued for delivery.
	LedgerPhaseHookQueued = "hook_queued"
	// LedgerPhaseHookAttempt records one hook delivery attempt.
	LedgerPhaseHookAttempt = "hook_attempt"
	// LedgerPhaseHookDeadLetter records a hook that exhausted delivery attempts.
	LedgerPhaseHookDeadLetter = "hook_dead_letter"

	// HookDeliveryBlocking means the hook can fail the caller after retries.
	HookDeliveryBlocking = "blocking"
	// HookDeliveryNonBlocking means the hook is telemetry-only background work.
	HookDeliveryNonBlocking = "non_blocking"

	// HookOutcomeQueued means background delivery was accepted for later work.
	HookOutcomeQueued = "queued"
	// HookOutcomeSuccess means a hook attempt completed successfully.
	HookOutcomeSuccess = "success"
	// HookOutcomeFailed means a hook attempt exited or started unsuccessfully.
	HookOutcomeFailed = "failed"
	// HookOutcomeTimeout means a hook attempt exceeded its timeout.
	HookOutcomeTimeout = "timeout"
	// HookOutcomeCanceled means a hook attempt was canceled by context.
	HookOutcomeCanceled = "canceled"
	// HookOutcomeDenied means command policy denied a hook before start.
	HookOutcomeDenied = "denied"
	// HookOutcomeAuditFailed means Atteler could not persist command audit data.
	HookOutcomeAuditFailed = "audit_failed"
	// HookOutcomeDeadLetter means retries were exhausted.
	HookOutcomeDeadLetter = "dead_letter"
)

// Ledger is a thread-safe append-only JSONL writer for redacted lifecycle
// events and hook delivery attempts.
//
//nolint:govet // Field order follows lifecycle state, not memory layout.
type Ledger struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	closed bool
}

// LedgerRecord is one durable JSONL entry in the lifecycle event ledger.
//
//nolint:govet // JSON order follows read/debug flow.
type LedgerRecord struct {
	Timestamp             time.Time         `json:"timestamp"`
	Event                 *Event            `json:"event,omitempty"`
	Hook                  *HookLedgerRecord `json:"hook,omitempty"`
	SchemaVersion         int               `json:"schema_version"`
	Phase                 string            `json:"phase"`
	Outcome               string            `json:"outcome,omitempty"`
	ErrorSummary          string            `json:"error_summary,omitempty"`
	TimeoutClassification string            `json:"timeout_classification,omitempty"`
	Attempt               int               `json:"attempt,omitempty"`
	MaxAttempts           int               `json:"max_attempts,omitempty"`
	PayloadBytes          int               `json:"payload_bytes,omitempty"`
	DurationMillis        int64             `json:"duration_ms,omitempty"`
	StderrBytes           int               `json:"stderr_bytes,omitempty"`
}

// HookLedgerRecord describes a hook without exposing raw argv, environment, or
// payload data.
type HookLedgerRecord struct {
	EventID     string `json:"event_id,omitempty"`
	EventType   string `json:"event_type,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Command     string `json:"command,omitempty"`
	Delivery    string `json:"delivery,omitempty"`
	PayloadMode string `json:"payload_mode,omitempty"`
	TimeoutMS   int64  `json:"timeout_ms,omitempty"`
}

type hookLedgerRecord struct {
	Phase                 string
	Outcome               string
	ErrorSummary          string
	TimeoutClassification string
	Attempt               int
	MaxAttempts           int
	PayloadBytes          int
	DurationMillis        int64
	StderrBytes           int
}

// NewLedger creates an append-only lifecycle ledger. A nil writer disables the
// ledger and Append becomes a no-op.
func NewLedger(w io.Writer) *Ledger {
	if w == nil {
		return nil
	}

	return &Ledger{w: w}
}

// NewFileLedger opens path for append-only lifecycle ledger writes. Parent
// directories are created with owner-only permissions.
func NewFileLedger(path string) (*Ledger, error) {
	if path == "" {
		return nil, nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("events: create ledger directory %q: %s", ledgerPathLabel(dir), ledgerPathErrorSummary(err))
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("events: open ledger %q: %s", ledgerPathLabel(path), ledgerPathErrorSummary(err))
	}

	return &Ledger{w: file, closer: file}, nil
}

func ledgerPathLabel(path string) string {
	base := filepath.Base(filepath.Clean(path))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "ledger"
	}

	return sanitizeAuditLabel("ledger_path", base)
}

func ledgerPathErrorSummary(err error) string {
	if err == nil {
		return ""
	}

	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Err != nil {
		return pathErr.Err.Error()
	}

	return "filesystem error"
}

// Append writes record as one JSON line.
func (l *Ledger) Append(record LedgerRecord) error {
	if l == nil || l.w == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return errors.New("events: ledger is closed")
	}

	if record.SchemaVersion == 0 {
		record.SchemaVersion = LedgerSchemaVersion
	}

	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("events: marshal ledger record: %w", err)
	}

	if _, err := l.w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("events: write ledger record: %w", err)
	}

	if flusher, ok := l.w.(flushWriter); ok {
		if err := flusher.Flush(); err != nil {
			return fmt.Errorf("events: flush ledger record: %w", err)
		}
	}

	return nil
}

// Close closes the ledger when it owns a file handle.
func (l *Ledger) Close() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return nil
	}

	l.closed = true

	if l.closer == nil {
		return nil
	}

	if err := l.closer.Close(); err != nil {
		return fmt.Errorf("events: close ledger: %w", err)
	}

	return nil
}

func (r *Runner) appendLedgerEvent(event Event) error {
	if r == nil || r.ledger == nil {
		return nil
	}

	ledgerEvent := sanitizeEventForHook(event, PayloadSummary)
	ledgerEvent.SchemaVersion = EventSchemaVersion

	if err := r.ledger.Append(LedgerRecord{
		Phase:   LedgerPhaseEvent,
		Event:   &ledgerEvent,
		Outcome: HookOutcomeSuccess,
	}); err != nil {
		return fmt.Errorf("events: append lifecycle ledger: %w", err)
	}

	return nil
}

func (r *Runner) appendHookLedgerRecord(hook Hook, event Event, result hookLedgerRecord) {
	if r == nil || r.ledger == nil {
		return
	}

	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	record := LedgerRecord{
		Phase:                 result.Phase,
		Outcome:               result.Outcome,
		ErrorSummary:          result.ErrorSummary,
		TimeoutClassification: result.TimeoutClassification,
		Attempt:               result.Attempt,
		MaxAttempts:           result.MaxAttempts,
		PayloadBytes:          result.PayloadBytes,
		DurationMillis:        result.DurationMillis,
		StderrBytes:           result.StderrBytes,
		Hook: &HookLedgerRecord{
			EventID:     hookLedgerEventID(event),
			EventType:   hookLedgerEventType(event),
			SessionID:   hookLedgerSessionID(event),
			Command:     hookAuditCommand(hook.Command),
			Delivery:    hookDelivery(hook),
			PayloadMode: string(normalizePayloadMode(event.PayloadMode)),
			TimeoutMS:   timeout.Milliseconds(),
		},
	}

	if err := r.ledger.Append(record); err != nil {
		auditEvent := Event{}
		safeType := sanitizeEventType(&auditEvent, event.Type)
		slogWarnHookDelivery(safeType, hook, err)
	}
}

func hookDelivery(hook Hook) string {
	if hook.Blocking {
		return HookDeliveryBlocking
	}

	return HookDeliveryNonBlocking
}

func hookLedgerEventID(event Event) string {
	if event.EventID == "" {
		return ""
	}

	return sanitizeScalar("event_id", event.EventID)
}

func hookLedgerEventType(event Event) string {
	auditEvent := Event{}

	return sanitizeEventType(&auditEvent, event.Type)
}

func hookLedgerSessionID(event Event) string {
	if event.SessionID == "" {
		return ""
	}

	return sanitizeScalar("session_id", event.SessionID)
}
