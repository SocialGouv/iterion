// A `.bot` draft produced in conversation lives ONLY as a node artifact: the
// assistant that wrote it cannot write to the workspace. These tests pin how
// it is found, because the two properties that matter are easy to lose.
//
//   by SHAPE, not by node name — the chat registry is manifest-driven, so a
//   hardcoded "copi" would put one bot's node id back into studio code;
//   newest FIRST — a conversation drafts repeatedly, and the operator means
//   the last one.
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  findDraftBotSource,
  lookupDraft,
  lookupEditorProposal,
} from "./runs/artifacts";

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

// The ORDER the operator asked for: settle where the work happens, then do it
// there. That needs the studio to tell "designing, nothing to show yet" apart
// from "draft in hand" — the first offers the editor, the second offers the
// draft. Collapsing them puts the invitation after the work again.
describe("lookupDraft — designing vs draft-ready", () => {
  it("reports designing before anything has been drafted", async () => {
    stubApi(
      [{ node_id: "copi", version: 0, written_at: "2026-08-23T10:00:00Z" }],
      { "copi/0": { mode: "design", reply: "let's build this in the editor" } },
    );
    await expect(lookupDraft("run1")).resolves.toEqual({
      source: null,
      designing: true,
    });
  });

  it("reports the draft once the turn produced one", async () => {
    stubApi(
      [{ node_id: "copi", version: 1, written_at: "2026-08-23T11:00:00Z" }],
      { "copi/1": { mode: "design", draft_bot: "workflow demo:\n" } },
    );
    const got = await lookupDraft("run1");
    expect(got.source).toContain("workflow demo:");
    expect(got.designing).toBe(true);
  });

  it("does not turn an editor-bound proposal into a new draft tab", async () => {
    stubApi(
      [{ node_id: "copi", version: 1, written_at: "2026-08-23T11:00:00Z" }],
      {
        "copi/1": {
          mode: "design",
          draft_bot: "workflow changed:\n",
          editor_session_id: "opaque-session",
          editor_revision: 3,
        },
      },
    );
    await expect(lookupDraft("run1")).resolves.toEqual({
      source: null,
      designing: true,
    });
  });

  it("treats a draft as designing even when the mode field is absent", async () => {
    stubApi(
      [{ node_id: "copi", version: 0, written_at: "2026-08-23T10:00:00Z" }],
      { "copi/0": { draft_bot: "workflow demo:\n" } },
    );
    await expect(lookupDraft("run1")).resolves.toMatchObject({
      designing: true,
    });
  });

  it("offers nothing for an ordinary answer", async () => {
    stubApi(
      [{ node_id: "copi", version: 0, written_at: "2026-08-23T10:00:00Z" }],
      { "copi/0": { mode: "info", reply: "C176 means…" } },
    );
    await expect(lookupDraft("run1")).resolves.toEqual({
      source: null,
      designing: false,
    });
  });

  it("lets the newest info posture retire an older design and draft", async () => {
    stubApi(
      [
        { node_id: "older", version: 1, written_at: "2026-08-23T10:00:00Z" },
        { node_id: "newer", version: 1, written_at: "2026-08-23T12:00:00Z" },
      ],
      {
        "older/1": { mode: "design", draft_bot: "STALE" },
        "newer/1": { mode: "info", reply: "we are done designing" },
      },
    );
    await expect(lookupDraft("run1")).resolves.toEqual({
      source: null,
      designing: false,
    });
  });
});

describe("lookupEditorProposal", () => {
  it("returns the source only with a complete session/revision binding", async () => {
    stubApi(
      [{ node_id: "copi", version: 2, written_at: "2026-08-23T12:00:00Z" }],
      {
        "copi/2": {
          mode: "design",
          draft_bot: "workflow changed:\n",
          editor_session_id: "opaque-session",
          editor_revision: 7,
          editor_apply_intent: "explicit",
          editor_save_intent: "explicit",
        },
      },
    );
    await expect(lookupEditorProposal("run1")).resolves.toEqual({
      source: "workflow changed:\n",
      sessionId: "opaque-session",
      revision: 7,
      applyIntent: "explicit",
      saveIntent: "explicit",
    });
  });

  it("lets a newer ordinary turn retire an older editor proposal", async () => {
    stubApi(
      [
        { node_id: "old", version: 1, written_at: "2026-08-23T10:00:00Z" },
        { node_id: "new", version: 1, written_at: "2026-08-23T12:00:00Z" },
      ],
      {
        "old/1": {
          mode: "design",
          draft_bot: "OLD",
          editor_session_id: "session",
          editor_revision: 1,
        },
        "new/1": { mode: "info", editor_session_id: "", editor_revision: 0 },
      },
    );
    await expect(lookupEditorProposal("run1")).resolves.toEqual({
      source: null,
      sessionId: null,
      revision: null,
      applyIntent: "none",
      saveIntent: "none",
    });
  });

  it("does not grant save autonomy for an unknown model value", async () => {
    stubApi(
      [{ node_id: "copi", version: 2, written_at: "2026-08-23T12:00:00Z" }],
      {
        "copi/2": {
          mode: "design",
          draft_bot: "workflow changed:\n",
          editor_session_id: "opaque-session",
          editor_revision: 7,
          editor_save_intent: "always-trust-me",
        },
      },
    );
    await expect(lookupEditorProposal("run1")).resolves.toMatchObject({
      applyIntent: "none",
      saveIntent: "none",
    });
  });
});
