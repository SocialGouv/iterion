package runtime

import (
	"fmt"
	"sort"
	"strings"

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
// `.url` reference serves from the store wherever the node runs.
func admitAttachmentPaths(node ir.Node, w *ir.Workflow, td *model.TemplateData) error {
	if node == nil || td == nil || len(td.Attachments) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var missing []string
	for _, ref := range nodeTemplateRefs(node, w) {
		if ref == nil || ref.Kind != ir.RefAttachments || len(ref.Path) == 0 {
			continue
		}
		if len(ref.Path) >= 2 && ref.Path[1] != "path" {
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
