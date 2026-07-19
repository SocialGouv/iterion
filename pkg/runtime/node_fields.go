package runtime

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// Node field accessors — thin wrappers over ir.Node* exported helpers.
// ---------------------------------------------------------------------------

var (
	nodePublish       = ir.NodePublish
	nodePublishLabels = ir.NodePublishLabels
	nodeAwaitMode     = ir.NodeAwaitMode
	nodeInteraction   = ir.NodeInteraction
	isTerminalNode    = ir.IsTerminalNode
)

// dedupeLabels returns the input with empty strings dropped and duplicates
// removed, preserving first-seen order. Used to union a node's DSL
// artifact_labels with shape-derived labels into the persisted artifact.
func dedupeLabels(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(labels))
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
