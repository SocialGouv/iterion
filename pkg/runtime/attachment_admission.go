package runtime

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// admitAttachmentPaths refuses to run a node that names an attachment's
// PATH when the run holds the attachment but no path a node can open exists
// — a store with no filesystem root whose attachments were not materialised
// for this run. Rendered, the reference would be an empty string: a tool
// would `cat ""` and an agent would be told a file is at "", and both would
// improvise. The refusal names the attachments and the remedy instead.
//
// An attachment the run does NOT hold is not this guard's business: a
// declared optional attachment nobody uploaded renders empty, as it always
// has, and a bot may test for that. Only `.path` references (and the bare
// `{{attachments.<name>}}` form, which is the path) are admitted here; a
// `.url` reference serves from the store wherever the node runs — and so
// does an image the claw executor sends as an image block (imageBlockRefs).
func admitAttachmentPaths(node ir.Node, w *ir.Workflow, td *model.TemplateData, executor any) error {
	if node == nil || td == nil || len(td.Attachments) == 0 {
		return nil
	}
	imageBlocks := imageBlockRefs(node, w, executor)
	seen := map[string]bool{}
	var missing []string
	for _, ref := range nodeTemplateRefs(node, w) {
		if ref == nil || ref.Kind != ir.RefAttachments || len(ref.Path) == 0 {
			continue
		}
		if len(ref.Path) >= 2 && ref.Path[1] != "path" {
			continue
		}
		if imageBlocks[ref] {
			continue
		}
		name := ref.Path[0]
		info, held := td.Attachments[name]
		if !held || info.Path != "" || seen[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &RuntimeError{
		Code:    ErrCodeAttachmentPathUnavailable,
		NodeID:  node.NodeID(),
		Message: fmt.Sprintf("attachment %s: the run holds it, but no path a node can open exists here — the run store has no filesystem root and no copy of the attachments reached this node", strings.Join(missing, ", ")),
		Hint:    "reference {{attachments.<name>.url}} where a presigned URL serves (an in-process reader, an HTTP fetch), or run the bot where its attachments are delivered as files (a filesystem store, or a sandbox driver that binds or copies them)",
	}
}

// nodeTemplateRefs is every template reference a node renders when it runs:
// a tool's command, script, postcondition and action params, and the prompts
// an LLM, router or human node names. An edge mapping is the edge's own,
// resolved when it fires, and the run's refs (a fail message, an emit
// payload) are not a node's rendering.
func nodeTemplateRefs(node ir.Node, w *ir.Workflow) []*ir.Ref {
	var refs []*ir.Ref
	if tn, ok := node.(*ir.ToolNode); ok {
		refs = append(refs, tn.CommandRefs...)
		refs = append(refs, tn.ScriptRefs...)
		refs = append(refs, tn.PostcondRefs...)
		for _, p := range tn.Params {
			refs = append(refs, p.Refs...)
		}
	}
	if w != nil {
		for _, name := range ir.NodePromptRefs(node) {
			if p := w.Prompts[name]; p != nil {
				refs = append(refs, p.TemplateRefs...)
			}
		}
	}
	return refs
}

// imageBlockRefs are the references the claw executor turns into an image
// block instead of interpolating a path (buildUserPromptParts →
// buildUserContent): the image attachments of an LLM node's USER prompt,
// when every route that may serve the node is claw — the primary backend
// and each fallback (a fallback names its own backend, or "" for the
// node's). Everything else interpolates the path as text: a system or an
// interaction prompt, a router, a CLI backend, and a user prompt that is
// also the node's system or interaction prompt. The routes are the
// executor's own reading (EffectiveBackendName, EffectiveFallbackResolver)
// and the reference test is the executor's rule on the reference's text
// (model.IsImageAttachmentRef) — a copy of either would drift from it.
func imageBlockRefs(node ir.Node, w *ir.Workflow, executor any) map[*ir.Ref]bool {
	llm, ok := node.(ir.LLMNode)
	if !ok || w == nil {
		return nil
	}
	f := llm.GetLLMFields()
	if f.UserPrompt == "" || f.UserPrompt == f.SystemPrompt || f.UserPrompt == llm.GetInteractionFields().InteractionPrompt {
		return nil
	}
	if promptBackend(node, executor) != delegate.BackendClaw {
		return nil
	}
	if fr, ok := executor.(EffectiveFallbackResolver); ok {
		// The fallback routes the executor will walk, beyond the node's own
		// (a route spelled through vars is a backend NAME here, and a
		// route a vars gate turns off is absent).
		for _, b := range fr.EffectiveFallbackBackends(node) {
			if b != delegate.BackendClaw {
				return nil
			}
		}
	} else {
		for _, fb := range llm.GetFallbacks() {
			if fb.Backend != "" && fb.Backend != delegate.BackendClaw {
				return nil
			}
		}
	}
	p := w.Prompts[f.UserPrompt]
	if p == nil {
		return nil
	}
	names := model.ImageAttachmentNames(w)
	out := map[*ir.Ref]bool{}
	for _, ref := range p.TemplateRefs {
		if ref == nil || len(ref.Raw) < len("{{}}") {
			continue
		}
		if model.IsImageAttachmentRef(strings.TrimSpace(ref.Raw[2:len(ref.Raw)-2]), names) {
			out[ref] = true
		}
	}
	return out
}

// promptBackend is the backend the executor will serve an LLM node with —
// the IR's own when the executor answers nothing.
func promptBackend(node ir.Node, executor any) string {
	if namer, ok := executor.(interface{ EffectiveBackendName(ir.Node) string }); ok {
		if b := namer.EffectiveBackendName(node); b != "" {
			return b
		}
	}
	if llm, ok := node.(ir.LLMNode); ok {
		return llm.GetLLMFields().Backend
	}
	return ""
}
