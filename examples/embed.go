// Package examples exposes the recipe files shipped with iterion as an
// embed.FS so the binary can run them by basename from any working
// directory.
//
// Lookup order at launch time (see pkg/server.resolveWorkflowPath):
//  1. resolve the requested path against the server WorkDir;
//  2. on miss, if the path is a bare basename matching an embedded
//     recipe, materialise the embedded content into the run store
//     and use that path.
//
// Only top-level workflow files (.iter / .bot) and the curated
// `bots/*.bot` productized recipes are embedded. Companion .md design
// journals and large non-recipe assets (images, mcp test servers,
// github-actions YAML) are intentionally excluded to keep the binary
// slim. Archived legacy examples live under `.archive/examples/` and
// are NOT embedded. Bundle directories (`<name>/main.bot` + manifest
// + skills + prompts + attachments) are NOT embedded either — they
// have to be loaded by explicit path (`iterion run examples/<name>/`
// or against the packed `<name>.botz`); embedding them would lose
// the adjacent skills/prompts/attachments resources that make a
// bundle a bundle, plus encoding the whole tree as embedded bytes
// inflates the binary far more than a single .bot does.
package examples

import (
	"embed"
	"io/fs"
	"sort"
)

// The three productised bots (feature_dev, whole_improve_loop,
// branch_improve_loop) each ship as a single-file bundle: only
// `<name>/main.bot` is embedded — their manifest.yaml + README.md
// are stripped to keep the binary slim. Larger bundles
// (whats-next, doc-align, sec-audit-*, secured-renovacy) carry
// skills/prompts/attachments alongside main.bot and are deliberately
// NOT embedded; they have to be loaded by explicit path
// (`iterion run examples/<name>/` or against the packed `.botz`).
//
//go:embed feature_dev/main.bot whole_improve_loop/main.bot branch_improve_loop/main.bot
var Files embed.FS

// Get returns the contents of the embedded example with the given
// basename (e.g. "feature_dev/main.bot" or "skill/human_gate.bot").
// Returns ok=false if no such embedded recipe exists.
func Get(name string) ([]byte, bool) {
	data, err := Files.ReadFile(name)
	if err != nil {
		return nil, false
	}
	return data, true
}

// List returns the relative paths (within the embed FS) of all embedded
// workflow recipes, sorted alphabetically.
func List() []string {
	var out []string
	_ = fs.WalkDir(Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		out = append(out, path)
		return nil
	})
	sort.Strings(out)
	return out
}
