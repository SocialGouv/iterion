// A `.bot` draft produced in conversation lives ONLY as a node artifact: the
// assistant that wrote it cannot write to the workspace. These tests pin how
// it is found, because the two properties that matter are easy to lose.
//
//   by SHAPE, not by node name — the chat registry is manifest-driven, so a
//   hardcoded "copi" would put one bot's node id back into studio code;
//   newest FIRST — a conversation drafts repeatedly, and the operator means
//   the last one.
import { afterEach, describe, expect, it, vi } from "vitest";

import { findDraftBotSource } from "./runs/artifacts";

afterEach(() => {
  vi.unstubAllGlobals();
});

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// Routes the two endpoints findDraftBotSource walks: the per-run summary list,
// then one artifact fetch per node until a draft turns up.
function stubApi(
  summaries: Array<{ node_id: string; version: number; written_at: string }>,
  artifacts: Record<string, unknown>,
) {
  const seen: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (/\/artifacts(\?|$)/.test(url)) return json({ artifacts: summaries });
      const m = url.match(/\/artifacts\/([^/?]+)\/(\d+)/);
      if (m) {
        const key = `${m[1]}/${m[2]}`;
        seen.push(key);
        const data = artifacts[key];
        if (data === undefined) return json({ error: "gone" }, 404);
        return json({ node_id: m[1], version: Number(m[2]), data });
      }
      return json({ error: "unexpected " + url }, 500);
    }),
  );
  return seen;
}

describe("findDraftBotSource", () => {
  it("finds a draft on ANY node, by the field's shape", async () => {
    stubApi(
      [{ node_id: "some_other_bot_node", version: 3, written_at: "2026-08-22T10:00:00Z" }],
      { "some_other_bot_node/3": { draft_bot: "workflow demo:\n  entry: a\n" } },
    );
    await expect(findDraftBotSource("run1")).resolves.toContain("workflow demo:");
  });

  it("prefers the newest artifact when several carry a draft", async () => {
    stubApi(
      [
        { node_id: "older", version: 1, written_at: "2026-08-22T10:00:00Z" },
        { node_id: "newer", version: 1, written_at: "2026-08-22T12:00:00Z" },
      ],
      {
        "older/1": { draft_bot: "OLD" },
        "newer/1": { draft_bot: "NEW" },
      },
    );
    await expect(findDraftBotSource("run1")).resolves.toBe("NEW");
  });

  it("keeps looking past an unreadable artifact", async () => {
    const seen = stubApi(
      [
        { node_id: "broken", version: 9, written_at: "2026-08-22T12:00:00Z" },
        { node_id: "good", version: 1, written_at: "2026-08-22T10:00:00Z" },
      ],
      { "good/1": { draft_bot: "RECOVERED" } },
    );
    await expect(findDraftBotSource("run1")).resolves.toBe("RECOVERED");
    expect(seen).toEqual(["broken/9", "good/1"]);
  });

  it("reports no draft rather than an empty one", async () => {
    stubApi(
      [{ node_id: "copi", version: 2, written_at: "2026-08-22T10:00:00Z" }],
      { "copi/2": { draft_bot: "   " } },
    );
    await expect(findDraftBotSource("run1")).resolves.toBeNull();
  });

  it("reports no draft for a conversation that produced none", async () => {
    stubApi(
      [{ node_id: "copi", version: 1, written_at: "2026-08-22T10:00:00Z" }],
      { "copi/1": { reply: "an answer with no workflow in it" } },
    );
    await expect(findDraftBotSource("run1")).resolves.toBeNull();
  });
});
