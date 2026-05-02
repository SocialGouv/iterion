export interface DiagnosticHint {
  /** Short title (one line). */
  title: string;
  /** What this code means and how to fix it. */
  hint: string;
  /** Anchor in docs/diagnostics.md (currently a placeholder reference). */
  docsAnchor?: string;
}

const HINTS: Record<string, DiagnosticHint> = {
  C001: {
    title: "Edge references unknown node",
    hint: "Both endpoints of an edge must point to a declared node. Check spelling, or declare the missing node.",
    docsAnchor: "c001",
  },
  C002: {
    title: "Unknown schema reference",
    hint: "The schema is not declared in this file. Add a `schema <name>` block or reference an existing one.",
    docsAnchor: "c002",
  },
  C003: {
    title: "Unknown prompt reference",
    hint: "The prompt is not declared in this file. Add a `prompt <name>` block or reference an existing one.",
    docsAnchor: "c003",
  },
  C004: {
    title: "Malformed template reference",
    hint: "Template expressions must be `{{vars.X}}`, `{{input.X}}`, `{{outputs.node[.field]}}`, or `{{artifacts.X}}`.",
    docsAnchor: "c004",
  },
  C005: {
    title: "Conflicting loop definitions",
    hint: "A loop name is defined with different max_iterations on multiple edges. Make them consistent.",
    docsAnchor: "c005",
  },
  C006: {
    title: "No workflow declaration",
    hint: "Add at least one `workflow <name>:` block describing the entry point and edges.",
    docsAnchor: "c006",
  },
  C007: {
    title: "Multiple workflows not supported",
    hint: "V1 supports a single workflow per file. Split into separate files.",
    docsAnchor: "c007",
  },
  C008: {
    title: "Entry node not found",
    hint: "The `entry:` value must reference a declared node.",
    docsAnchor: "c008",
  },
  C009: {
    title: "Session inherit/fork on convergence point",
    hint: "Sessions can't inherit/fork at a node that joins multiple branches (`await != none`).",
    docsAnchor: "c009",
  },
  C010: {
    title: "Multiple unconditional edges",
    hint: "Only one unconditional edge is allowed from a non-fan_out source. Add a `when` condition or use a router.",
    docsAnchor: "c010",
  },
  C011: {
    title: "Ambiguous conditions",
    hint: "Two edges share the same field and polarity. Use distinct conditions or a router.",
    docsAnchor: "c011",
  },
  C012: {
    title: "Conditional edges without fallback",
    hint: "Add a default unconditional edge so the workflow has a path when no condition matches.",
    docsAnchor: "c012",
  },
  C013: {
    title: "Condition field is not boolean",
    hint: "`when <field>` requires a bool field in the source node's output schema.",
    docsAnchor: "c013",
  },
  C014: {
    title: "Condition field not found",
    hint: "Pick a bool field from the source node's output schema, or change the schema.",
    docsAnchor: "c014",
  },
  C016: {
    title: "Node unreachable from entry",
    hint: "No path leads from the entry to this node. Connect it or remove it.",
    docsAnchor: "c016",
  },
  C017: {
    title: "outputs.node.history but node not in a loop",
    hint: "`history` references require the node to participate in a declared loop.",
    docsAnchor: "c017",
  },
  C018: {
    title: "Missing model or backend",
    hint: "Set `model` or `backend` on this node, or define `ITERION_DEFAULT_SUPERVISOR_MODEL`.",
    docsAnchor: "c018",
  },
  C019: {
    title: "Cycle without declared loop",
    hint: "Add `as <loop>(N)` to one edge in the cycle so the runtime can bound iterations.",
    docsAnchor: "c019",
  },
  C020: {
    title: "round_robin router needs ≥ 2 edges",
    hint: "A `round_robin` router must have at least two outgoing edges.",
    docsAnchor: "c020",
  },
  C021: {
    title: "llm router needs ≥ 2 edges",
    hint: "An `llm` router must have at least two outgoing edges.",
    docsAnchor: "c021",
  },
  C022: {
    title: "llm router edge has 'when' condition",
    hint: "Edges from an `llm` router can't carry `when` clauses; the LLM picks the branch.",
    docsAnchor: "c022",
  },
  C023: {
    title: "LLM-only property on non-llm router",
    hint: "Properties like `model`, `backend`, `system`, `user`, `multi`, `reasoning_effort` apply only when `mode: llm`.",
    docsAnchor: "c023",
  },
  C024: {
    title: "Invalid reasoning_effort or duplicate MCP server",
    hint: "Use `low | medium | high | xhigh | max` for reasoning_effort, and unique mcp_server names.",
    docsAnchor: "c024",
  },
  C025: {
    title: "Invalid MCP server config",
    hint: "Check transport and required fields (`command` for stdio; `url` for http).",
    docsAnchor: "c025",
  },
  C026: {
    title: "Loop max_iterations < 1",
    hint: "Loop iteration cap must be at least 1.",
    docsAnchor: "c026",
  },
  C028: {
    title: "Duplicate with-mapping key",
    hint: "Multiple edges to the same target define the same `with` key. Resolve the conflict.",
    docsAnchor: "c028",
  },
  C030: {
    title: "Codex backend discouraged",
    hint: "Prefer `claude_code` for tool-using agents or `claw` with an OpenAI model for judges. (C030 is also emitted when `outputs.<node>` points at an unknown node — fix the reference if that's what you're seeing.)",
    docsAnchor: "c030",
  },
  C031: {
    title: "outputs ref field not in output schema",
    hint: "Reference a field that exists on the source node's `output` schema.",
    docsAnchor: "c031",
  },
  C032: {
    title: "outputs ref on schemaless node",
    hint: "The referenced node has no output schema. Add an `output:` to it.",
    docsAnchor: "c032",
  },
  C033: {
    title: "Undeclared variable",
    hint: "Add the variable to the file-level `vars:` block or to the workflow's `vars:` block.",
    docsAnchor: "c033",
  },
  C034: {
    title: "input ref field not in input schema",
    hint: "Reference a field declared on the consuming node's `input` schema.",
    docsAnchor: "c034",
  },
  C035: {
    title: "Unknown artifact",
    hint: "Add a `publish: <name>` on a prior node so the artifact is produced.",
    docsAnchor: "c035",
  },
  C036: {
    title: "Reference to non-reachable node",
    hint: "The referenced node is not reachable before this consumer. Reorder or wire the graph.",
    docsAnchor: "c036",
  },
  C037: {
    title: "Node max_tokens exceeds workflow budget",
    hint: "Lower the node's `max_tokens` or raise `budget.max_tokens`.",
    docsAnchor: "c037",
  },
  C038: {
    title: "Unsupported MCP auth type",
    hint: "Only `oauth2` is currently wired. Drop the `auth:` block or switch to a supported type.",
    docsAnchor: "c038",
  },
  C039: {
    title: "Compute node has no expressions",
    hint: "A `compute` node needs at least one `expr: key: \"<expression>\"` entry. Add one or remove the node.",
    docsAnchor: "c039",
  },
  C040: {
    title: "Expression failed to parse",
    hint: "The expression is not valid. Use the supported namespaces (vars/input/outputs/artifacts/loop/run), comparison/boolean operators, and the built-ins (length, concat, unique, contains).",
    docsAnchor: "c040",
  },
  C041: {
    title: "Duplicate node id",
    hint: "Two declarations share the same node name. Rename one — every node must be uniquely identified across agents/judges/routers/humans/tools/computes.",
    docsAnchor: "c041",
  },
  C042: {
    title: "Reserved node name",
    hint: "`done` and `fail` are reserved terminal targets — do not use them as user node names. Pick a different name.",
    docsAnchor: "c042",
  },
  C043: {
    title: "Invalid compaction values",
    hint: "`threshold` must be in (0, 1]; `preserve_recent` must be ≥ 1. Omit either field to inherit defaults.",
    docsAnchor: "c043",
  },
};

export function getHint(code: string): DiagnosticHint {
  return (
    HINTS[code.toUpperCase()] ?? {
      title: `Diagnostic ${code}`,
      hint: "No hint available for this code.",
    }
  );
}
