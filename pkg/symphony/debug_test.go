package symphony

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/permission"
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

func TestStartDebugServer_PermissionPolicyDeniesNetworkBeforeListen(t *testing.T) {
	t.Parallel()

	policy := permission.DefaultPolicy()
	policy.SetMode(permission.OperationNetwork, permission.ModeDeny)

	auditDir := t.TempDir()
	ctx := permission.ContextWithPolicy(t.Context(), &policy)
	ctx = permission.ContextWithAuditDir(ctx, auditDir)

	server, err := StartDebugServer(ctx, DebugConfig{
		Enabled: true,
		Address: "127.0.0.1:0",
	}, staticDebugSnapshotter{}, nil)
	require.Error(t, err)
	require.Nil(t, server)
	require.True(t, permission.ErrDenied(err))
	assert.Contains(t, err.Error(), "permission.network.deny")

	auditData, readErr := os.ReadFile(filepath.Join(auditDir, "side_effects.jsonl"))
	require.NoError(t, readErr)
	assert.Contains(t, string(auditData), "start local debug HTTP server")
	assert.Contains(t, string(auditData), "permission.network.deny")
}
