import { describe, expect, it } from "vitest";

import type { AgentDecl, IterDocument } from "@/api/types";
import { documentGroups } from "@/lib/groups";
import { createDocumentStore } from "./document";

// The canvas's visual groups ride on `## @group <name>: a, b`. Since comment
// provenance (#1282) such an annotation written around a declaration is
// carried BY that declaration, so every store operation that reaches a group
// has to reach it there too — not only on the file's head list (#1576).

function agent(name: string, over: Partial<AgentDecl> = {}): AgentDecl {
  return {
    name,
    model: "m",
    input: "in",
    output: "out",
    system: "s",
    user: "u",
    session: "fresh",
    ...over,
  };
}

/** A document whose only @group annotation is written on the first agent. */
function docWithAttachedGroup(): IterDocument {
  return {
    prompts: [],
    schemas: [],
    agents: [
      agent("plan", { comments: [{ text: "@group review: plan, write, ship" }] }),
      agent("write"),
      agent("ship"),
    ],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [{ name: "w", entry: "plan", edges: [] }],
    comments: [],
  };
}

function store() {
  const s = createDocumentStore();
  s.getState().setDocument(docWithAttachedGroup());
  return s;
}

describe("group operations reach an annotation carried by a declaration", () => {
  it("removeGroup drops it from the declaration that holds it", () => {
    const s = store();
    expect(documentGroups(s.getState().document)).toHaveLength(1);
    s.getState().removeGroup("review");
    expect(documentGroups(s.getState().document)).toEqual([]);
    expect(s.getState().document!.agents[0]!.comments).toEqual([]);
  });

  it("updateGroup rewrites it in place", () => {
    const s = store();
    s.getState().updateGroup("review", { nodeIds: ["plan", "write"] });
    expect(documentGroups(s.getState().document)).toEqual([
      { name: "review", nodeIds: ["plan", "write"] },
    ]);
    expect(s.getState().document!.agents[0]!.comments![0]!.text).toBe(
      "@group review: plan, write",
    );
  });

  it("addGroup refuses a name an attached annotation already uses", () => {
    const s = store();
    s.getState().addGroup({ name: "review", nodeIds: ["write", "ship"] });
    // Unchanged: the duplicate is refused, and no second `review` is
    // appended to the head list where nothing would notice the collision.
    expect(documentGroups(s.getState().document)).toEqual([
      { name: "review", nodeIds: ["plan", "write", "ship"] },
    ]);
    expect(s.getState().document!.comments).toEqual([]);
  });

  it("renameNode rewrites the member list of an attached annotation", () => {
    const s = store();
    s.getState().renameNode("write", "draft");
    expect(documentGroups(s.getState().document)).toEqual([
      { name: "review", nodeIds: ["plan", "draft", "ship"] },
    ]);
  });

  it("removeNode drops the node from an attached annotation", () => {
    const s = store();
    s.getState().removeNode("ship");
    expect(documentGroups(s.getState().document)).toEqual([
      { name: "review", nodeIds: ["plan", "write"] },
    ]);
  });

  // These two assert an ABSENCE, which a reader blind to the declaration
  // satisfies for the wrong reason — it never saw the annotation at all. The
  // precondition is what makes them witnesses: the group has to be visible
  // before the operation that is supposed to take it away.
  it("removeNode dissolves an attached annotation that falls below two members", () => {
    const s = store();
    expect(documentGroups(s.getState().document)).toHaveLength(1);
    s.getState().removeNode("write");
    s.getState().removeNode("ship");
    expect(documentGroups(s.getState().document)).toEqual([]);
    expect(s.getState().document!.agents[0]!.comments).toEqual([]);
  });

  // The annotation is on the declaration being removed: it goes with it,
  // and nothing is left pointing at a node that no longer exists.
  it("removeNode takes the annotation with the declaration that carried it", () => {
    const s = store();
    expect(documentGroups(s.getState().document)).toHaveLength(1);
    s.getState().removeNode("plan");
    expect(documentGroups(s.getState().document)).toEqual([]);
    expect(s.getState().document!.agents.map((a) => a.name)).toEqual(["write", "ship"]);
  });
});
