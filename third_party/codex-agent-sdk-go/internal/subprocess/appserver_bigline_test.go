package subprocess

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/stretchr/testify/require"
)

// bigLineNotificationSize is comfortably above the historical 1 MB scanner
// cap that made the transport die silently on image-generation events.
const bigLineNotificationSize = 3 * 1024 * 1024

// writeFakeAppServer writes an executable script that speaks just enough of
// the app-server JSON-RPC protocol for AppServerTransport.Start: it answers
// the initialize request, then emits one oversized notification followed by
// a small marker notification, and stays alive until stdin closes.
func writeFakeAppServer(t *testing.T) string {
	t.Helper()

	script := fmt.Sprintf(`#!/usr/bin/env python3
import json, sys

line = sys.stdin.readline()
req = json.loads(line)
sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": req["id"], "result": {}}) + "\n")
sys.stdout.flush()

big = "x" * %d
sys.stdout.write(json.dumps({"jsonrpc": "2.0", "method": "codex/event/big", "params": {"data": big}}) + "\n")
sys.stdout.write(json.dumps({"jsonrpc": "2.0", "method": "codex/event/done", "params": {}}) + "\n")
sys.stdout.flush()

sys.stdin.read()
`, bigLineNotificationSize)

	path := filepath.Join(t.TempDir(), "fake-codex")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))

	return path
}

// TestAppServerTransportHandlesMultiMegabyteLines is the regression test for
// the silent session death on image-generation events: codex inlines the
// generated image as base64 in a single JSON-RPC line (several MB), which
// overflowed the old 1 MB scanner buffer and ended the session without any
// result or error.
func TestAppServerTransportHandlesMultiMegabyteLines(t *testing.T) {
	transport := NewAppServerTransport(slog.Default(), &config.Options{
		CliPath:          writeFakeAppServer(t),
		SkipVersionCheck: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	require.NoError(t, transport.Start(ctx))

	t.Cleanup(func() { _ = transport.Close() })

	var methods []string

	for notif := range transport.Notifications() {
		methods = append(methods, notif.Method)

		if notif.Method == "codex/event/big" {
			var params map[string]any

			require.NoError(t, json.Unmarshal(notif.Params, &params))

			data, _ := params["data"].(string)
			require.Len(t, data, bigLineNotificationSize)
		}

		if notif.Method == "codex/event/done" {
			break
		}
	}

	require.Equal(t, []string{"codex/event/big", "codex/event/done"}, methods)
	require.NoError(t, transport.ReadErr())
}

// TestAppServerAdapterSurfacesTransportReadError verifies that when the
// inner transport dies from a fatal read error, the adapter forwards it to
// the errs channel instead of just closing the messages channel — so the
// query loop can report the failure rather than returning no result.
func TestAppServerAdapterSurfacesTransportReadError(t *testing.T) {
	mock := newMockAppServerRPC()
	mock.readErr = fmt.Errorf("bufio.Scanner: token too long")

	adapter := newTestAdapter(mock)

	t.Cleanup(func() { _ = adapter.Close() })

	// Simulate the transport read loop dying: channels close while the
	// adapter is still running.
	require.NoError(t, mock.Close())

	select {
	case err := <-adapter.errs:
		require.ErrorContains(t, err, "token too long")
	case <-time.After(5 * time.Second):
		t.Fatal("expected transport read error on errs channel")
	}
}

// TestAppServerAdapterNoErrorOnCleanShutdown verifies the errs channel stays
// empty when the transport channels close without a read error.
func TestAppServerAdapterNoErrorOnCleanShutdown(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	t.Cleanup(func() { _ = adapter.Close() })

	require.NoError(t, mock.Close())

	// messages closes once the adapter read loop drains both channels.
	for range adapter.messages { //nolint:revive // draining until close
	}

	select {
	case err, ok := <-adapter.errs:
		if ok {
			t.Fatalf("unexpected error on clean shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("errs channel should be closed after clean shutdown")
	}
}
