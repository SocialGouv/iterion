import { describe, expect, it } from "vitest";
import type { ContractDecl, WorkflowDecl } from "@/api/types";
import { effectiveNativePortTypes } from "./nativePorts";

describe("effectiveNativePortTypes", () => {
  const contracts: ContractDecl[] = [
    { name: "Root", display_name: "Root", responsibility: "Provide items", inputs: [{ name: "items", type: "string[]" }] },
    { name: "Render", display_name: "Render", responsibility: "Render one item", inputs: [{ name: "item", type: "string" }], outputs: [{ name: "text", type: "string" }] },
    { name: "Collect", display_name: "Collect", responsibility: "Collect all items", inputs: [{ name: "texts", type: "string[]" }], outputs: [{ name: "result", type: "string[]" }] },
  ];
  const workflow: WorkflowDecl = {
    name: "main", entry: "", edges: [], contract: "Root", runtime_semantics: "ports-v1",
    graph: {
      nodes: [
        { name: "render", implementation: "render_impl", contract: "Render" },
        { name: "collect", implementation: "collect_impl", contract: "Collect" },
      ],
      bindings: [{ from: "input.items", to: "render.item" }, { from: "render.text", to: "collect.texts" }],
    },
  };

  it("lifts a mapped output so a downstream whole-array port can receive it", () => {
    const types = effectiveNativePortTypes(workflow, contracts);
    expect(types.get("input.items")).toBe("string[]");
    expect(types.get("render.text")).toBe("string[]");
    expect(types.get("collect.result")).toBe("string[]");
  });

  it("propagates one more mapping through a later scalar consumer", () => {
    const chained: WorkflowDecl = { ...workflow, graph: { ...workflow.graph,
      nodes: [...(workflow.graph?.nodes ?? []), { name: "second", implementation: "render_impl", contract: "Render" }],
      bindings: [...(workflow.graph?.bindings ?? []), { from: "render.text", to: "second.item" }],
    } };
    expect(effectiveNativePortTypes(chained, contracts).get("second.text")).toBe("string[]");
  });
});
