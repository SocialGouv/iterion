//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// TestStderrCallback_ReceivesOutput tests Stderr callback invocation.
func TestStderrCallback_ReceivesOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var (
		mu          sync.Mutex
		stderrLines []string
	)

	for _, err := range codexsdk.Query(ctx, codexsdk.Text("Say 'hello'"),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithStderr(func(line string) {
			mu.Lock()
			defer mu.Unlock()
			stderrLines = append(stderrLines, line)
		}),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	t.Logf("Received %d stderr lines", len(stderrLines))
}

// TestStderrCallback_CapturesDebugInfo tests that stderr callback captures
// output from a query that produces verbose output.
func TestStderrCallback_CapturesDebugInfo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var (
		mu          sync.Mutex
		stderrLines []string
	)

	for _, err := range codexsdk.Query(ctx, codexsdk.Text("Say 'debug test'"),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithStderr(func(line string) {
			mu.Lock()
			defer mu.Unlock()
			stderrLines = append(stderrLines, line)
		}),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	t.Logf("Received %d stderr lines", len(stderrLines))

	if len(stderrLines) > 0 {
		t.Logf("First line: %s", stderrLines[0])
	}
}
