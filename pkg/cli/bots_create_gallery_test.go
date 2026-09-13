package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botscaffold"
)

// TestBotsCreate_EveryTemplateValidates walks the operator's first loop for
// EVERY gallery template — `iterion bots create <slug> --template <id>` then
// `iterion validate bots/<slug>` — and requires a clean verdict.
//
// Distinct from botscaffold's own TestGalleryShapes, which compiles the
// rendered workflow in-process: this is the CLI path, so it also covers the
// manifest cross-check (bundlelint: var maps, capabilities, per-bot-memory
// name stability) that only `validate` runs, and the bundle promotion the
// bare-file form goes through. A shape that compiles but whose manifest
// disagrees with it would ship a bundle whose very first validate is red.
func TestBotsCreate_EveryTemplateValidates(t *testing.T) {
	for _, tpl := range botscaffold.Templates() {
		t.Run(tpl.ID, func(t *testing.T) {
			inTempWorkspace(t)
			p, _ := testPrinter()
			// The model and backend are pinned: `validate` does not waive
			// C018 (no model and no credential the host can detect), a
			// bare CI has no credential, and the test measures the scaffold
			// and the bundle lint, not the host.
			if err := BotsCreate(BotsCreateOptions{Slug: tpl.ID, Template: tpl.ID, Model: "anthropic/claude-opus-4-8", Backend: "claude_code"}, p); err != nil {
				t.Fatalf("bots create --template %s: %v", tpl.ID, err)
			}
			// Both forms, because they are one verdict: the bundle
			// directory and the bare main.bot promoted to it.
			for _, target := range []string{filepath.Join("bots", tpl.ID), filepath.Join("bots", tpl.ID, "main.bot")} {
				jp, out := jsonPrinter()
				err := RunValidate(target, jp)
				var result struct {
					Valid       bool     `json:"valid"`
					Parse       []string `json:"parse_diagnostics"`
					Compile     []string `json:"compile_diagnostics"`
					BundleDiags []string `json:"bundle_diagnostics"`
				}
				if jerr := json.Unmarshal(out.Bytes(), &result); jerr != nil {
					t.Fatalf("validate %s: output is not JSON: %v\n%s", target, jerr, out.String())
				}
				if err != nil || !result.Valid {
					t.Errorf("validate %s: %v — parse=%v compile=%v bundle=%v",
						target, err, result.Parse, result.Compile, result.BundleDiags)
				}
				// Clean, not merely valid: compile_diagnostics carries the
				// WARNINGS too, and a canonical shape whose first validate
				// prints one teaches the pattern the warning exists to stamp
				// out (a ref inside author-written quotes, C137, shipped
				// that way once) while training operators to ignore it.
				if len(result.Compile) != 0 || len(result.Parse) != 0 {
					t.Errorf("validate %s: a template validates clean; got parse=%v compile=%v",
						target, result.Parse, result.Compile)
				}
			}
		})
	}
}
