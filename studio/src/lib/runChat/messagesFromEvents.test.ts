import { describe, it, expect } from "vitest";

import type { RunEvent, WireWorkflow } from "@/api/runs";

import { messagesFromEvents } from "./messagesFromEvents";
import { irKindResolver } from "./nodeKindResolver";

// Minimal IR fixture covering one node per kind we care about. Lines
// up with the node ids the test events reference so the resolver
// resolves them to the right RunChatNodeKind.
const fixtureWorkflow: WireWorkflow = {
  name: "test",
  entry: "explorer",
  nodes: [
    { id: "explorer", kind: "agent" },
    { id: "planner", kind: "judge" },
    { id: "compute_step", kind: "compute" },
    { id: "shell_step", kind: "tool" },
    { id: "router_step", kind: "router" },
    { id: "ask_user", kind: "human" },
    { id: "done_node", kind: "done" },
    { id: "fail_node", kind: "fail" },
  ],
  edges: [],
};

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

describe("runChat messagesFromEvents", () => {
  it("returns no messages when the stream is empty", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [],
      snapshot: null,
    });
    expect(out).toEqual([]);
  });

  it("pushes a running banner on node_started for an agent node", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [evt("node_started", { node_id: "explorer" })],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({
      kind: "banner",
      nodeId: "explorer",
      status: "running",
      label: "explorer",
    });
  });

  it("emits a node-output card after an agent node_finished", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [
        evt("node_started", { node_id: "explorer" }),
        evt("node_finished", {
          node_id: "explorer",
          data: { output: { summary: "Found 3 issues." } },
        }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(2);
    expect(out[0]).toMatchObject({ kind: "banner", status: "done" });
    expect(out[1]).toMatchObject({
      kind: "node-output",
      nodeId: "explorer",
      iteration: 0,
    });
    const cardOutput = (out[1] as { output: Record<string, unknown> }).output;
    expect(cardOutput.summary).toBe("Found 3 issues.");
  });

  it("does NOT emit a node-output card for tool nodes (their stdout lives in the log panel)", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [
        evt("node_started", { node_id: "shell_step" }),
        evt("node_finished", {
          node_id: "shell_step",
          data: { output: { exit_code: 0 } },
        }),
      ],
      snapshot: null,
    });
    expect(out.filter((m) => m.kind === "node-output")).toEqual([]);
    expect(out.filter((m) => m.kind === "banner")).toHaveLength(1);
  });

  it("does NOT push messages for silent kinds (router/done/fail)", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [
        evt("node_started", { node_id: "router_step" }),
        evt("node_finished", { node_id: "router_step" }),
        evt("node_started", { node_id: "done_node" }),
        evt("node_finished", { node_id: "done_node" }),
        evt("node_started", { node_id: "fail_node" }),
        evt("node_finished", { node_id: "fail_node" }),
      ],
      snapshot: null,
    });
    expect(out).toEqual([]);
  });

  it("dedupes a duplicate node_started (WS replay)", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [
        evt("node_started", { node_id: "explorer" }),
        evt("node_started", { node_id: "explorer" }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
  });

  it("accumulates tool_started progress on the active banner", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [
        evt("node_started", { node_id: "explorer" }),
        evt("tool_started", {
          node_id: "explorer",
          data: { tool: "bash", input: { command: "ls -la" } },
        }),
        evt("tool_started", {
          node_id: "explorer",
          data: { tool: "read_file", input: { file_path: "README.md" } },
        }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
    const banner = out[0];
    if (banner?.kind === "banner") {
      expect(banner.progress?.toolCount).toBe(2);
      expect(banner.progress?.latestTool).toBe("read_file");
      expect(banner.progress?.latestToolHint).toBe("README.md");
    } else {
      throw new Error("expected banner");
    }
  });

  it("re-orders tool_started arriving before node_started by seq", () => {
    nextSeq = 1;
    // seq order: node_started=2, tool_started=1 — tool arrives first
    // in the array (out-of-order), but the fold sorts by seq before
    // processing, so the banner is registered before the tool event.
    const out = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [
        evt("tool_started", {
          seq: 2,
          node_id: "explorer",
          data: { tool: "bash", input: { command: "ls" } },
        }),
        evt("node_started", { seq: 1, node_id: "explorer" }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
    const banner = out[0];
    if (banner?.kind === "banner") {
      expect(banner.progress?.toolCount).toBe(1);
    } else {
      throw new Error("expected banner");
    }
  });

  it("renders a human-question on human_input_requested, then flips to answered on human_answers_recorded", () => {
    nextSeq = 1;
    const out = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [
        evt("node_started", { node_id: "ask_user" }),
        evt("human_input_requested", {
          node_id: "ask_user",
          data: { iteration: 0 },
        }),
        evt("human_answers_recorded", {
          node_id: "ask_user",
          data: { iteration: 0, answers: { context: "ship the board" } },
        }),
      ],
      snapshot: null,
    });
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({
      kind: "human-question",
      nodeId: "ask_user",
      status: "answered",
      userReply: "ship the board",
    });
  });

  it("recovers iteration from nodeIteration fallback when payload omits it", () => {
    nextSeq = 1;
    // Two iterations of the same human node. human_input_requested
    // payload omits iteration; without the nodeIteration fallback the
    // second iter-1 turn would collide with the first iter-0 turn
    // and the dedupe check would drop it.
    const out = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [
        evt("node_started", { node_id: "ask_user", data: { iteration: 0 } }),
        evt("human_input_requested", { node_id: "ask_user" }),
        evt("human_answers_recorded", {
          node_id: "ask_user",
          data: { answers: { text: "first" } },
        }),
        evt("node_started", { node_id: "ask_user", data: { iteration: 1 } }),
        evt("human_input_requested", { node_id: "ask_user" }),
      ],
      snapshot: null,
    });
    const human = out.filter((m) => m.kind === "human-question");
    expect(human).toHaveLength(2);
    expect(human[0]).toMatchObject({ status: "answered" });
    expect(human[1]).toMatchObject({ status: "pending" });
  });

  it("renders run_finished / run_failed / run_cancelled as session-closed markers and finalises active banners", () => {
    nextSeq = 1;
    const failed = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [
        evt("node_started", { node_id: "explorer" }),
        evt("run_failed", { data: { error: "boom" } }),
      ],
      snapshot: null,
    });
    expect(failed.filter((m) => m.kind === "banner")).toMatchObject([
      { status: "failed", errorMessage: "boom" },
    ]);
    expect(failed.filter((m) => m.kind === "session-closed")).toMatchObject([
      { reason: "failed" },
    ]);

    nextSeq = 1;
    const finished = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [evt("run_finished")],
      snapshot: null,
    });
    expect(finished).toMatchObject([{ kind: "session-closed", reason: "finished" }]);

    nextSeq = 1;
    const cancelled = messagesFromEvents({
      resolver: irKindResolver(fixtureWorkflow),
      events: [evt("run_cancelled")],
      snapshot: null,
    });
    expect(cancelled).toMatchObject([{ kind: "session-closed", reason: "cancelled" }]);
  });

  it("invokes resolver.extension after pushing the node-output card and lifts the payload through the renderer", () => {
    nextSeq = 1;
    const customResolver = {
      ...irKindResolver(fixtureWorkflow),
      emitsOutputCard: () => false, // route through extension instead
      extension: () => ({ tag: "custom-card", payload: { kind: "ok" } }),
    };
    const out = messagesFromEvents({
      resolver: customResolver,
      events: [
        evt("node_started", { node_id: "explorer" }),
        evt("node_finished", {
          node_id: "explorer",
          data: { output: { value: 42 } },
        }),
      ],
      snapshot: null,
    });
    // No node-output, but one extension follow-up.
    expect(out.filter((m) => m.kind === "node-output")).toHaveLength(0);
    const ext = out.find((m) => m.kind === "extension");
    expect(ext).toBeDefined();
    if (ext?.kind === "extension") {
      expect(ext.tag).toBe("custom-card");
      expect(ext.payload).toEqual({ kind: "ok" });
    }
  });
});
