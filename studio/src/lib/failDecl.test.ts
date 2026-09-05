import { describe, it, expect } from "vitest";
import type { IterDocument, WorkflowDecl } from "@/api/types";
import { findNodeDecl, getAllNodeNames } from "./defaults";

function doc(partial: Partial<IterDocument>): IterDocument {
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
  } as IterDocument;
}

// The canvas renders a named fail node, but every OTHER studio surface that
// resolves a node by name skipped `doc.fails`: the inspector reported it
// "not found", and the name was invisible to the collision check — so a
// user could create an agent with the same name and only find out at
// compile time (Rf16ca4).
describe("a named fail node is a first-class node in the studio", () => {
  const d = doc({
    computes: [{ name: "gate", output: "gauge", expr: [] }],
    fails: [
      {
        name: "plan_exhausted",
        code: "PLAN_BUDGET_EXHAUSTED",
        message: "planning used {{outputs.gate.pct}}% of max_duration",
        resumable: true,
      },
    ],
    workflows: [
      {
        name: "main",
        entry: "gate",
        edges: [{ from: "gate", to: "plan_exhausted" }],
      } as WorkflowDecl,
    ],
  });

  it("resolves through findNodeDecl so the inspector can render it", () => {
    const match = findNodeDecl(d, "plan_exhausted");
    expect(match).not.toBeNull();
    expect(match!.kind).toBe("fail");
    expect((match!.decl as { code?: string }).code).toBe("PLAN_BUDGET_EXHAUSTED");
    expect((match!.decl as { resumable?: boolean }).resumable).toBe(true);
  });

  it("contributes its name to the collision set", () => {
    const names = getAllNodeNames(d);
    expect(names.has("plan_exhausted")).toBe(true);
    expect(names.has("gate")).toBe(true);
  });

  it("does not shadow an unrelated name", () => {
    expect(findNodeDecl(d, "not_declared")).toBeNull();
  });
});
