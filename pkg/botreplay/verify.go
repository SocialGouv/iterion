package botreplay

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// VerifySchema validates the recorded output against the node's declared
// output schema (required fields, type compatibility, enum membership)
// using the same model.ValidateOutput the runtime applies in production.
// Nodes that declare no output schema pass trivially.
func VerifySchema(f *Fixture, wf *ir.Workflow) error {
	node, ok := wf.Nodes[f.Node]
	if !ok {
		return fmt.Errorf("node %q not found in bot %q", f.Node, f.Bot)
	}
	name := ir.NodeOutputSchema(node)
	if name == "" {
		return nil
	}
	schema, ok := wf.Schemas[name]
	if !ok {
		return fmt.Errorf("output schema %q for node %q not found", name, f.Node)
	}
	if err := model.ValidateOutput(f.Output, schema); err != nil {
		return fmt.Errorf("schema %q: %w", name, err)
	}
	return nil
}

// VerifyRequiredNonEmpty asserts each named field is present in the
// output AND carries a non-empty value. This complements VerifySchema:
// a `json`-typed schema field (e.g. created_issues) accepts any non-nil
// value, so an empty array passes schema validation but is semantically
// empty — the emit_action golden must actually have created issues.
func VerifyRequiredNonEmpty(f *Fixture, fields []string) error {
	for _, name := range fields {
		val, ok := f.Output[name]
		if !ok {
			return fmt.Errorf("required field %q absent from output", name)
		}
		if isEmptyValue(val) {
			return fmt.Errorf("required field %q is present but empty", name)
		}
	}
	return nil
}

func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// assigneeKeys are the JSON object keys whose string values name a bot
// expected to run an issue. Extend this set when a new schema introduces
// a differently-named bot field.
var assigneeKeys = map[string]bool{
	"assignee": true,
	"bot":      true,
}

// collectAssignees recursively walks any decoded JSON value and returns
// every string found under an assignee/bot key. This finds both
// emit_output.created_issues[].assignee and the nested
// roadmap_item.assignee arrays without hardcoding either path.
func collectAssignees(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if assigneeKeys[k] {
				if s, ok := child.(string); ok {
					out = append(out, s)
				}
			}
			out = append(out, collectAssignees(child)...)
		}
	case []any:
		for _, item := range t {
			out = append(out, collectAssignees(item)...)
		}
	}
	return out
}

// ValidBots returns the set of NormalizeName'd bot names discovered under
// the conventional roots below root (bots/, examples/, .botz/) — the
// universe of legitimate assignees a bot may emit.
func ValidBots(root string) (map[string]bool, error) {
	entries, err := botregistry.List(botregistry.ListOptions{
		Paths: botregistry.DefaultPaths(root),
	})
	if err != nil {
		return nil, err
	}
	valid := make(map[string]bool, len(entries))
	for _, e := range entries {
		valid[botregistry.NormalizeName(e.Name)] = true
	}
	return valid, nil
}

// VerifyNoHallucinatedAssignees checks that every non-empty assignee in
// the recorded output resolves (tolerant of kebab/snake/case via
// NormalizeName) to a bot that actually exists. An empty assignee is
// allowed — the bot contract drops an unrecognized assignee to "" plus a
// needs-manual-triage label rather than inventing a name.
func VerifyNoHallucinatedAssignees(f *Fixture, valid map[string]bool) error {
	for _, a := range collectAssignees(f.Output) {
		if strings.TrimSpace(a) == "" {
			continue
		}
		if !valid[botregistry.NormalizeName(a)] {
			return fmt.Errorf("hallucinated assignee %q: not a known bot", a)
		}
	}
	return nil
}
