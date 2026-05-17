import { describe, it, expect } from "vitest";

import type { RunEvent, RunSnapshot } from "@/api/runs";
import {
  FIRST_CLASS_BOTS,
  type FirstClassBot,
} from "@/lib/pilote/firstClassBots";

import { messagesFromEvents } from "./messagesFromEvents";

const whatsNext = FIRST_CLASS_BOTS["whats-next"] as FirstClassBot;

let nextSeq = 1;
function evt(
  type: string,
  fields: Partial<Omit<RunEvent, "type">> = {},
): RunEvent {
  return {
    seq: fields.seq ?? nextSeq++,
    timestamp: fields.timestamp ?? new Date().toISOString(),
    type,
    run_id: "run_test",
    branch_id: fields.branch_id,
    node_id: fields.node_id,
    data: fields.data,
  };
}

function snapshotWith(
  outputs: Record<string, Record<string, unknown>>,
): RunSnapshot {
  return {
    run: {
      id: "run_test",
      workflow_name: "whats-next",
      status: "running",
      created_at: "",
      updated_at: "",
      active_duration_ms: 0,
      checkpoint: { outputs },
    },
    executions: [],
    last_seq: 0,
  };
}

describe("messagesFromEvents", () => {
  it("returns no messages when the stream is empty", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [],
      snapshot: null,
    });
    expect(out).toEqual([]);
  });

  it("pushes a running banner on node_started for an agent node", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [evt("node_started", { node_id: "explore" })],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({
      kind: "banner",
      nodeId: "explore",
      status: "running",
      label: "Surveying the repository",
    });
  });

  it("flips the banner to done on node_finished, populating summary from checkpoint", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "explore" }),
        evt("node_finished", { node_id: "explore" }),
      ],
      snapshot: snapshotWith({ explore: { summary: "A short summary." } }),
    });
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({
      kind: "banner",
      status: "done",
      summary: "A short summary.",
    });
  });

  it("dedupes a duplicate node_started (WS replay)", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "explore" }),
        evt("node_started", { node_id: "explore" }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
  });

  it("ignores events for nodes not in the nodeMap", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "some_other_node" }),
        evt("node_finished", { node_id: "some_other_node" }),
      ],
      snapshot: null,
    });
    expect(out).toEqual([]);
  });

  it("does NOT render carry_roadmap (silent kind)", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "carry_roadmap" }),
        evt("node_finished", { node_id: "carry_roadmap" }),
      ],
      snapshot: null,
    });
    expect(out).toEqual([]);
  });

  it("renders a human-question on human_input_requested, then flips to answered on human_answers_recorded", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "ask_priorities" }),
        evt("human_input_requested", {
          node_id: "ask_priorities",
          data: { iteration: 0 },
        }),
        evt("human_answers_recorded", {
          node_id: "ask_priorities",
          data: { iteration: 0, answers: { context: "ship the board" } },
        }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({
      kind: "human-question",
      nodeId: "ask_priorities",
      status: "answered",
      userReply: "ship the board",
    });
  });

  it("emits a roadmap-card after propose_roadmap when the snapshot carries a valid roadmap", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "propose_roadmap" }),
        evt("node_finished", { node_id: "propose_roadmap" }),
      ],
      snapshot: snapshotWith({
        propose_roadmap: {
          long_term: [],
          short_term: [{ title: "X", body: "y", assignee: "vibe", args: {} }],
          next_action: { title: "A", body: "b", assignee: "", args: {} },
          rationale: "because",
        },
      }),
    });
    expect(out).toHaveLength(2);
    const card = out[1];
    expect(card).toMatchObject({
      kind: "roadmap-card",
      iteration: 0,
    });
    if (card && card.kind === "roadmap-card") {
      expect(card.roadmap.rationale).toBe("because");
      expect(card.roadmap.short_term).toHaveLength(1);
      expect(card.roadmap.next_action?.title).toBe("A");
    }
  });

  it("creates a fresh roadmap-card on each revise iteration (loop)", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "revise_roadmap", data: { iteration: 1 } }),
        evt("node_finished", { node_id: "revise_roadmap", data: { iteration: 1 } }),
        evt("node_started", { node_id: "revise_roadmap", data: { iteration: 2 } }),
        evt("node_finished", { node_id: "revise_roadmap", data: { iteration: 2 } }),
      ],
      snapshot: snapshotWith({
        revise_roadmap: {
          long_term: [],
          short_term: [],
          next_action: { title: "rev", body: "", assignee: "", args: {} },
          rationale: "",
        },
      }),
    });
    // 2 banners (iter 1 + iter 2) + 2 roadmap cards
    expect(out.filter((m) => m.kind === "banner")).toHaveLength(2);
    expect(out.filter((m) => m.kind === "roadmap-card")).toHaveLength(2);
  });

  it("emits an issues-summary card after emit_action", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "emit_action" }),
        evt("node_finished", { node_id: "emit_action" }),
      ],
      snapshot: snapshotWith({
        emit_action: {
          plan_path: "/tmp/p.md",
          created_issues: [
            {
              id: "native:1",
              title: "T1",
              horizon: "next_action",
              assignee: "vibe",
            },
          ],
          failed_issues: [],
          summary: "Created 1 issue.",
        },
      }),
    });
    const card = out.find((m) => m.kind === "issues-summary");
    expect(card).toBeDefined();
    if (card && card.kind === "issues-summary") {
      expect(card.createdIssues).toHaveLength(1);
      expect(card.createdIssues[0]!.id).toBe("native:1");
      expect(card.summary).toBe("Created 1 issue.");
    }
  });

  it("accumulates tool_started progress on the active banner", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "explore" }),
        evt("tool_started", {
          node_id: "explore",
          data: { tool: "bash", input: { command: "ls -la" } },
        }),
        evt("tool_started", {
          node_id: "explore",
          data: { tool: "read_file", input: { file_path: "README.md" } },
        }),
        evt("tool_started", {
          node_id: "explore",
          data: { tool: "glob", input: { pattern: "**/*.go" } },
        }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
    const banner = out[0];
    if (banner?.kind === "banner") {
      expect(banner.progress?.toolCount).toBe(3);
      expect(banner.progress?.latestTool).toBe("glob");
      expect(banner.progress?.latestToolHint).toBe("**/*.go");
    }
  });

  it("drops banner progress once the node finishes", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      bot: whatsNext,
      events: [
        evt("node_started", { node_id: "explore" }),
        evt("tool_started", {
          node_id: "explore",
          data: { tool: "bash", input: { command: "ls" } },
        }),
        evt("node_finished", { node_id: "explore" }),
      ],
      snapshot: snapshotWith({ explore: { summary: "Surveyed." } }),
    });
    expect(out).toHaveLength(1);
    const banner = out[0];
    if (banner?.kind === "banner") {
      expect(banner.status).toBe("done");
      expect(banner.progress).toBeUndefined();
      expect(banner.summary).toBe("Surveyed.");
    }
  });

  it("renders run_finished / run_failed / run_cancelled as session-closed markers", () => {
    nextSeq = 1;
    const finished = messagesFromEvents({
      bot: whatsNext,
      events: [evt("run_finished")],
      snapshot: null,
    });
    expect(finished).toMatchObject([{ kind: "session-closed", reason: "finished" }]);

    nextSeq = 1;
    const failed = messagesFromEvents({
      bot: whatsNext,
      events: [evt("run_failed")],
      snapshot: null,
    });
    expect(failed).toMatchObject([{ kind: "session-closed", reason: "failed" }]);

    nextSeq = 1;
    const cancelled = messagesFromEvents({
      bot: whatsNext,
      events: [evt("run_cancelled")],
      snapshot: null,
    });
    expect(cancelled).toMatchObject([
      { kind: "session-closed", reason: "cancelled" },
    ]);
  });
});
