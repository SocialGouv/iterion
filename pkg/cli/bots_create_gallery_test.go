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
			if err := BotsCreate(BotsCreateOptions{Slug: tpl.ID, Template: tpl.ID}, p); err != nil {
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
			}
		})
	}
}
