//nolint:wsl_v5 // Notification-ordering tests use compact handler bookkeeping blocks.
package lsp

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startNotificationTestClient wires an rpcClient to an in-memory pipe so tests
// can feed framed JSON-RPC server messages straight into the read loop without
// spawning a language server process.
func startNotificationTestClient(t *testing.T) (*rpcClient, *io.PipeWriter) {
	t.Helper()

	reader, writer := io.Pipe()
	client := &rpcClient{
		pending:  make(map[int]chan rpcResponse),
		done:     make(chan struct{}),
		waitDone: make(chan struct{}),
		stderr:   newDiagnosticBuffer(4096, nil),
		stdout:   newDiagnosticBuffer(4096, nil),
	}

	client.start(reader)

	t.Cleanup(func() {
		_ = writer.Close()
	})

	return client, writer
}

func writeNotificationFrame(t *testing.T, writer io.Writer, method string, params any) {
	t.Helper()

	payload, err := json.Marshal(rpcMessage{JSONRPC: "2.0", Method: method, Params: params})
	require.NoError(t, err)
	require.NoError(t, writeFrame(writer, payload))
}

func waitForNotifications(t *testing.T, processed <-chan struct{}) {
	t.Helper()

	select {
	case <-processed:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "timed out waiting for notifications to be processed")
	}
}

func TestRPCClient_DispatchesNotificationsInReceiptOrder(t *testing.T) {
	t.Parallel()

	const notificationCount = 200

	client, writer := startNotificationTestClient(t)

	var (
		mu        sync.Mutex
		received  []int
		processed = make(chan struct{})
	)

	client.setNotificationHandler(func(_ string, params json.RawMessage) {
		var payload struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return
		}

		// Yield so unordered per-notification goroutines interleave freely.
		runtime.Gosched()

		mu.Lock()
		received = append(received, payload.Seq)
		if len(received) == notificationCount {
			close(processed)
		}
		mu.Unlock()
	})

	for i := range notificationCount {
		writeNotificationFrame(t, writer, "atteler/orderProbe", map[string]int{"seq": i})
	}

	waitForNotifications(t, processed)

	mu.Lock()
	defer mu.Unlock()

	expected := make([]int, 0, notificationCount)
	for i := range notificationCount {
		expected = append(expected, i)
	}

	assert.Equal(t, expected, received, "notifications must be delivered in receipt order")
}

func TestServerSession_StoresLastPublishedDiagnosticsAfterBurst(t *testing.T) {
	t.Parallel()

	const notificationCount = 200

	client, writer := startNotificationTestClient(t)
	session := newServerSession(serverKey{}, CommandSpec{}, client)

	var (
		handled   int
		mu        sync.Mutex
		processed = make(chan struct{})
	)

	client.setNotificationHandler(func(method string, params json.RawMessage) {
		session.handleNotification(method, params)

		mu.Lock()
		handled++
		if handled == notificationCount {
			close(processed)
		}
		mu.Unlock()
	})

	const uri = "file:///tmp/burst.go"

	for i := range notificationCount {
		params := publishDiagnosticsParams{
			URI:         uri,
			Diagnostics: []Diagnostic{{Message: fmt.Sprintf("stale-%d", i)}, {Message: "noise"}},
		}
		if i == notificationCount-1 {
			params.Diagnostics = []Diagnostic{{Message: "final"}}
		}

		writeNotificationFrame(t, writer, "textDocument/publishDiagnostics", params)
	}

	waitForNotifications(t, processed)

	diagnostics := session.diagnostics()
	require.Len(t, diagnostics, 1, "stored diagnostics must match the last notification, not a stale earlier one")
	assert.Equal(t, "final", diagnostics[0].Message)
	assert.Equal(t, uri, diagnostics[0].URI)
}
