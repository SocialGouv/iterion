package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestMergePluginHooks_UntrustedWritesNothingAndAnswersFromMemory is the
// ADR-162 U3 runtime witness for the hooks floor: on a workspace its tenant
// did not write, mergePluginHooks returns the merged record IN MEMORY and the
// checkout is neither read nor written — no settings.json created, none
// rewritten, no sidecar, and nothing the checkout plants (an unparseable
// file, a directory, a dangling link) vetoes the operator's hooks.
//
// The trusted control rows keep the pre-floor behavior: the file is written
// and the `.claude` dangling link vetoes the merge (complete=false, nothing
// written), so both directions of the change are pinned by value.
//
// Mutations: dropping the `!pass.Trusted()` early return reddens every fork
// row's not-written and not-vetoed assertions; muting pluginHooksBlob to nil
// reddens the blob assertions on both lanes.
func TestMergePluginHooks_UntrustedWritesNothingAndAnswersFromMemory(t *testing.T) {
	const userHook = `{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"checkout-hook"}]}]}}`
	for _, row := range []struct {
		name        string
		trust       store.RunTrust
		plant       func(t *testing.T, ws string) // nil: no .claude at all
		wantBlob    bool
		wantFile    bool // .claude/settings.json exists after the merge
		wantMutated bool // a pre-existing settings.json changed bytes
	}{
		{"fork: no .claude — nothing created, the blob answers", store.RunTrustFork, nil, true, false, false},
		{"fork: the checkout's own settings.json stays byte-exact, the blob answers", store.RunTrustFork, func(t *testing.T, ws string) {
			writeTestJSONFile(t, filepath.Join(ws, ".claude", "settings.json"), userHook)
		}, true, true, false},
		{"fork: an unparseable settings.json vetoes nothing", store.RunTrustFork, func(t *testing.T, ws string) {
			writeTestJSONFile(t, filepath.Join(ws, ".claude", "settings.json"), `{ "hooks": `)
		}, true, true, false},
		{"fork: a dangling .claude link vetoes nothing", store.RunTrustFork, func(t *testing.T, ws string) {
			if err := os.Symlink(filepath.Join(ws, "nowhere"), filepath.Join(ws, ".claude")); err != nil {
				t.Fatal(err)
			}
		}, true, false, false},
		{"trusted: the file is written", store.RunTrustDefault, nil, true, true, false},
		{"trusted: the checkout's own hook is preserved and the blob answers", store.RunTrustDefault, func(t *testing.T, ws string) {
			writeTestJSONFile(t, filepath.Join(ws, ".claude", "settings.json"), userHook)
		}, true, true, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("ITERION_HOME", home)
			writeHookyPlugin(t, home, true)
			ws := t.TempDir()
			settingsPath := filepath.Join(ws, ".claude", "settings.json")
			if row.plant != nil {
				row.plant(t, ws)
			}
			before, _ := os.ReadFile(settingsPath)

			blob, _, err := mergePluginHooks(ws, testMirrorPass(t, ws, row.trust), nil)
			if err != nil {
				t.Fatalf("mergePluginHooks: %v", err)
			}
			if got := len(blob["PreToolUse"]) > 0; got != row.wantBlob {
				t.Errorf("the merged record carries the plugin's group = %v, want %v: %v", got, row.wantBlob, blob)
			}
			after, aerr := os.ReadFile(settingsPath)
			if got := aerr == nil; got != row.wantFile {
				t.Errorf(".claude/settings.json exists = %v, want %v", got, row.wantFile)
			}
			if row.wantFile && row.plant != nil {
				if mutated := string(before) != string(after); mutated != row.wantMutated {
					t.Errorf("the checkout's settings.json changed bytes = %v, want %v", mutated, row.wantMutated)
				}
			}
			if !row.trust.Trusted() {
				// The sidecar is the merge's bookkeeping INSIDE the checkout:
				// on the untrusted lane it is never written either.
				if _, err := os.Lstat(filepath.Join(ws, ".claude", bundleMirrorMarkerDir, pluginHooksSidecar)); err == nil {
					t.Error("the hooks sidecar was written into an untrusted checkout")
				}
			}
		})
	}
}

// TestMergePluginHooks_TrustedDanglingLinkStillVetoes pins the trusted lane's
// pre-floor answer against the same probe the fork row shrugs off above: a
// `.claude` symlink (dangling or not) refuses the merge, complete=false —
// the veto the orphan pruner's gate depends on (#2061) — and nothing is
// written through the link.
func TestMergePluginHooks_TrustedDanglingLinkStillVetoes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	writeHookyPlugin(t, home, true)
	ws := t.TempDir()
	if err := os.Symlink(filepath.Join(ws, "nowhere"), filepath.Join(ws, ".claude")); err != nil {
		t.Fatal(err)
	}
	blob, complete, err := mergePluginHooks(ws, testMirrorPass(t, ws, store.RunTrustDefault), nil)
	if err != nil {
		t.Fatalf("the veto is a soft skip, not an error: %v", err)
	}
	if complete {
		t.Error("complete = true through a .claude symlink, want false (the pruner veto)")
	}
	if len(blob) != 0 {
		t.Errorf("a vetoed merge hands no record: nothing was merged, the blob must be empty, got %v", blob)
	}
}

func writeTestJSONFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
