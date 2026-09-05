package tools

import "github.com/SocialGouv/claw-code-go/internal/api"

// WorkflowTool returns the tool definition for the JavaScript orchestration
// engine (executed by the conversation loop's workflow runtime).
func WorkflowTool() api.Tool {
	return api.Tool{
		Name: "workflow",
		Description: "Execute a JavaScript orchestration script that fans work out across sub-agents " +
			"deterministically — use it when the STRUCTURE of the fan-out matters (parallel coverage, " +
			"verify-then-synthesize, migrations over a work list), not for a single delegation (use agent). " +
			"It can spawn many sub-agents; reach for it deliberately.\n\n" +
			"Script API: agent(prompt, {label?, subagent_type?, model?, schema?}) → Promise of the sub-agent's transcript, " +
			"or — when schema (a JSON Schema object) is given — of the object it returned through structured_output, " +
			"validated for required fields and property types (no structured result, or a mismatch, rejects the promise) " +
			"(sub-agents are stateless; the prompt must carry all context). " +
			"parallel(thunks) awaits an array of () => Promise, mapping a thrown thunk to null. " +
			"pipeline(items, ...stages) runs each item through the stages independently with NO barrier between " +
			"stages; a stage that throws drops that item to null and skips its remaining stages. " +
			"phase(title) and log(msg) record progress; `args` carries the value you pass in. " +
			"Use `return` for the final value (top-level await is available). DEFAULT TO pipeline(); " +
			"synchronize with parallel() only when a step genuinely needs every prior result at once. " +
			"Filter nulls before using results. Caps: 200 agents per run, 8 concurrent, 15min default timeout.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"script":          {Type: "string", Description: "The JavaScript orchestration script (plain JS, async context; `export const meta = {...}` openers are tolerated)."},
				"args":            {Type: "object", Description: "Optional value exposed to the script as the global `args`."},
				"timeout_seconds": {Type: "integer", Description: "Optional overall timeout in seconds (default 900, max 3600)."},
			},
			Required: []string{"script"},
		},
	}
}
