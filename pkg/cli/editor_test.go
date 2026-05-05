package cli_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// TestRunEditor_OnReady_RandomPort verifies that:
//   - Port=-1 yields an OS-assigned random port (not 4891)
//   - OnReady fires with a non-empty addr containing 127.0.0.1:<port>
//   - The server is reachable at that addr
func TestRunEditor_OnReady_RandomPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrCh := make(chan string, 1)
	done := make(chan error, 1)
	dir := t.TempDir()

	go func() {
		done <- cli.RunEditor(ctx, cli.EditorOptions{
			Port:      -1, // random
			Bind:      "127.0.0.1",
			Dir:       dir,
			NoBrowser: true,
			OnReady:   func(addr string) { addrCh <- addr },
		}, cli.NewPrinter(cli.OutputJSON))
	}()

	select {
	case addr := <-addrCh:
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Errorf("addr = %q, want 127.0.0.1:<port>", addr)
		}
		// Smoke-test the listener is real.
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://" + addr + "/api/effort-capabilities")
		if err != nil {
			t.Fatalf("GET /api/effort-capabilities: %v", err)
		}
		_ = resp.Body.Close()
	case err := <-done:
		t.Fatalf("RunEditor returned before OnReady: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("OnReady did not fire within 5s")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(65 * time.Second):
		t.Fatal("RunEditor did not return after cancel")
	}
}
