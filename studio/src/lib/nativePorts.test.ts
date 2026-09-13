import { describe, expect, it } from "vitest";
import type { ContractDecl, WorkflowDecl } from "@/api/types";
import { effectiveNativePortTypes, nativeBindingCanBeAdded, nativePortCanSupply, nativePortEndpoints } from "./nativePorts";

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

  it("keeps mapped outputs available for a complete-array consumer and public export", () => {
    const partial = { ...workflow, graph: { ...workflow.graph, bindings: workflow.graph?.bindings?.slice(0, 1) } };
    expect(nativeBindingCanBeAdded(partial, contracts, "render.text", "collect.texts")).toBe(true);
    const rendered = nativePortEndpoints(partial, contracts).sources.find(port => port.name === "render.text");
    if (!rendered) throw new Error("missing mapped output");
    expect(nativePortCanSupply(rendered, { name: "result", type: "string[]" }, false)).toBe(true);
    expect(nativePortCanSupply(rendered, { name: "result", type: "string" }, false)).toBe(false);
  });

  it("does not propose a second supplier, self-edge or data cycle", () => {
    expect(nativeBindingCanBeAdded(workflow, contracts, "input.items", "collect.texts")).toBe(false);
    const partial = { ...workflow, graph: { ...workflow.graph, bindings: workflow.graph?.bindings?.slice(1) } };
    expect(nativeBindingCanBeAdded(partial, contracts, "render.text", "render.item")).toBe(false);
    expect(nativeBindingCanBeAdded(partial, contracts, "collect.result", "render.item")).toBe(false);
  });

  it("broadcasts scalars and reuses one axis but never suggests a second array axis", () => {
    const multi = contracts.map(contract => contract.name === "Root" ? { ...contract,
      inputs: [...(contract.inputs ?? []), { name: "other", type: "string[]" }, { name: "prefix", type: "string" }],
    } : contract.name === "Render" ? { ...contract,
      inputs: [...(contract.inputs ?? []), { name: "prefix", type: "string" }],
    } : contract);
    expect(nativeBindingCanBeAdded(workflow, multi, "input.prefix", "render.prefix")).toBe(true);
    expect(nativeBindingCanBeAdded(workflow, multi, "input.items", "render.prefix")).toBe(true);
    expect(nativeBindingCanBeAdded(workflow, multi, "input.other", "render.prefix")).toBe(false);
  });

  it("checks requiredness and nullability without treating absence as null", () => {
    const target = { name: "item", type: "string" };
    const optional = { name: "input.item", type: "string", port: { ...target, required: false } };
    expect(nativePortCanSupply(optional, target)).toBe(false);
    expect(nativePortCanSupply(optional, { ...target, required: false })).toBe(true);
    expect(nativePortCanSupply({ ...optional, port: { ...optional.port, default: "fallback" } }, target)).toBe(true);
    expect(nativePortCanSupply({ ...optional, port: { ...optional.port, nullable: true, default: null } }, target)).toBe(false);
    expect(nativePortCanSupply({ name: "input.items", type: "string[]", port: { name: "items", type: "string[]", nullable: true } }, { ...target, nullable: true })).toBe(false);
  });

  it("rejects disjoint array bounds and conflicting file media types", () => {
    const array = { name: "input.items", type: "string[]", port: { name: "items", type: "string[]", min_items: 2, max_items: 4 } };
    expect(nativePortCanSupply(array, { name: "items", type: "string[]", min_items: 5 })).toBe(false);
    expect(nativePortCanSupply(array, { name: "items", type: "string[]", max_items: 1 })).toBe(false);
    expect(nativePortCanSupply(array, { name: "items", type: "string[]", min_items: 3 })).toBe(true);
    const file = { name: "render.file", type: "file", port: { name: "file", type: "file", file: { media_type: "Text/Plain; charset=utf-8" } } };
    expect(nativePortCanSupply(file, { name: "file", type: "file", file: { media_type: "text/plain" } })).toBe(true);
    expect(nativePortCanSupply(file, { name: "file", type: "file", file: { media_type: "video/mp4" } })).toBe(false);
  });
});
