import { describe, expect, it } from "vitest";

import type { Comment, IterDocument } from "@/api/types";
import {
  documentComments,
  documentGroups,
  mapDocumentComments,
  parseGroups,
} from "@/lib/groups";

// Comment provenance (#1282) made `document.comments` the file's head and
// tail ONLY: a comment written around a declaration is carried by that
// declaration, one written on an edge by that edge. Everything below holds
// the group grammar to the whole document rather than to that one list —
// mutate `documentComments` back to `doc.comments` and every case here
// reddens (#1576).

function doc(over: Partial<IterDocument> = {}): IterDocument {
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
    ...over,
  };
}

const group = (text: string): Comment => ({ text });

describe("parseGroups", () => {
  it("reads name and members, and skips a comment that is not a group", () => {
    expect(
      parseGroups([group("@group review: a, b"), group("just a note"), group("@group  ")]),
    ).toEqual([{ name: "review", nodeIds: ["a", "b"] }]);
  });
});

describe("documentGroups over every comment provenance", () => {
  it("sees a group annotation the file's head carries", () => {
    expect(documentGroups(doc({ comments: [group("@group head: a, b")] }))).toEqual([
      { name: "head", nodeIds: ["a", "b"] },
    ]);
  });

  it("sees a group annotation attached to a node declaration", () => {
    const d = doc({
      agents: [
        {
          name: "a",
          model: "m",
          input: "in",
          output: "out",
          system: "s",
          user: "u",
          session: "fresh",
          comments: [group("@group attached: a, b")],
        },
      ],
    });
    expect(documentGroups(d)).toEqual([{ name: "attached", nodeIds: ["a", "b"] }]);
  });

  it("sees a group annotation attached to an edge", () => {
    const d = doc({
      workflows: [
        {
          name: "w",
          entry: "a",
          edges: [{ from: "a", to: "b", comments: [group("@group edged: a, b")] }],
        },
      ],
    });
    expect(documentGroups(d)).toEqual([{ name: "edged", nodeIds: ["a", "b"] }]);
  });

  // The carriers are not a list to keep in step: the walk keys on the
  // `comments` array itself, so a declaration kind added later is covered
  // the day its JSON arrives. This asserts that over every array the
  // document type declares today — drop one from the walk and it reddens.
  it("sees one on every declaration array of the document", () => {
    const named = (kind: string) => ({
      name: kind,
      comments: [group(`@group ${kind}: a, b`)],
    });
    const d = doc({
      mcp_servers: [named("mcp_servers")] as IterDocument["mcp_servers"],
      prompts: [named("prompts")] as unknown as IterDocument["prompts"],
      schemas: [named("schemas")] as unknown as IterDocument["schemas"],
      cursors: [named("cursors")] as unknown as IterDocument["cursors"],
      agents: [named("agents")] as unknown as IterDocument["agents"],
      judges: [named("judges")] as unknown as IterDocument["judges"],
      routers: [named("routers")] as unknown as IterDocument["routers"],
      humans: [named("humans")] as unknown as IterDocument["humans"],
      tools: [named("tools")] as unknown as IterDocument["tools"],
      computes: [named("computes")] as unknown as IterDocument["computes"],
      subbots: [named("subbots")] as unknown as IterDocument["subbots"],
      fails: [named("fails")] as unknown as IterDocument["fails"],
      contracts: [named("contracts")] as unknown as IterDocument["contracts"],
      workflows: [named("workflows")] as unknown as IterDocument["workflows"],
      comments: [group("@group comments: a, b")],
    });
    expect(documentGroups(d).map((g) => g.name).sort()).toEqual(
      [
        "agents",
        "comments",
        "computes",
        "contracts",
        "cursors",
        "fails",
        "humans",
        "judges",
        "mcp_servers",
        "prompts",
        "routers",
        "schemas",
        "subbots",
        "tools",
        "workflows",
      ].sort(),
    );
  });

  // `Comment.text` is `omitempty` on the wire (pkg/dsl/ast/jsonenc.go:182),
  // and a bare `##` — the paragraph break every long head block uses — has
  // no text, so it arrives as `{}` in the SAME array as the @group lines.
  // 8 of them in bots/feature-dev/main.bot alone.
  it("sees the groups of a list that also holds a bare ## comment", () => {
    const d = doc({
      comments: [
        group("@group head: a, b"),
        { text: "" } as Comment,
        JSON.parse('{"file":"main.bot"}') as Comment,
      ],
    });
    expect(documentGroups(d)).toEqual([{ name: "head", nodeIds: ["a", "b"] }]);
  });

  it("sees one on a declaration whose comments include a bare ##", () => {
    const d = doc({
      agents: [
        {
          name: "a",
          model: "m",
          input: "in",
          output: "out",
          system: "s",
          user: "u",
          session: "fresh",
          comments: [
            { text: "pipeline stage" },
            JSON.parse('{"file":"lib/nodes.bot"}') as Comment,
            group("@group attached: a, b"),
          ],
        },
      ],
    });
    expect(documentGroups(d)).toEqual([{ name: "attached", nodeIds: ["a", "b"] }]);
  });

  it("returns nothing for an absent document rather than throwing", () => {
    expect(documentGroups(null)).toEqual([]);
    expect(documentComments(undefined)).toEqual([]);
  });
});

describe("mapDocumentComments", () => {
  it("rewrites a comment the declaration carries, in place", () => {
    const d = doc({
      agents: [
        {
          name: "a",
          model: "m",
          input: "in",
          output: "out",
          system: "s",
          user: "u",
          session: "fresh",
          comments: [group("@group attached: a, b"), group("keep me")],
        },
      ],
    });
    const next = mapDocumentComments(d, (c) =>
      c.text.startsWith("@group ") ? { ...c, text: "@group attached: a, b, c" } : c,
    );
    expect(next.agents[0]!.comments).toEqual([
      { text: "@group attached: a, b, c" },
      { text: "keep me" },
    ]);
    // …and the source document is untouched: the store keeps it for undo.
    expect(d.agents[0]!.comments![0]!.text).toBe("@group attached: a, b");
  });

  it("drops a comment the rewrite returns null for, wherever it lives", () => {
    const d = doc({
      comments: [group("@group head: a, b")],
      workflows: [
        {
          name: "w",
          entry: "a",
          edges: [{ from: "a", to: "b", comments: [group("@group edged: a, b")] }],
        },
      ],
    });
    const next = mapDocumentComments(d, (c) => (c.text.startsWith("@group ") ? null : c));
    expect(next.comments).toEqual([]);
    expect(next.workflows[0]!.edges[0]!.comments).toEqual([]);
  });

  // A rewrite that changes nothing has to hand the SAME object back: the
  // canvas re-renders on document identity, so a fresh object per keystroke
  // would relayout the graph for a rename that touched no group.
  it("returns the same document when no comment changed", () => {
    const d = doc({ comments: [group("a note")] });
    expect(mapDocumentComments(d, (c) => c)).toBe(d);
  });
});

// `parseGroups` does not dedupe, and `documentComments` now surfaces every
// carrier instead of the head list alone — so a hand-written `.bot` that
// declares the same group twice yields two annotations of one name.
// `documentToGraph` keys its group node `makeGroupNodeId(g.name)`, so React
// Flow gets two nodes sharing one id and one box becomes unrenderable.
// `addGroup`'s duplicate check cannot prevent it: the second declaration was
// typed by hand, not drawn.
describe("two declarations of one group name", () => {
  it("reads as ONE group — the file's own list wins over a declaration's", () => {
    const d = doc({
      comments: [group("@group deploy: build, ship")],
      agents: [
        {
          name: "build",
          model: "m",
          input: "in",
          output: "out",
          system: "s",
          user: "u",
          session: "fresh",
          comments: [group("@group deploy: build, verify")],
        },
      ],
    });
    expect(documentGroups(d)).toEqual([{ name: "deploy", nodeIds: ["build", "ship"] }]);
  });

  it("leaves parseGroups itself alone — it reports what the text says", () => {
    // The dedupe belongs to the single reader every call site goes through,
    // not to the grammar: a caller parsing ONE comment must still get it.
    expect(
      parseGroups([group("@group deploy: a, b"), group("@group deploy: a, c")]),
    ).toHaveLength(2);
  });
});
