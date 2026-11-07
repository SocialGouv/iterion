package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// pluginHooksSidecar records the exact hooks blob iterion last injected into
// <workDir>/.claude/settings.json, so a re-run or resume can remove the prior
// injection before adding the current one — keeping the merge idempotent and
// never duplicating a plugin's hooks. Lives under the same managed dir as the
// skill markers.
const pluginHooksSidecar = "plugin-hooks.json"

// mergePluginHooks idempotently merges the `hooks` settings fragments
// contributed by every enabled plugin into <workDir>/.claude/settings.json,
// where claude_code discovers them via --setting-sources project and claw
// bridges the command-type subset (see settings_hooks.go). User hooks
// already in settings.json are preserved (only iterion's own prior injection,
// tracked in a sidecar, is removed before re-injecting). A plugin registry
// that cannot be loaded skips the merge ENTIRELY — a load failure is not an
// empty hook set, so the previously injected hooks stay as they are and the
// sidecar keeps recording them. A broken individual plugin is logged and
// skipped; an existing settings.json that cannot be parsed returns
// an error WITHOUT rewriting the file (callers downgrade to warn + skip, so a
// run never fails — but the user's file is never destroyed either).
// No-op when workDir is empty or nothing is (or was) injected.
//
// The returned blob is the merged record IN MEMORY, on every path: the
// engine pushes it to the executor (pushInjectedHooks), which stamps it on
// every task (delegate.Task.InjectedHooks). The sidecar on disk exists only
// so a later pass can remove what an earlier one added; it is never the
// authority for admitting anything, because it lives inside the workspace
// and a checkout can write it.
//
// FLOOR (ADR-162 U3): on a workspace its tenant did not write, the in-memory
// record is the whole answer and the workspace is not touched — nothing is
// read from it (the sidecar included) and nothing is written into it.
// Neither backend reads the file there — claw registers these groups from
// memory, claude_code gets them through `--settings` — so the merge would
// only be a write into the fork's tree. Worse, it made the fork the arbiter:
// every failure path below skips the injection, so a checkout with an
// unparseable settings.json, a directory in its place, or a `.claude` link
// (dangling or not) switched off every hook the operator enabled, their
// guards among them, on exactly the runs the lane exists for.
//
// A `.claude` symlink gets the ambient writer's soft-fail (#2061, mirroring
// mirrorPluginContributions' #2044 split): warn naming the link, skip the
// merge, and report complete=false so the caller vetoes the orphan pruner —
// the pruner walks `.claude/<kind>` itself and must not follow the link
// into its target. complete stays true on every other outcome: a skipped or
// failed hooks merge says nothing about skill orphans.
//
// A settings.json the merge CHANGES is recorded in the mirror manifest
// with its new content hash only when the file as found was still the
// mirror's (mirrorMayClaimSettings) — and a merge that changes nothing
// writes nothing at all, mtime included. Both rules exist for the resume
// paths: a previous segment's edit of a tracked settings.json must keep
// reading as the run's work (#1571, R213476), while a repository TRACKING
// the file with no edit in between must not see the engine's own injection
// banked either (#1364).
func mergePluginHooks(workDir string, pass *mirrorPass, logger *iterlog.Logger) (blob map[string][]any, complete bool, err error) {
	complete = true
	if workDir == "" {
		return nil, complete, nil
	}
	if !pass.Trusted() {
		// The untrusted lane reads and writes nothing here (the floor above):
		// the `.claude` symlink veto is the pruner's own entry check, which
		// stays armed — this pass simply never reaches the file.
		forkBlob, loadErr := pluginHooksBlob(logger)
		if loadErr != nil {
			if logger != nil {
				logger.Warn("runtime/plugin: load plugins for the hooks merge: %v — the run goes without operator plugin hooks this pass", loadErr)
			}
			return nil, complete, nil
		}
		return forkBlob, complete, nil
	}
	if err := refuseAClaudeSymlink(workDir); err != nil {
		if logger != nil {
			logger.Warn("runtime/plugin: %v — skipping the hooks merge this pass", err)
		}
		return nil, false, nil
	}
	newBlob, loadErr := pluginHooksBlob(logger)
	if loadErr != nil {
		// A registry that cannot be loaded is not an empty hook set: skipping
		// the merge entirely keeps the previously injected hooks in place —
		// mistaking the failure for "no plugin contributes" would strip them
		// from settings.json and null the sidecar, and both backends (which
		// read the file on this lane) would run without the operator's hooks
		// until the registry heals.
		if logger != nil {
			logger.Warn("runtime/plugin: load plugins for the hooks merge: %v — skipping the merge this pass; any hooks already injected stay as they are", loadErr)
		}
		return nil, complete, nil
	}

	claudeDir := filepath.Join(workDir, ".claude")
	settingsPath := filepath.Join(claudeDir, "settings.json")
	sidecarPath := filepath.Join(claudeDir, bundleMirrorMarkerDir, pluginHooksSidecar)
	prevBlob, prevErr := readHooksSidecar(pass.root(), sidecarPath)
	if prevErr != nil && logger != nil {
		logger.Warn("runtime/plugin: hooks sidecar: %v — prior injected hooks may be left stale", prevErr)
	}

	if len(newBlob) == 0 && len(prevBlob) == 0 {
		return newBlob, complete, nil // nothing to do, and nothing was ever injected
	}

	settings, err := readJSONObject(pass.root(), settingsPath)
	if err != nil {
		return newBlob, complete, fmt.Errorf("runtime/plugin: %w — refusing to rewrite %s", err, settingsPath)
	}
	// The canonical form of the file AS FOUND, before the merge mutates the
	// map: the no-op detector below, and — via the manifest — the divergence
	// test that decides whether the merge's result may be recorded as the
	// mirror's own bytes. A semantic no-op (same injected hooks removed and
	// re-added) must not touch the file at all: on a RESUME the previous
	// segment's edits live in it, and rewriting + re-recording would
	// reclassify the run's own deliverable as mirror noise and destroy it at
	// finalize (#1571 on the resume paths, R213476). The marshal is
	// deterministic (sorted keys), so an agent's mere reformatting also
	// reads as no-op and survives uncanonicalized.
	before, _ := json.Marshal(settings)
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	// Remove our prior injection (deep-equal groups), preserving user hooks.
	for event, prevGroups := range prevBlob {
		existing, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		kept := existing[:0]
		for _, g := range existing {
			if !containsGroup(prevGroups, g) {
				kept = append(kept, g)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	// Add the current injection, skipping any group already present.
	for event, groups := range newBlob {
		existing, _ := hooks[event].([]any)
		for _, g := range groups {
			if !containsGroup(existing, g) {
				existing = append(existing, g)
			}
		}
		hooks[event] = existing
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}

	after, _ := json.Marshal(settings)

	if err := pass.root().MkdirAll(filepath.Dir(sidecarPath), 0o755); err != nil {
		return newBlob, complete, fmt.Errorf("runtime/plugin: mkdir managed dir: %w", err)
	}
	if !bytes.Equal(before, after) {
		// Decided BEFORE the write, from the file as found: may the result be
		// recorded as the mirror's own bytes?
		record := mirrorMayClaimSettings(workDir, settingsPath)
		if err := writeJSONFile(pass.root(), settingsPath, settings); err != nil {
			return newBlob, complete, fmt.Errorf("runtime/plugin: write settings.json: %w", err)
		}
		if record {
			if h, herr := hashFile(settingsPath); herr == nil {
				recordMirrorWrite(pass.root(), mirrorManifestPath(workDir), settingsPath, h, logger)
			}
		}
	}
	if !reflect.DeepEqual(prevBlob, newBlob) {
		if err := writeJSONFile(pass.root(), sidecarPath, newBlob); err != nil {
			return newBlob, complete, fmt.Errorf("runtime/plugin: write hooks sidecar: %w", err)
		}
	}
	return newBlob, complete, nil
}

// pluginHooksBlob is the merged `hooks` record of every enabled plugin, in
// memory: event -> []group, concatenated across plugins. It is computed from
// the plugin registry alone — never from the workspace — so it is the one
// value both lanes can trust: the trusted merge writes it into the
// workspace's settings.json, the untrusted lane hands it to the executor
// without touching the tree. A registry that fails to load returns the error
// — a failure is NOT an empty record, and the caller skips the pass rather
// than strip a prior injection. A plugin that loads but fails to produce its
// fragments is logged and skipped (ambient plugins must not brick a run);
// nil when nothing contributes.
func pluginHooksBlob(logger *iterlog.Logger) (map[string][]any, error) {
	reg, err := plugin.Load()
	if err != nil {
		return nil, err
	}
	blob := map[string][]any{}
	for _, p := range reg.Enabled() {
		frags, ferr := p.HookFragments()
		if ferr != nil {
			if logger != nil {
				logger.Warn("runtime: plugin %q hooks: %v — skipping", p.Name(), ferr)
			}
			continue
		}
		for _, frag := range frags {
			for event, v := range frag {
				if groups, ok := v.([]any); ok {
					blob[event] = append(blob[event], groups...)
				}
			}
		}
	}
	if len(blob) == 0 {
		return nil, nil
	}
	return blob, nil
}

// mirrorMayClaimSettings reports whether a settings.json the hooks merge is
// about to rewrite may be recorded in the mirror manifest as the mirror's
// own bytes. Yes when the mirror never recorded the file (its first claim —
// the pre-merge bytes are the repository's own, or nothing), or when the
// current bytes still hash to what the manifest recorded (untouched since
// the mirror last wrote). NO when the bytes diverged from the record: an
// agent (or the operator, during a pause) edited the file after the mirror
// laid it, and recording the merge result would reclassify that edit as the
// mirror's own — the finalize probe would then call the run's deliverable
// noise and the worktree removal would destroy it (#1571 on the resume
// paths, R213476). The edit is not lost to the merge: it travels inside the
// rewritten file, which — unrecorded — reads as the run's work and banks.
// This is the hooks writer's twin of the skill funnel's divergence respect
// (a shadow keeps its marker: the restore of exact mirror bytes reads as
// still-ours, a divergence never gets re-claimed).
func mirrorMayClaimSettings(workDir, settingsPath string) bool {
	m := loadMirrorManifest(workDir)
	if m == nil {
		return true
	}
	recorded, ok := m.Files[settingsPath]
	if !ok {
		return true
	}
	cur, err := hashFile(settingsPath)
	return err == nil && cur == recorded
}

func containsGroup(groups []any, g any) bool {
	for _, x := range groups {
		if reflect.DeepEqual(x, g) {
			return true
		}
	}
	return false
}

// readHooksSidecar reads iterion's own bookkeeping of the previously injected
// hooks blob, through the workspace handle: the sidecar lives under
// `.claude/.iterion-managed/`, and a checkout that committed it as a link gets
// the refusal, not the operator's bytes. A missing sidecar means nothing was
// ever injected (empty map, no error). A read or parse failure is reported
// alongside an empty map so the caller can warn and continue: the merge stays
// additive-correct (user hooks are never removed), but prior injected hook
// groups may be left stale.
func readHooksSidecar(wr *workspaceRoot, path string) (map[string][]any, error) {
	out := map[string][]any{}
	data, err := wr.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string][]any{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if out == nil { // a literal `null` unmarshals into a nil map without error
		out = map[string][]any{}
	}
	return out, nil
}

// readJSONObject reads path as a JSON object, through the workspace handle:
// a settings.json the checkout committed as a SYMLINK to the operator's own
// files reads as the refusal, not as settings — and the merge below then
// rewrites nothing through it. Only a MISSING file yields an empty map: a
// file that exists but cannot be read or parsed returns an error, so a
// caller about to rewrite the file never mistakes a corrupt existing
// settings file for an empty one (and destroys it on write-back).
func readJSONObject(wr *workspaceRoot, path string) (map[string]any, error) {
	data, err := wr.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if out == nil { // a literal `null` unmarshals into a nil map without error
		out = map[string]any{}
	}
	return out, nil
}

func writeJSONFile(wr *workspaceRoot, path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return wr.WriteFile(path, append(data, '\n'), 0o644)
}
