package runtime

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// Schema validation
// ---------------------------------------------------------------------------

// validateNodeOutput checks that the node's output conforms to its declared
// output schema. Returns nil if validation is disabled, the node has no
// output schema, or the output is valid.
func (e *Engine) validateNodeOutput(nodeID string, node ir.Node, output map[string]any) error {
	if !e.validateOutputs {
		return nil
	}
	schemaName := ir.NodeOutputSchema(node)
	if schemaName == "" {
		return nil
	}
	schema, ok := e.workflow.Schemas[schemaName]
	if !ok {
		return nil // schema not found; compile-time validation covers this
	}
	// ValidateOutput only checks declared schema fields — extra keys
	// (including _-prefixed metadata) are silently ignored.
	if err := model.ValidateOutput(output, schema); err != nil {
		return &RuntimeError{
			Code:    ErrCodeSchemaValidation,
			Message: fmt.Sprintf("node %q output does not match schema %q: %v", nodeID, schemaName, err),
			NodeID:  nodeID,
			Hint:    fmt.Sprintf("ensure node %q produces output conforming to schema %q", nodeID, schemaName),
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Output utilities
// ---------------------------------------------------------------------------

// extractUsage reads conventional _tokens and _cost_usd keys from a node
// output. Returns zeros if absent.
func extractUsage(output map[string]any) (tokens int, costUSD float64) {
	if v, ok := output["_tokens"]; ok {
		switch t := v.(type) {
		case int:
			tokens = t
		case float64:
			tokens = int(t)
		case int64:
			tokens = int(t)
		}
	}
	if v, ok := output["_cost_usd"]; ok {
		switch t := v.(type) {
		case float64:
			costUSD = t
		case int:
			costUSD = float64(t)
		}
	}
	return
}

// buildNodeFinishedData builds the data payload for a node_finished event,
// including usage metrics (_tokens, _cost_usd) and a snapshot of the output.
func buildNodeFinishedData(output map[string]any) map[string]any {
	if output == nil {
		return nil
	}
	data := map[string]any{
		"output": output,
	}
	if v, ok := output["_tokens"]; ok {
		data["_tokens"] = v
	}
	if v, ok := output["_cost_usd"]; ok {
		data["_cost_usd"] = v
	}
	return data
}

// SecretScrubber is optionally implemented by a NodeExecutor to scrub
// secret values from a node's output before the engine persists it to
// an OBSERVATIONAL sink (the node_finished event). It is Layer 0 of
// iterion's secrets protection.
//
// ScrubOutput MUST return a redacted deep copy and never mutate its
// input: the live output map feeds downstream nodes (`{{outputs.X}}` /
// `{{artifacts.X}}`) and the resume checkpoint, both of which must keep
// the real values. For that reason the engine applies scrubbing only on
// the event path here — NOT to persisted artifacts or the checkpoint,
// which are load-bearing for resume.
type SecretScrubber interface {
	ScrubOutput(map[string]any) map[string]any
}

// sanitizeOutputForEvent returns a copy of output scrubbed for the
// node_finished event stream. Two layers apply:
//
//   - Secret redaction (Layer 0): when the active executor implements
//     SecretScrubber, secret values are replaced with placeholders /
//     markers in a deep copy. The live output is untouched.
//   - The privacy_unfilter special-case: that tool's output carries the
//     restored text in the `text` field, which must not enter the event
//     stream (replaced with privacy.EventTextMarker).
//
// Returns the original map only when neither layer changes anything.
func (e *Engine) sanitizeOutputForEvent(node ir.Node, output map[string]any) map[string]any {
	if output == nil {
		return nil
	}
	out := output
	if scrubber, ok := e.executor.(SecretScrubber); ok {
		// ScrubOutput returns a redacted deep copy (never the original),
		// so the live output map is safe.
		out = scrubber.ScrubOutput(out)
	}
	if toolNode, ok := node.(*ir.ToolNode); ok && toolNode.Command == privacy.UnfilterToolName {
		if _, has := out["text"]; has {
			sanitized := make(map[string]any, len(out))
			for k, v := range out {
				sanitized[k] = v
			}
			sanitized["text"] = privacy.EventTextMarker
			out = sanitized
		}
	}
	return out
}
