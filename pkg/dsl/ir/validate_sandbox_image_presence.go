package ir

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// SandboxImageGap is one node whose backend runs a CLI NO image iterion
// publishes carries — kimi, grok and opencode, whose `image-presence` cell is
// `unwired (no image)`, derived from the image manifests themselves (#1643).
//
// Deliberately the merged cell and not the per-family scope. A reader here
// does not know which image the run will use, and the backend a family splits
// on — codex, in the runner images and outside the stock sandbox's PATH — is
// already refused on its own terms wherever a sandbox is involved (the launch
// screen's codex arm, the delegate's dispatch guard): a second warning on
// every codex bot would be noise a compile guard rightly refuses. The split is
// read where the resolved image is known, by the engine's admission.
type SandboxImageGap struct {
	NodeID   string
	NodeKind NodeKind
	Backend  string
	// Binary is the executable the image would have to put on PATH — the
	// image-presence cell's subject, the adapter's own DefaultBinary.
	Binary string
}

// Reason is the one sentence every plane says about the gap: what is missing,
// what happens, and the two ways out. ONE wording for the compiler's C321 and
// for a launch's warning, so the author and the operator who launches are told
// the same thing in the same words.
func (g SandboxImageGap) Reason() string {
	return fmt.Sprintf(
		"backend %q runs the %q CLI, which no image iterion publishes puts on PATH — a sandboxed run of this node is refused at start, where the resolved image is known, so either run it on the host (sandbox: none on the workflow, --sandbox none, ITERION_SANDBOX_OVERRIDE=none) or start the sandbox from an image that ships %q (sandbox: { image: … } / build:, --sandbox-default-image, ITERION_SANDBOX_DEFAULT_IMAGE); the sandbox images iterion publishes serve %s",
		g.Backend, g.Binary, g.Binary,
		strings.Join(backendcaps.ServedByImage(backendcaps.ImageFamilySandbox), ", "))
}

// SandboxImageGaps reports the nodes of w whose PRIMARY route runs on a
// backend the published sandbox images provably carry no CLI for, sorted by
// node id — Workflow.Nodes is a map, so an unsorted walk would word the same
// run's warning differently on every launch. route resolves a node's backend for the caller's plane: the compiler
// reads the source, a launch reads its `model_overrides` first — the route a
// launch names is one the compiler never saw, which is why both planes ask
// here rather than each deriving the rule.
//
// Whether the run will be sandboxed at all is the CALLER's question: the
// compiler cannot know (it warns unless the workflow opted out), a launch
// resolves it (runtime.RunWillBeSandboxed), and the refusal itself waits for
// the resolved image. A route the plane cannot decide — "", `auto`, a
// reference — yields nothing: a warning on a guess trains operators to ignore
// warnings.
//
// `fallbacks:` routes are deliberately out: a route is a survival path the run
// may never take, and the launch screen already judges the ones it can
// (ApplyRunFallback).
func SandboxImageGaps(w *Workflow, route func(LLMNode) string) []SandboxImageGap {
	if w == nil || route == nil {
		return nil
	}
	var gaps []SandboxImageGap
	for _, node := range w.Nodes {
		n, ok := node.(LLMNode)
		if !ok {
			continue
		}
		backend := route(n)
		if backend == "" || !backendcaps.UnservedByPublishedImages(backend) {
			continue
		}
		gaps = append(gaps, SandboxImageGap{
			NodeID:   node.NodeID(),
			NodeKind: node.NodeKind(),
			Backend:  backend,
			Binary:   backendcaps.ImageCLI(backend),
		})
	}
	slices.SortFunc(gaps, func(a, b SandboxImageGap) int { return strings.Compare(a.NodeID, b.NodeID) })
	return gaps
}

// SourceSandboxRoute is the source reading of a node's primary route — the
// node's `backend:`, else the workflow's `default_backend:`, and "" for
// anything this reading cannot decide. It is what a plane with no launch
// overrides hands [SandboxImageGaps].
func SourceSandboxRoute(w *Workflow) func(LLMNode) string {
	return func(n LLMNode) string {
		return effectiveNodeBackend(n.GetLLMFields().Backend, w.DefaultBackend)
	}
}

// validateSandboxImagePresence warns (C321) about those nodes at compile time.
//
// A warning, never a refusal, and the reason is the whole shape of #1643: the
// limit is the IMAGE's contents, not the backend. The compiler knows neither
// the image a run will resolve (`sandbox: auto` reads the repository's
// devcontainer, an operator's default image, or nothing of the kind when the
// host has no driver) nor whether the run will be sandboxed at all. An
// operator whose image bakes `opencode` must not have their `.bot` refused for
// naming a backend THEIR image serves, so the refusal lives where the resolved
// image is known and names it (pkg/runtime's admitSandboxImageBackends), and
// the compiler states the coupling before launch.
//
// Only the WORKFLOW's opt-out silences it, like C136's: a node-level
// `sandbox:` is not honoured at run time, so it cannot take a node out of the
// sandbox.
func (c *compiler) validateSandboxImagePresence(w *Workflow) {
	if sandboxOptsOut(w.Sandbox) {
		return
	}
	for _, gap := range SandboxImageGaps(w, SourceSandboxRoute(w)) {
		c.warnfAt(DiagBackendAbsentFromSandboxImage, gap.NodeID, "",
			"%s %q: %s", gap.NodeKind, gap.NodeID, gap.Reason())
	}
}
