import { beforeEach, describe, expect, it } from "vitest";
import { makeEdgeId } from "./documentToGraph";
import {
  DETAIL_PREFIX_CENTRAL,
  DETAIL_PREFIX_EDGE,
  DETAIL_PREFIX_PROMPT,
  DETAIL_PREFIX_SCHEMA,
  DETAIL_PREFIX_VAR,
  generateNodeDetailGraph,
  parseDetailId,
} from "./nodeDetailGraph";
import type { IterDocument } from "@/api/types";
import { useSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";

beforeEach(() => {
  useSelectionStore.setState({
    selectedNodeId: null,
    selectedEdgeId: null,
    copiedNodeId: null,
  });
  useUIStore.setState({ editingItem: null, subNodeViewStack: [] });
});

describe("detail edge node click handling", () => {
  it("parses edge detail IDs so Canvas does not fall through to selecting the synthetic node", () => {
    expect(parseDetailId(`${DETAIL_PREFIX_EDGE}workflow:0`)).toEqual({
      kind: "edge",
      workflowName: "workflow",
      edgeIndex: 0,
    });
  });

  it("preserves DetailSubNode's edge selection when the ReactFlow node click follows", () => {
    const workflowName = "workflow";
    const edgeIndex = 0;
    const syntheticNodeId = `${DETAIL_PREFIX_EDGE}${workflowName}:${edgeIndex}`;

    useUIStore.setState({ editingItem: { kind: "prompt", name: "previous_prompt" }, subNodeViewStack: ["streak_check"] });

    // DetailSubNode.handleClick runs first: select the real document edge and exit detail view.
    useSelectionStore.getState().setSelectedEdge(makeEdgeId(workflowName, edgeIndex));
    useUIStore.getState().clearSubNodeView();

    // Then ReactFlow invokes Canvas.onNodeClick. Recognized detail IDs return early and must not
    // overwrite the real edge selection with the synthetic detail node id.
    const detail = parseDetailId(syntheticNodeId);
    if (!detail) useSelectionStore.getState().setSelectedNode(syntheticNodeId);

    expect(useSelectionStore.getState().selectedEdgeId).toBe(makeEdgeId(workflowName, edgeIndex));
    expect(useSelectionStore.getState().selectedNodeId).toBeNull();
    expect(useUIStore.getState().editingItem).toBeNull();
    expect(useUIStore.getState().subNodeViewStack).toEqual([]);
  });
});

// Minimal IterDocument scaffolding so generateNodeDetailGraph has the
// required shape. Only the arrays and fields the code reads are populated.
function makeDoc(partial: Partial<IterDocument>): IterDocument {
  return {
    prompts: [],
    schemas: [],
    agents: [],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [],
    comments: [],
    ...partial,
  };
}

describe("generateNodeDetailGraph for compute nodes", () => {
  it("emits input + output schema sub-nodes and var sub-nodes for vars referenced in compute expressions", () => {
    const doc = makeDoc({
      vars: {
        fields: [
          { name: "workspace_dir", type: "string", default: { kind: "string", raw: "\"./\"", str_val: "./" } },
        ],
      },
      schemas: [
        { name: "review_input", fields: [{ name: "approved", type: "bool" }] },
        { name: "streak_state", fields: [{ name: "stop", type: "bool" }] },
      ],
      computes: [
        {
          name: "streak_check",
          input: "review_input",
          output: "streak_state",
          expr: [
            { key: "stop", expr: "input.approved && vars.workspace_dir != ''" },
          ],
        },
      ],
      workflows: [
        {
          name: "wf",
          entry: "streak_check",
          edges: [
            { from: "streak_check", to: "done" },
          ],
        },
      ],
    });

    const { nodes, edges } = generateNodeDetailGraph(doc, "streak_check", "wf");
    const ids = nodes.map((n) => n.id);

    expect(ids).toContain(DETAIL_PREFIX_CENTRAL);
    expect(ids).toContain(`${DETAIL_PREFIX_SCHEMA}review_input:input`);
    expect(ids).toContain(`${DETAIL_PREFIX_SCHEMA}streak_state:output`);
    expect(ids).toContain(`${DETAIL_PREFIX_VAR}workspace_dir`);

    // Var attaches to the central compute node since compute has no prompt sub-node.
    const varEdge = edges.find((e) => e.source === `${DETAIL_PREFIX_VAR}workspace_dir`);
    expect(varEdge).toBeDefined();
    expect(varEdge!.target).toBe(DETAIL_PREFIX_CENTRAL);

    // Outgoing edge sub-node still rendered (this part already worked pre-fix).
    expect(ids).toContain(`${DETAIL_PREFIX_EDGE}wf:0`);
  });

  it("does not emit a spurious input schema sub-node when compute has only output", () => {
    const doc = makeDoc({
      schemas: [{ name: "out_schema", fields: [] }],
      computes: [
        { name: "c1", output: "out_schema", expr: [] },
      ],
      workflows: [{ name: "wf", entry: "c1", edges: [] }],
    });

    const { nodes } = generateNodeDetailGraph(doc, "c1", "wf");
    const ids = nodes.map((n) => n.id);

    expect(ids).toContain(`${DETAIL_PREFIX_SCHEMA}out_schema:output`);
    expect(ids.some((id) => id.startsWith(DETAIL_PREFIX_SCHEMA) && id.endsWith(":input"))).toBe(false);
    // No prompt sub-nodes for compute.
    expect(ids.some((id) => id.startsWith(DETAIL_PREFIX_PROMPT))).toBe(false);
  });

  it("does not emit a var sub-node when the compute expression references no vars", () => {
    const doc = makeDoc({
      vars: { fields: [{ name: "unused", type: "string" }] },
      schemas: [{ name: "out_schema", fields: [] }],
      computes: [
        { name: "c1", output: "out_schema", expr: [{ key: "x", expr: "input.foo + outputs.bar" }] },
      ],
      workflows: [{ name: "wf", entry: "c1", edges: [] }],
    });

    const { nodes } = generateNodeDetailGraph(doc, "c1", "wf");
    expect(nodes.some((n) => n.id.startsWith(DETAIL_PREFIX_VAR))).toBe(false);
  });
});

describe("generateNodeDetailGraph for tool nodes with input schemas", () => {
  it("emits a sub-node for tool.input (regression: previously only agent/human input rendered)", () => {
    const doc = makeDoc({
      schemas: [
        { name: "tool_input", fields: [{ name: "path", type: "string" }] },
        { name: "tool_output", fields: [{ name: "ok", type: "bool" }] },
      ],
      tools: [
        { name: "shell_cmd", command: "echo {{input.path}}", input: "tool_input", output: "tool_output" },
      ],
      workflows: [{ name: "wf", entry: "shell_cmd", edges: [] }],
    });

    const { nodes } = generateNodeDetailGraph(doc, "shell_cmd", "wf");
    const ids = nodes.map((n) => n.id);
    expect(ids).toContain(`${DETAIL_PREFIX_SCHEMA}tool_input:input`);
    expect(ids).toContain(`${DETAIL_PREFIX_SCHEMA}tool_output:output`);
  });
});
