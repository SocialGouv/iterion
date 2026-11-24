package model

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestInjectedHooks_TheCheckoutCanNeitherForgeNorVetoThem is the ADR-162 U3
// claw witness: on a workspace its tenant did not write, iterion's own
// plugin-contributed hook groups come from memory (Task.InjectedHooks) and
// nothing else. The checkout's settings.json is not their authority, so it
// can neither add a group of its own nor — the case these rows were written
// for — switch the operator's off by committing a file that does not parse,
// a directory in its place, or a dangling link.
//
// Driven through the REAL path: ClawBackend.Execute → the Execute-side switch
// → registerInjectedHooks. GenerateTextDirect fires Stop in a defer, so each
// row's hook runs on any generation, and its canary is the observable.
//
// Mutations: registerInjectedHooks muted to `return 0` reddens every fork
// row's wantOwn assertion; the Execute-side switch muted to skip the
// untrusted case reddens "the checkout's own hook never runs".
func TestInjectedHooks_TheCheckoutCanNeitherForgeNorVetoThem(t *testing.T) {
	t.Parallel()

	stopHook := func(canary string) map[string][]any {
		return map[string][]any{"Stop": {map[string]any{
			"matcher": "",
			"hooks":   []any{map[string]any{"type": "command", "command": "touch " + canary}},
		}}}
	}
	settingsWith := func(canary string) string {
		return `{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"touch ` + canary + `"}]}]}}`
	}
	for _, row := range []struct {
		name      string
		trust     store.RunTrust
		plant     func(t *testing.T, ws, fork string)
		injected  bool // the operator enabled a hooks plugin
		wantOwn   bool // iterion's own hook ran
		wantForks bool // the checkout's own hook ran
	}{
		{"trusted: the file's hook runs", store.RunTrustDefault, func(t *testing.T, ws, fork string) {
			writeHookTestFile(t, filepath.Join(ws, ".claude", "settings.json"), settingsWith(fork))
		}, false, false, true},
		{"fork: an unparseable settings.json does not veto iterion's hook", store.RunTrustFork, func(t *testing.T, ws, _ string) {
			writeHookTestFile(t, filepath.Join(ws, ".claude", "settings.json"), `{ "hooks": `)
		}, true, true, false},
		{"fork: a directory in its place does not either", store.RunTrustFork, func(t *testing.T, ws, _ string) {
			if err := os.MkdirAll(filepath.Join(ws, ".claude", "settings.json"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, true, true, false},
		{"fork: a dangling link in its place does not either", store.RunTrustFork, func(t *testing.T, ws, _ string) {
			if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(ws, "does-not-exist"), filepath.Join(ws, ".claude", "settings.json")); err != nil {
				t.Fatal(err)
			}
		}, true, true, false},
		{"fork: the checkout's own hook never runs", store.RunTrustFork, func(t *testing.T, ws, fork string) {
			writeHookTestFile(t, filepath.Join(ws, ".claude", "settings.json"), settingsWith(fork))
		}, true, true, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			ws := t.TempDir()
			own := filepath.Join(t.TempDir(), "operator-hook-ran")
			fork := filepath.Join(t.TempDir(), "checkout-hook-ran")
			row.plant(t, ws, fork)
			var injected map[string][]any
			if row.injected {
				injected = stopHook(own)
			}
			backend, capture := newCapturingBackend()
			_, _ = backend.Execute(context.Background(), delegate.Task{
				NodeID: "n", Model: "test/m", UserPrompt: "review this diff",
				WorkDir: ws, WorkspaceTrust: row.trust, InjectedHooks: injected,
			})
			if len(capture.requests) == 0 {
				t.Fatal("the model was never called — this row proves nothing")
			}
			if _, err := os.Stat(own); (err == nil) != row.wantOwn {
				t.Errorf("iterion's own hook ran = %v, want %v", err == nil, row.wantOwn)
			}
			if _, err := os.Stat(fork); (err == nil) != row.wantForks {
				t.Errorf("the checkout's hook ran = %v, want %v", err == nil, row.wantForks)
			}
		})
	}
}

func writeHookTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
