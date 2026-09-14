package cli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// TestRunStudio_OnReady_RandomPort verifies that:
//   - Port=-1 yields an OS-assigned random port (not 4891)
//   - OnReady fires with a non-empty addr containing 127.0.0.1:<port>
//   - The server is reachable at that addr
func TestRunStudio_OnReady_RandomPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrCh := make(chan string, 1)
	done := make(chan error, 1)
	dir := t.TempDir()

	go func() {
		done <- cli.RunStudio(ctx, cli.StudioOptions{
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
		t.Fatalf("RunStudio returned before OnReady: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("OnReady did not fire within 5s")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(65 * time.Second):
		t.Fatal("RunStudio did not return after cancel")
	}
}

func TestRunStudio_RecoveryPassiveIsLoopbackOnlyAndDisablesDispatcher(t *testing.T) {
	if err := cli.RunStudio(context.Background(), cli.StudioOptions{
		Port: -1, Bind: "0.0.0.0", Dir: t.TempDir(), NoBrowser: true, RecoveryPassive: true,
	}, cli.NewPrinter(cli.OutputJSON)); err == nil {
		t.Fatal("recovery-passive accepted a non-loopback bind")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- cli.RunStudio(ctx, cli.StudioOptions{
			Port: -1, Bind: "127.0.0.1", Dir: t.TempDir(), NoBrowser: true,
			RecoveryPassive: true,
			OnReady:         func(addr string) { addrCh <- addr },
		}, cli.NewPrinter(cli.OutputJSON))
	}()

	var addr string
	select {
	case addr = <-addrCh:
	case err := <-done:
		t.Fatalf("RunStudio returned before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("recovery-passive studio did not become ready")
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr + "/api/server/info")
	if err != nil {
		t.Fatalf("GET server info: %v", err)
	}
	defer resp.Body.Close()
	var info struct {
		RecoveryPassive   bool `json:"recovery_passive"`
		DispatcherEnabled bool `json:"dispatcher_enabled"`
		TriggersEnabled   bool `json:"triggers_enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode server info: %v", err)
	}
	if !info.RecoveryPassive || info.DispatcherEnabled || info.TriggersEnabled {
		t.Fatalf("unexpected recovery-passive server info: %+v", info)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(65 * time.Second):
		t.Fatal("recovery-passive studio did not stop")
	}
}
