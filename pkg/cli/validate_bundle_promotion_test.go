package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunValidate_BareMainBotOfABundleSeesItsPrompts: `iterion validate
// <bundle>/main.bot` gives the verdict of `iterion validate <bundle>`. The
// bare file is promoted to its bundle by openBundleOrFile — the one
// helper run, resume, validate and doctor open a workflow through — so
// the prompts/*.md the bundle ships are in scope. The multi-file gallery
// shape, which declares no prompt in main.bot, used to validate INVALID
// (C003 twice) as a file and OK as a directory.
func TestRunValidate_BareMainBotOfABundleSeesItsPrompts(t *testing.T) {
	inTempWorkspace(t)
	p, _ := testPrinter()
	// The model and backend are pinned: `validate` does not waive C018
	// (no model and no credential the host can detect), and a bare CI has
	// no credential — the test measures the bundle promotion, not the host.
	if err := BotsCreate(BotsCreateOptions{Slug: "mf", Template: "multi-file", Model: "anthropic/claude-opus-4-8", Backend: "claude_code"}, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}
	for _, target := range []string{filepath.Join("bots", "mf", "main.bot"), filepath.Join("bots", "mf")} {
		jp, out := jsonPrinter()
		if err := RunValidate(target, jp); err != nil {
			t.Fatalf("validate %s: %v\n%s", target, err, out.String())
		}
		var result struct {
			Valid       bool     `json:"valid"`
			Diagnostics []any    `json:"diagnostics"`
			Bundle      string   `json:"bundle"`
			Compile     []string `json:"compile_diagnostics"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatalf("validate %s: output is not JSON: %v\n%s", target, err, out.String())
		}
		if !result.Valid {
			t.Errorf("validate %s: invalid: %v", target, result.Compile)
		}
	}
}

// TestRunValidate_BareMainBotKeepsTheBundleDirName: the promotion must not
// change WHICH directory name the bundle lint is told about. `bundleDir`
// is read off the path the operator passed, which on the promoted form is
// the FILE — so a bot carrying per-bot memory (the one check that reads it,
// C2xx name stability) was told dir="main.bot" and failed as a file while
// passing as a directory: the exact file/dir divergence the promotion
// exists to close, in the other direction.
func TestRunValidate_BareMainBotKeepsTheBundleDirName(t *testing.T) {
	inTempWorkspace(t)
	p, _ := testPrinter()
	// Pinned model and backend: `validate` does not waive C018, and a bare
	// CI has no credential to detect — the test measures the lint's dir name.
	if err := BotsCreate(BotsCreateOptions{Slug: "memo", Model: "anthropic/claude-opus-4-8", Backend: "claude_code"}, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}
	// Arm checkBundleNameStability: it only speaks for a bot that actually
	// uses per-bot memory. Manifest, workflow and dir are all "memo" here,
	// so it must stay silent whichever form is validated.
	mainBot := filepath.Join("bots", "memo", "main.bot")
	src, err := os.ReadFile(mainBot)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(src), "workflow memo:\n", "workflow memo:\n  auto_memory: on\n", 1)
	if patched == string(src) {
		t.Fatalf("the scaffolded workflow block moved; the fixture no longer arms the check:\n%s", src)
	}
	if err := os.WriteFile(mainBot, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{mainBot, filepath.Join("bots", "memo")} {
		jp, out := jsonPrinter()
		if err := RunValidate(target, jp); err != nil {
			t.Fatalf("validate %s: %v\n%s", target, err, out.String())
		}
		var result struct {
			Valid       bool     `json:"valid"`
			Compile     []string `json:"compile_diagnostics"`
			BundleDiags []string `json:"bundle_diagnostics"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatalf("validate %s: output is not JSON: %v\n%s", target, err, out.String())
		}
		if !result.Valid || len(result.BundleDiags) != 0 {
			t.Errorf("validate %s: valid=%v bundle=%v compile=%v; the three names agree, the lint must be silent",
				target, result.Valid, result.BundleDiags, result.Compile)
		}
	}
}
