package symphony

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staticDebugSnapshotter struct {
	err      error
	snapshot DebugSnapshot
}

func (s staticDebugSnapshotter) Snapshot(context.Context) (DebugSnapshot, error) {
	return s.snapshot, s.err
}

func TestDebugServerStatusAndEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	now := time.Now().UTC()
	server, err := StartDebugServer(ctx, DebugConfig{
		Enabled:    true,
		Address:    "127.0.0.1:0",
		EventLimit: 10,
	}, staticDebugSnapshotter{snapshot: DebugSnapshot{
		Now: now,
		Service: DebugServiceSnapshot{
			StartedAt:     now.Add(-time.Minute),
			UptimeSeconds: 60,
		},
		Counts: DebugCounts{
			AvailableSlots: 2,
		},
		Summary: DebugSummary{
			WhatIsGoingOn: []string{"GH-2 is running"},
			WhatWillDo:    []string{"publish on success"},
		},
		RecentEvents: []DebugEvent{{
			Timestamp: now,
			Kind:      "issue_dispatched",
			Issue:     "GH-2",
			Message:   "worker dispatched",
		}},
	}}, nil)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, server.Close())
	}()

	statusResp, err := http.Get("http://" + server.Address() + "/debug/status") //nolint:noctx // Test client is bounded by server shutdown.
	require.NoError(t, err)

	defer func() {
		require.NoError(t, statusResp.Body.Close())
	}()

	require.Equal(t, http.StatusOK, statusResp.StatusCode)

	var status DebugSnapshot
	require.NoError(t, json.NewDecoder(statusResp.Body).Decode(&status))
	assert.Equal(t, int64(60), status.Service.UptimeSeconds)
	assert.Equal(t, []string{"GH-2 is running"}, status.Summary.WhatIsGoingOn)
	assert.Len(t, status.RecentEvents, 1)

	eventsResp, err := http.Get("http://" + server.Address() + "/debug/events") //nolint:noctx // Test client is bounded by server shutdown.
	require.NoError(t, err)

	defer func() {
		require.NoError(t, eventsResp.Body.Close())
	}()

	require.Equal(t, http.StatusOK, eventsResp.StatusCode)

	var events struct {
		RecentEvents []DebugEvent `json:"recent_events"`
	}
	require.NoError(t, json.NewDecoder(eventsResp.Body).Decode(&events))
	require.Len(t, events.RecentEvents, 1)
	assert.Equal(t, "issue_dispatched", events.RecentEvents[0].Kind)
}

func TestDebugConfigSnapshotIncludesPublishVerificationControls(t *testing.T) {
	t.Parallel()

	got := debugConfigSnapshot(Config{
		Publish: PublishConfig{
			Enabled:                 true,
			BaseBranch:              "main",
			BranchPrefix:            "symphony",
			Draft:                   true,
			DraftOnFailedValidation: false,
			VerificationGates: []VerificationGateConfig{{
				Name:     "gate_token=secret-value",
				Command:  "printf api_key=super-secret",
				Timeout:  2 * time.Second,
				Required: true,
			}},
			VerificationAllowCommands:  []string{"printf"},
			VerificationDenyCommands:   []string{"curl"},
			VerificationOutputMaxBytes: 128,
		},
	})

	assert.True(t, got.PublishEnabled)
	assert.True(t, got.PublishDraft)
	assert.False(t, got.PublishDraftOnFailedValidation)
	assert.Equal(t, int64(128), got.PublishVerificationOutputMaxBytes)
	assert.Equal(t, []string{"printf"}, got.PublishVerificationAllowCommands)
	assert.Equal(t, []string{"curl"}, got.PublishVerificationDenyCommands)
	require.Len(t, got.PublishVerificationGates, 1)
	assert.True(t, got.PublishVerificationGates[0].Required)
	assert.Equal(t, int64(2000), got.PublishVerificationGates[0].TimeoutMS)
	assert.Contains(t, got.PublishVerificationGates[0].Name, "[REDACTED]")
	assert.Contains(t, got.PublishVerificationGates[0].Command, "[REDACTED]")
	assert.NotContains(t, got.PublishVerificationGates[0].Name, "secret-value")
	assert.NotContains(t, got.PublishVerificationGates[0].Command, "super-secret")
}

func TestDebugServer_RateLimitsRoundTripAsJSONObject(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server, err := StartDebugServer(ctx, DebugConfig{
		Enabled: true,
		Address: "127.0.0.1:0",
	}, staticDebugSnapshotter{snapshot: DebugSnapshot{
		Now:   time.Now().UTC(),
		Codex: debugCodexSnapshot(codexTotals{}, jsonRaw(`{"primary":{"used_percent":42}}`)),
	}}, nil)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, server.Close())
	}()

	statusResp, err := http.Get("http://" + server.Address() + "/debug/status") //nolint:noctx // Test client is bounded by server shutdown.
	require.NoError(t, err)

	defer func() {
		require.NoError(t, statusResp.Body.Close())
	}()

	require.Equal(t, http.StatusOK, statusResp.StatusCode)

	var status struct {
		Codex struct {
			RateLimits any `json:"rate_limits"`
		} `json:"codex"`
	}
	require.NoError(t, json.NewDecoder(statusResp.Body).Decode(&status))

	limits, ok := status.Codex.RateLimits.(map[string]any)
	require.True(t, ok, "rate_limits must decode as a JSON object, got %T (%v)", status.Codex.RateLimits, status.Codex.RateLimits)
	assert.Equal(t, map[string]any{"primary": map[string]any{"used_percent": float64(42)}}, limits)
}
