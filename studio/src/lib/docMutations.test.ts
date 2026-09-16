import { describe, expect, it } from "vitest";
import type { IterDocument } from "@/api/types";
import { createEmptyDocument } from "@/lib/defaults";
import { addToolToNode, assignFieldToNode } from "./docMutations";

// The canvas mutates a document by spreading it: a public contract, and the
// workflow's `contract:`, ride through a node edit untouched — the save
// carries back what the author declared, an explicit null default included.
describe("document mutations keep the public contract", () => {
  it("a node edit leaves the contracts and the workflow's contract as they were", () => {
    const base = createEmptyDocument();
    const agent = base.agents[0]?.name ?? "agent_1";
    const doc: IterDocument = {
      ...base,
      contracts: [
        {
          name: "c",
          version: 0,
          inputs: [{ name: "goal", type: "string", default: null, required: false }],
          outputs: [{ name: "url", type: "string", from: `${agent}.url` }],
          criteria: [{ name: "k", kind: "min_length", port: "input.goal", params: { min: 2 } }],
          effects: [{ name: "ships", paid: true }],
        },
      ],
      workflows: [{ name: "w", entry: agent, contract: "c", edges: [] }],
    };
    const out = addToolToNode(assignFieldToNode(doc, agent, "model", "m"), agent, "bash");
    expect(out.contracts).toBe(doc.contracts);
    expect(out.workflows[0]?.contract).toBe("c");
    expect(out.contracts?.[0]?.inputs?.[0]?.default).toBeNull();
    expect(out.contracts?.[0]?.version).toBe(0);
    expect(out.agents[0]?.model).toBe("m");
  });
});
