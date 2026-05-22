// Package runview — agent-driven merge-conflict resolution.
//
// The HTTP layer's `POST /merge/conflicts/resolve-with-agent` calls
// ResolveAllConflictsWithAgent below; it short-circuits to
// ErrAgentResolverNotWired when no LLM credential is reachable, and
// otherwise builds a claw direct-LLM call against the resolver prompt
// + schema, applies the returned resolutions via StageResolvedFile,
// and returns the refreshed conflict snapshot. The implementation
// lives in this file rather than service_control.go because the
// imports (model, detect) are heavy and would otherwise burden every
// service-control reader with the LLM dependency surface.
package runview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resolverSchema is the structured-output schema the agent is forced
// to populate via GenerateObjectDirect's synthetic tool. The model
// returns one entry per file in the conflict set; we tolerate it
// returning fewer (partial resolution) and silently skip entries
// whose path isn't in the active conflict set.
const resolverSchema = `{
  "type": "object",
  "required": ["files"],
  "properties": {
    "files": {
      "type": "array",
      "description": "One entry per conflicted file. The content field must hold the full, resolved file content with NO conflict markers remaining (no <<<<<<< / ======= / >>>>>>> lines).",
      "items": {
        "type": "object",
        "required": ["path", "content"],
        "properties": {
          "path":    {"type": "string"},
          "content": {"type": "string"}
        }
      }
    }
  }
}`

// resolverSystemPrompt is the resolver's calibration. The framing is
// deliberately conservative: "preserve the ours side unless the
// incoming change clearly supersedes it" — operators routinely care
// more about not losing in-flight work than picking the "best" merge.
const resolverSystemPrompt = "You are a git merge-conflict resolver. You receive the FULL content of one or more files that git left in a conflicted state after a squash merge.\n\nFor each file:\n1. Remove every <<<<<<<, =======, |||||||, and >>>>>>> marker.\n2. Produce a resolved file content that preserves the operator's intent:\n   - Default: keep the OURS side (the operator's main branch) and incorporate the INCOMING side's logic only when it clearly supersedes ours (bug fix, completed feature, deletion of dead code).\n   - When both sides edited the same line for different reasons, prefer the more recent / more specific change; if you can't decide, lean toward OURS.\n   - Preserve formatting, import order, and surrounding context from the OURS side.\n3. Return the FULL file content, not just the modified region. The content must compile / parse the same way the surrounding (non-conflicted) regions imply.\n\nReturn ONLY the structured tool call; do not add prose commentary."

// resolverFile mirrors the schema for a single returned file.
type resolverFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// resolverResponse mirrors the structured-output schema above.
type resolverResponse struct {
	Files []resolverFile `json:"files"`
}

// resolveAllConflictsWithAgent is the implementation behind the
// stub in service_control.go. Returns ErrAgentResolverNotWired when
// no provider credential is reachable so the UI surfaces a useful
// hint instead of a generic 5xx.
//
// Wired by overriding the stub at construction-time (see init()
// below) so the heavy imports don't pollute service_control.go's
// dependency surface.
func (s *Service) resolveAllConflictsWithAgent(ctx context.Context, runID, modelSpec string) (*MergeConflictsResponse, error) {
	if runID == "" {
		return nil, errors.New("runview: run_id is required")
	}
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if r.MergeStatus != store.MergeStatusConflicted {
		return nil, fmt.Errorf("run %q has no pending conflict (merge_status=%q)", runID, r.MergeStatus)
	}
	repoRoot := mergeRepoRoot(r)
	if repoRoot == "" {
		return nil, fmt.Errorf("run %q has no resolvable repo root", runID)
	}
	det, err := runtime.ParseConflicts(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("parse conflicts: %w", err)
	}
	if len(det.Files) == 0 {
		// Nothing to resolve — return the empty snapshot so the UI
		// reflects the (already-clean) state.
		return &MergeConflictsResponse{
			Files:            det.Files,
			PendingMessage:   r.PendingMergeMessage,
			PendingMergeInto: r.PendingMergeInto,
		}, nil
	}

	registry := model.NewRegistry()
	spec, err := resolveResolverModel(registry, modelSpec)
	if err != nil {
		return nil, err
	}
	client, err := registry.Resolve(spec)
	if err != nil {
		return nil, fmt.Errorf("resolve model %q: %w", spec, err)
	}

	prompt, err := buildResolverPrompt(det.Files)
	if err != nil {
		return nil, fmt.Errorf("build prompt: %w", err)
	}

	opts := model.GenerationOptions{
		Model:          providerlessModel(spec),
		System:         resolverSystemPrompt,
		Messages:       []api.Message{userMessage(prompt)},
		ExplicitSchema: json.RawMessage(resolverSchema),
		SchemaName:     "merge_conflict_resolution",
		MaxTokens:      32_000, // resolver can emit large files verbatim
	}
	res, err := model.GenerateObjectDirect[resolverResponse](ctx, client, opts)
	if err != nil {
		return nil, fmt.Errorf("agent call: %w", err)
	}

	// Validate + stage. Tolerate the model returning files not in the
	// conflict set (skip them silently) but require at least one
	// staged file — otherwise the operator clicked the button and
	// nothing happened, which is worse than a clear error.
	conflictPaths := make([]string, len(det.Files))
	for i, f := range det.Files {
		conflictPaths[i] = f.Path
	}
	staged := 0
	for _, f := range res.Object.Files {
		if !slices.Contains(conflictPaths, f.Path) {
			continue
		}
		if runtime.HasConflictMarkers(f.Content) {
			// Agent missed a marker — refuse rather than stage a
			// file that still won't compile.
			return nil, fmt.Errorf("agent left conflict markers in %q; resolve manually", f.Path)
		}
		if err := runtime.StageResolvedFile(repoRoot, f.Path, f.Content); err != nil {
			return nil, fmt.Errorf("stage %s: %w", f.Path, err)
		}
		staged++
	}
	if staged == 0 {
		return nil, fmt.Errorf("agent returned no usable resolutions (responded with %d files, none matched the conflict set)", len(res.Object.Files))
	}

	// Refresh + return.
	next, err := runtime.ParseConflicts(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("re-parse conflicts: %w", err)
	}
	return &MergeConflictsResponse{
		Files:            next.Files,
		PendingMessage:   r.PendingMergeMessage,
		PendingMergeInto: r.PendingMergeInto,
	}, nil
}

func init() {
	resolveAllConflictsWithAgentImpl = func(ctx context.Context, s *Service, runID, modelSpec string) (*MergeConflictsResponse, error) {
		return s.resolveAllConflictsWithAgent(ctx, runID, modelSpec)
	}
}

// resolveResolverModel picks the model spec to use for the agent
// call. Caller's pin wins; otherwise we fall back to the detector's
// suggested claw model. Returns ErrAgentResolverNotWired when no
// model is available so the UI surfaces a friendly message.
func resolveResolverModel(registry *model.Registry, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	// Honour an explicit operator override via env — useful in
	// devloops where the detector's pick isn't what we want for
	// resolution.
	if env := os.Getenv("ITERION_CONFLICT_RESOLVER_MODEL"); env != "" {
		return env, nil
	}
	report := detect.Detect(context.Background())
	spec := detect.SuggestedModel(detect.BackendClaw, report.Providers)
	if spec == "" {
		return "", ErrAgentResolverNotWired
	}
	return spec, nil
}

// providerlessModel strips the leading "<provider>/" off a claw model
// spec when present. Some claw provider factories expect just the
// model ID at GenerationOptions.Model, while Registry.Resolve
// expects the full spec — so we keep both around.
func providerlessModel(spec string) string {
	if i := strings.Index(spec, "/"); i >= 0 && i+1 < len(spec) {
		return spec[i+1:]
	}
	return spec
}

// buildResolverPrompt assembles the user-message body for the
// resolver. Each conflicted file is rendered as a `<file path="..."
// hunks="N">` block followed by the full content (markers included).
// The model has been told (system prompt) to emit fully-resolved
// content per file.
func buildResolverPrompt(files []runtime.ConflictFile) (string, error) {
	var b strings.Builder
	b.WriteString("Resolve the following ")
	if len(files) == 1 {
		b.WriteString("conflicted file.\n\n")
	} else {
		fmt.Fprintf(&b, "%d conflicted files.\n\n", len(files))
	}
	for i, f := range files {
		fmt.Fprintf(&b, "--- file %d/%d: %s (%d hunk%s) ---\n",
			i+1, len(files), f.Path, len(f.Hunks), pluralS(len(f.Hunks)))
		b.WriteString(f.Content)
		if !strings.HasSuffix(f.Content, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// userMessage wraps a text body in an api.Message with the user role.
// Centralised so future agentic features (tool use, system blocks)
// have one place to adjust.
func userMessage(text string) api.Message {
	return api.Message{
		Role:    "user",
		Content: []api.ContentBlock{{Type: "text", Text: text}},
	}
}
