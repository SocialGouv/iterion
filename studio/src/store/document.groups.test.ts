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

});

// A group survives the loss of the DECLARATION that happened to carry its
// comment: the annotation names its members by id, and the members that are
// left are still on the canvas. Before comment provenance was honoured this
// was invisible — the group was not drawn at all — so the loss only became
// observable once the canvas started reading it.
describe("a group outliving the declaration that carried it", () => {
  it("keeps the group when its carrier is removed and two members remain", () => {
    const s = store();
    expect(documentGroups(s.getState().document)).toHaveLength(1);
    s.getState().removeNode("plan");
    expect(documentGroups(s.getState().document)).toEqual([
      { name: "review", nodeIds: ["write", "ship"] },
    ]);
  });

  it("still dissolves it when the removal leaves fewer than two members", () => {
    const s = store();
    s.getState().removeNode("write");
    s.getState().removeNode("plan");
    expect(documentGroups(s.getState().document)).toEqual([]);
  });
});

// `duplicateNode` deep-clones a declaration, comments included. A @group
// annotation names its members by id, so a verbatim copy declares a SECOND
// group of the same name — two canvas nodes sharing one React Flow id, and
// the line written twice into the .bot on save.
describe("duplicating a node that carries a group annotation", () => {
  it("does not clone the annotation", () => {
    const s = store();
    s.getState().duplicateNode("plan");
    expect(documentGroups(s.getState().document)).toEqual([
      { name: "review", nodeIds: ["plan", "write", "ship"] },
    ]);
  });

  it("keeps the declaration's other comments on the copy", () => {
    const s = store();
    const doc = s.getState().document!;
    s.getState().setDocument({
      ...doc,
      agents: doc.agents.map((a) =>
        a.name === "plan"
          ? { ...a, comments: [{ text: "why this node exists" }, ...(a.comments ?? [])] }
          : a,
      ),
    });
    const copy = s.getState().duplicateNode("plan");
    const cloned = s.getState().document!.agents.find((a) => a.name === copy);
    expect(cloned?.comments).toEqual([{ text: "why this node exists" }]);
  });
});
