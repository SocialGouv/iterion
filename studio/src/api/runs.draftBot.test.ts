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
  lookupAssistantActions,
  lookupDraft,
  lookupEditorProposal,
  lookupFileChangeProposal,
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
  events: unknown[] = [],
) {
  const seen: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (/\/events(?:\?|$)/.test(url)) return json({ events });
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

  it("returns a save-only request without accepting model content or a path", async () => {
    stubApi(
      [{ node_id: "assistant", version: 3, written_at: "2026-09-08T12:00:00Z" }],
      {
        "assistant/3": {
          mode: "design",
          draft_bot: "",
          editor_session_id: "host-session",
          editor_revision: 11,
          editor_apply_intent: "explicit",
          editor_save_intent: "explicit",
          editor_path: "model/selected/path.bot",
        },
      },
    );

    await expect(lookupEditorProposal("run1")).resolves.toEqual({
      source: null,
      sessionId: "host-session",
      revision: 11,
      applyIntent: "none",
      saveIntent: "explicit",
    });
  });

  it("rejects an empty editor artifact without an actionable save intent", async () => {
    stubApi(
      [{ node_id: "assistant", version: 4, written_at: "2026-09-08T12:00:00Z" }],
      {
        "assistant/4": {
          mode: "design",
          draft_bot: "",
          editor_session_id: "host-session",
          editor_revision: 11,
          editor_apply_intent: "explicit",
          editor_save_intent: "none",
        },
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

describe("lookupAssistantActions", () => {
  it("returns typed requests from the newest capable turn", async () => {
    stubApi(
      [{ node_id: "nexie", version: 3, written_at: "2026-08-28T12:00:00Z" }],
      {
        "nexie/3": {
          assistant_actions: [
            {
              id: "board.issue.transition",
              intent: "explicit",
              args: { issue_id: "issue-1", to: "ready" },
            },
          ],
        },
      },
    );
    await expect(lookupAssistantActions("run1")).resolves.toEqual({
      requests: [
        {
          key: "run1:nexie:3:0",
          id: "board.issue.transition",
          intent: "explicit",
          args: { issue_id: "issue-1", to: "ready" },
        },
      ],
    });
  });

  it("lets an empty newest turn retire prior actions", async () => {
    stubApi(
      [
        { node_id: "old", version: 1, written_at: "2026-08-28T10:00:00Z" },
        { node_id: "new", version: 2, written_at: "2026-08-28T12:00:00Z" },
      ],
      {
        "old/1": {
          assistant_actions: [
            { id: "run.cancel", intent: "explicit", args: { run_id: "old" } },
          ],
        },
        "new/2": { assistant_actions: [] },
      },
    );
    await expect(lookupAssistantActions("run1")).resolves.toEqual({
      requests: [],
    });
  });

  it("uses a reviewed action instead of Copi's older first-pass request", async () => {
    stubApi(
      [
        { node_id: "copi", version: 1, written_at: "2026-08-28T10:00:00Z" },
        { node_id: "revise", version: 1, written_at: "2026-08-28T12:00:00Z" },
      ],
      {
        "copi/1": {
          assistant_actions: [
            { id: "run.resume", intent: "explicit", args: { run_id: "orphan" } },
          ],
        },
        "revise/1": {
          assistant_actions: [
            { id: "run.resume", intent: "explicit", args: { run_id: "root" } },
          ],
        },
      },
    );
    await expect(lookupAssistantActions("run1")).resolves.toMatchObject({
      requests: [{ id: "run.resume", args: { run_id: "root" } }],
    });
  });

  it("withholds the whole list and reports the exact rejected entries", async () => {
    stubApi(
      [{ node_id: "copi", version: 4, written_at: "2026-09-08T13:00:00Z" }],
      {
        "copi/4": {
          assistant_actions: [
            { id: "run.resume", intent: "explicit", args: { run_id: "root" } },
            { id: "resume-planner-r4-root", intent: "explicit", args: {} },
            { id: "run.watch", intent: "explicit", args: { run_id: "root" } },
          ],
        },
      },
    );
    await expect(lookupAssistantActions("run1")).resolves.toMatchObject({
      requests: [],
      invalid: {
        attempt: 1,
        rejected: [
          {
            index: 1,
            id: "resume-planner-r4-root",
            reason: "unknown_action_id",
          },
        ],
        validIds: ["run.resume", "run.watch"],
      },
    });
  });

  it("bounds the repair attempt from durable host-event history", async () => {
    stubApi(
      [{ node_id: "copi", version: 5, written_at: "2026-09-08T14:00:00Z" }],
      {
        "copi/5": {
          assistant_actions: [
            { id: "resume-planner-r4-root", intent: "explicit", args: {} },
          ],
        },
      },
      [
        {
          data: {
            answers: {
              host_event: {
                kind: "assistant-action-invalid",
                attempt: 1,
                source: "run1:copi:5",
              },
            },
          },
        },
      ],
    );
    await expect(lookupAssistantActions("run1")).resolves.toMatchObject({
      requests: [],
      invalid: { attempt: 2 },
    });
  });
});

describe("lookupFileChangeProposal", () => {
  it("accepts bounded exact replacements bound to the editor turn", async () => {
    stubApi(
      [{ node_id: "copi", version: 4, written_at: "2026-08-28T13:00:00Z" }],
      {
        "copi/4": {
          mode: "design",
          editor_session_id: "session",
          editor_revision: 9,
          file_changes_intent: "explicit",
          file_changes: [
            {
              scope: "workspace",
              path: "film_pipeline/matter.py",
              replacements: [{ before: "return 41", after: "return 42" }],
            },
          ],
        },
      },
    );
    await expect(lookupFileChangeProposal("run1")).resolves.toEqual({
      changes: [
        {
          scope: "workspace",
          path: "film_pipeline/matter.py",
          replacements: [{ before: "return 41", after: "return 42" }],
        },
      ],
      sessionId: "session",
      revision: 9,
      intent: "explicit",
    });
  });

  it("accepts a bounded declared-create shape bound to the editor turn", async () => {
    stubApi(
      [{ node_id: "copi", version: 5, written_at: "2026-08-28T14:00:00Z" }],
      {
        "copi/5": {
          editor_session_id: "session",
          editor_revision: 10,
          file_changes_intent: "explicit",
          file_changes: [{
            scope: "workspace",
            path: "iterion/vertical/planner-input-r4.json",
            create: { content: '{"run_id":"tabarria-v1-epics-r4"}\n' },
          }],
        },
      },
    );
    await expect(lookupFileChangeProposal("run1")).resolves.toEqual({
      changes: [{
        scope: "workspace",
        path: "iterion/vertical/planner-input-r4.json",
        create: { content: '{"run_id":"tabarria-v1-epics-r4"}\n' },
      }],
      sessionId: "session",
      revision: 10,
      intent: "explicit",
    });
  });

  it("fails closed on empty-before and unbound proposals", async () => {
    stubApi(
      [{ node_id: "copi", version: 4, written_at: "2026-08-28T13:00:00Z" }],
      {
        "copi/4": {
          mode: "design",
          editor_session_id: "",
          editor_revision: 0,
          file_changes_intent: "explicit",
          file_changes: [
            {
              scope: "bundle",
              path: "new.py",
              replacements: [{ before: "", after: "whole file" }],
            },
          ],
        },
      },
    );
    await expect(lookupFileChangeProposal("run1")).resolves.toEqual({
      changes: [],
      sessionId: null,
      revision: null,
      intent: "none",
    });
  });

  it("fails closed when a file change mixes create and replacements", async () => {
    stubApi(
      [{ node_id: "copi", version: 6, written_at: "2026-08-28T15:00:00Z" }],
      {
        "copi/6": {
          editor_session_id: "session",
          editor_revision: 11,
          file_changes_intent: "explicit",
          file_changes: [{
            scope: "workspace",
            path: "new.json",
            create: { content: "{}" },
            replacements: [{ before: "old", after: "new" }],
          }],
        },
      },
    );
    await expect(lookupFileChangeProposal("run1")).resolves.toEqual({
      changes: [],
      sessionId: null,
      revision: null,
      intent: "none",
    });
  });

  it("uses reviewed replacements and lets an empty review retire the first pass", async () => {
    const summaries = [
      { node_id: "copi", version: 1, written_at: "2026-08-28T10:00:00Z" },
      { node_id: "revise", version: 1, written_at: "2026-08-28T12:00:00Z" },
    ];
    const firstPass = {
      mode: "debug",
      editor_session_id: "session",
      editor_revision: 4,
      file_changes_intent: "explicit",
      file_changes: [
        {
          scope: "workspace",
          path: "workflow.bot",
          replacements: [{ before: "old", after: "incomplete" }],
        },
      ],
    };
    stubApi(summaries, {
      "copi/1": firstPass,
      "revise/1": {
        editor_session_id: "session",
        editor_revision: 4,
        file_changes_intent: "explicit",
        file_changes: [
          {
            scope: "workspace",
            path: "workflow.bot",
            replacements: [{ before: "old", after: "reviewed" }],
          },
        ],
      },
    });
    await expect(lookupFileChangeProposal("run1")).resolves.toEqual({
      changes: [
        {
          scope: "workspace",
          path: "workflow.bot",
          replacements: [{ before: "old", after: "reviewed" }],
        },
      ],
      sessionId: "session",
      revision: 4,
      intent: "explicit",
    });

    stubApi(summaries, {
      "copi/1": firstPass,
      "revise/1": {
        editor_session_id: "",
        editor_revision: 0,
        file_changes_intent: "none",
        file_changes: [],
      },
    });
    await expect(lookupFileChangeProposal("run1")).resolves.toEqual({
      changes: [],
      sessionId: null,
      revision: null,
      intent: "none",
    });
  });

  it("does not let a reviewed file-change artifact hide a validated draft/editor proposal", async () => {
    stubApi(
      [
        { node_id: "copi", version: 1, written_at: "2026-08-28T10:00:00Z" },
        { node_id: "revise", version: 1, written_at: "2026-08-28T12:00:00Z" },
      ],
      {
        "copi/1": {
          mode: "design",
          draft_bot: "workflow reviewed-draft:\n  entry: start\n",
          editor_session_id: "session",
          editor_revision: 4,
          editor_apply_intent: "explicit",
          editor_save_intent: "suggested",
          file_changes: [],
        },
        "revise/1": {
          assistant_actions: [],
          file_changes: [],
          file_changes_intent: "none",
          editor_session_id: "",
          editor_revision: 0,
        },
      },
    );
    await expect(lookupDraft("run1")).resolves.toEqual({
      source: null,
      designing: true,
    });
    await expect(lookupEditorProposal("run1")).resolves.toEqual({
      source: "workflow reviewed-draft:\n  entry: start\n",
      sessionId: "session",
      revision: 4,
      applyIntent: "explicit",
      saveIntent: "suggested",
    });
  });
});
