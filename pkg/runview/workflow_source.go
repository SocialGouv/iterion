package runview

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

func captureWorkflowSource(u *unit.Unit, b *bundle.Bundle, prompts, includes map[string]string) (*workflowfile.Snapshot, error) {
	s := &workflowfile.Snapshot{Files: map[string]string{}, Bundle: b != nil}
	root, prefix := u.Root, ""
	if root == "" && len(u.Files) > 0 {
		root = filepath.Dir(u.Files[0].Name)
	}
	if b != nil {
		root = b.Dir
		if rel, err := filepath.Rel(root, u.Root); err == nil && filepath.IsLocal(rel) {
			prefix = rel
		}
	}
	for _, f := range u.Files {
		rel := f.Rel
		if !filepath.IsLocal(rel) {
			rel = filepath.Base(rel)
		}
		rel = filepath.ToSlash(filepath.Join(prefix, rel))
		s.Files[rel] = string(f.Source)
		if s.Main == "" {
			s.Main = rel
		}
	}
	for rel, body := range prompts {
		s.Files[rel] = body
	}
	for full, body := range includes {
		rel, err := filepath.Rel(root, full)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("capture included resource outside workflow root: %s", filepath.Base(full))
		}
		s.Files[filepath.ToSlash(rel)] = body
	}
	// Presets participate in the workflow hash even when the projection does
	// not use them. Retaining them keeps a captured revision self-contained.
	if b != nil && b.PresetsDir != "" {
		entries, err := os.ReadDir(b.PresetsDir)
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(strings.ToLower(ent.Name()), ".md") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(b.PresetsDir, ent.Name()))
			if err != nil {
				return nil, err
			}
			s.Files["presets/"+ent.Name()] = string(body)
		}
	}
	return s, nil
}

// WithSourceSnapshot carries the source alongside the AST inside its existing
// queue/offload envelope. ast.UnmarshalFile ignores the transport metadata.
func WithSourceSnapshot(body []byte, source *workflowfile.Snapshot) ([]byte, error) {
	if source == nil {
		return body, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	doc["source_snapshot"] = raw
	return json.Marshal(doc)
}

func SourceSnapshotFromTransport(body []byte) (*workflowfile.Snapshot, error) {
	var doc struct {
		Source *workflowfile.Snapshot `json:"source_snapshot"`
	}
	err := json.Unmarshal(body, &doc)
	return doc.Source, err
}

func compileSourceSnapshot(source *workflowfile.Snapshot) (*ir.Workflow, string, error) {
	u, b, cleanup, err := materializeSourceSnapshot(source)
	if err != nil {
		return nil, "", err
	}
	defer cleanup()
	// Projection resolves authoring resources but never rebinds execution to
	// this server's MCP configuration, credentials or relative executables —
	// no execution stage runs here, so no workspace either (#1800).
	wf, cs, err := compileLoadedUnitMode(u, u.Files[0].Name, source.Main, true, b, false, "")
	if err != nil {
		return nil, "", err
	}
	return wf, cs.Hash, nil
}

// MarshalSourceSnapshot makes the queue's executable AST from the SAME frozen
// files as its provenance. A second read of the launch path would execute an
// edit made between compile and publish while still claiming the old source.
func MarshalSourceSnapshot(source *workflowfile.Snapshot) ([]byte, error) {
	u, b, cleanup, err := materializeSourceSnapshot(source)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if err := MergeBundlePrompts(u.Merged, b); err != nil {
		return nil, err
	}
	if err := ir.InlinePromptIncludes(u.Merged); err != nil {
		return nil, err
	}
	body, err := ast.MarshalFile(u.Merged)
	if err != nil {
		return nil, err
	}
	return WithSourceSnapshot(body, source)
}

func materializeSourceSnapshot(source *workflowfile.Snapshot) (*unit.Unit, *bundle.Bundle, func(), error) {
	if source == nil || source.Main == "" || !filepath.IsLocal(source.Main) {
		return nil, nil, nil, fmt.Errorf("attempt has no captured workflow source")
	}
	if _, ok := source.Files[source.Main]; !ok {
		return nil, nil, nil, fmt.Errorf("captured workflow entry is missing")
	}
	dir, err := os.MkdirTemp("", "iterion-workflow-projection-*")
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) } // only scratch created above
	if err := botsource.Materialize(dir, source.Files); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	main := filepath.Join(dir, filepath.FromSlash(source.Main))
	u, err := unit.LoadDir(main)
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	if u.HasErrors() || len(u.Files) == 0 || u.Merged == nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("captured source does not form a workflow unit: %v", u.Diagnostics)
	}
	var b *bundle.Bundle
	if source.Bundle {
		b = &bundle.Bundle{Dir: dir, IterPath: main}
		if _, err := os.Stat(filepath.Join(dir, "prompts")); err == nil {
			b.PromptsDir = filepath.Join(dir, "prompts")
		}
		if _, err := os.Stat(filepath.Join(dir, "presets")); err == nil {
			b.PresetsDir = filepath.Join(dir, "presets")
		}
	}
	return u, b, cleanup, nil
}
