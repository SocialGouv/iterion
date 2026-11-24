import { describe, expect, it } from "vitest";

import type { RunEvent } from "@/api/runs";

import { groupToolCalls, nestedItems, type ToolCall } from "./toolCallModel";

// The events one codemode program emits, shaped as pkg/backend/model's store
// hooks write them: the program's own start and completion, and each nested
// call naming the program as its parent, with its batch slot when it came
// in a call.parallel batch.
function ev(seq: number, type: RunEvent["type"], data: Record<string, unknown>): RunEvent {
  return { seq, timestamp: "2026-01-01T00:00:00Z", type, run_id: "r", node_id: "n", data } as RunEvent;
}

const PROGRAM_REPORT = {
  limits: { tool_calls: 64, wall_ns: 30_000_000_000 },
  calls: 3,
  source_bytes: 40,
};

function programEvents(): RunEvent[] {
  return [
    ev(1, "tool_started", { tool: "codemode", tool_use_id: "toolu_P", input: '{"source":"x"}' }),
    ev(2, "tool_started", { tool: "glob", tool_use_id: "codemode_A_1", parent_tool_use_id: "toolu_P", input: "{}" }),
    ev(3, "tool_called", { tool: "glob", tool_use_id: "codemode_A_1", parent_tool_use_id: "toolu_P", output: "a" }),
    ev(4, "tool_started", {
      tool: "grep",
      tool_use_id: "codemode_A_2",
      parent_tool_use_id: "toolu_P",
      batch: { first: 2, size: 2, index: 0 },
    }),
    ev(5, "tool_started", {
      tool: "grep",
      tool_use_id: "codemode_A_3",
      parent_tool_use_id: "toolu_P",
      batch: { first: 2, size: 2, index: 1 },
    }),
    // The batch's second entry finishes first.
    ev(6, "tool_called", {
      tool: "grep",
      tool_use_id: "codemode_A_3",
      parent_tool_use_id: "toolu_P",
      batch: { first: 2, size: 2, index: 1 },
    }),
    ev(7, "tool_error", {
      tool: "grep",
      tool_use_id: "codemode_A_2",
      parent_tool_use_id: "toolu_P",
      batch: { first: 2, size: 2, index: 0 },
      error: "boom",
    }),
    ev(8, "tool_called", { tool: "codemode", tool_use_id: "toolu_P", output: "3", codemode: PROGRAM_REPORT }),
    ev(9, "tool_started", { tool: "Bash", tool_use_id: "toolu_Q", input: "{}" }),
    ev(10, "tool_called", { tool: "Bash", tool_use_id: "toolu_Q", output: "ok" }),
  ];
}

// The shape a reader sees: ids and nesting, with the batch slot of each.
function shape(calls: ToolCall[]): unknown {
  return calls.map((c) => ({
    id: c.toolUseID,
    seq: c.seq,
    running: c.running ?? false,
    batch: c.batch?.index,
    nested: c.nested ? shape(c.nested) : undefined,
  }));
}

describe("groupToolCalls", () => {
  it("hangs a program's nested calls under it, by parent id, in either event order", () => {
    const events = programEvents();
    const want = [
      {
        id: "toolu_P",
        seq: 8,
        running: false,
        batch: undefined,
        nested: [
          { id: "codemode_A_1", seq: 3, running: false, batch: undefined, nested: undefined },
          { id: "codemode_A_2", seq: 7, running: false, batch: 0, nested: undefined },
          { id: "codemode_A_3", seq: 6, running: false, batch: 1, nested: undefined },
        ],
      },
      { id: "toolu_Q", seq: 10, running: false, batch: undefined, nested: undefined },
    ];
    expect(shape(groupToolCalls(events))).toEqual(want);
    // The same events in reverse: grouping reads ids, never positions.
    expect(shape(groupToolCalls([...events].reverse()))).toEqual(want);
  });

  it("merges each call's start into its completion and keeps the program's report", () => {
    const [program] = groupToolCalls(programEvents());
    expect(program?.codemode).toEqual(PROGRAM_REPORT);
    expect(program?.input).toBe('{"source":"x"}');
    const failed = program?.nested?.find((c) => c.toolUseID === "codemode_A_2");
    expect(failed?.isError).toBe(true);
    expect(failed?.errorMsg).toBe("boom");
  });

  it("keeps events without a parent id flat, whatever their ids look like", () => {
    // Legacy events: an id shaped like a nested call's, and no parent key.
    const events = [
      ev(1, "tool_started", { tool: "codemode", tool_use_id: "toolu_P", input: '{"source":"x"}' }),
      ev(2, "tool_called", { tool: "glob", tool_use_id: "codemode_A_1" }),
      ev(3, "tool_called", { tool: "codemode", tool_use_id: "toolu_P" }),
      ev(4, "tool_called", { tool: "Bash" }),
    ];
    const got = groupToolCalls(events);
    expect(got.map((c) => c.toolName)).toEqual(["glob", "codemode", "Bash"]);
    expect(got.every((c) => c.nested === undefined)).toBe(true);
  });

  it("leaves a nested call whose program is outside the window at the top level", () => {
    const events = [
      ev(5, "tool_called", { tool: "glob", tool_use_id: "codemode_A_1", parent_tool_use_id: "toolu_gone" }),
      ev(6, "tool_called", { tool: "Bash", tool_use_id: "toolu_Q" }),
    ];
    expect(groupToolCalls(events).map((c) => c.toolUseID)).toEqual(["codemode_A_1", "toolu_Q"]);
  });

  it("gives a running program a card for the nested calls it has finished", () => {
    const events = programEvents().slice(0, 3);
    const got = groupToolCalls(events);
    expect(shape(got)).toEqual([
      {
        id: "toolu_P",
        seq: 1,
        running: true,
        batch: undefined,
        nested: [{ id: "codemode_A_1", seq: 3, running: false, batch: undefined, nested: undefined }],
      },
    ]);
  });

  it("keeps two programs that share an id apart, and their nested calls flat", () => {
    // A provider numbering calls per turn reuses `call_0` across steps.
    const events = [
      ev(1, "tool_started", { tool: "codemode", tool_use_id: "call_0", input: '{"source":"A"}' }),
      ev(2, "tool_started", { tool: "glob", tool_use_id: "codemode_A_1", parent_tool_use_id: "call_0" }),
      ev(3, "tool_called", { tool: "glob", tool_use_id: "codemode_A_1", parent_tool_use_id: "call_0" }),
      ev(4, "tool_called", { tool: "codemode", tool_use_id: "call_0", output: "a" }),
      ev(5, "tool_started", { tool: "codemode", tool_use_id: "call_0", input: '{"source":"B"}' }),
      ev(6, "tool_started", { tool: "grep", tool_use_id: "codemode_B_1", parent_tool_use_id: "call_0" }),
      ev(7, "tool_called", { tool: "grep", tool_use_id: "codemode_B_1", parent_tool_use_id: "call_0" }),
      ev(8, "tool_called", { tool: "codemode", tool_use_id: "call_0", output: "b" }),
    ];
    const want = [
      { seq: 3, id: "codemode_A_1", input: undefined, nested: undefined },
      { seq: 4, id: "call_0", input: '{"source":"A"}', nested: undefined },
      { seq: 7, id: "codemode_B_1", input: undefined, nested: undefined },
      { seq: 8, id: "call_0", input: '{"source":"B"}', nested: undefined },
    ];
    const view = (calls: ToolCall[]) =>
      calls.map((c) => ({ seq: c.seq, id: c.toolUseID, input: c.input, nested: c.nested }));
    expect(view(groupToolCalls(events))).toEqual(want);
    expect(view(groupToolCalls([...events].reverse()))).toEqual(want);
  });

  it("keeps the launcher's refusal as its own card, its text untouched by the runner's event", () => {
    // The launcher records its refusal under the id and parent the runner
    // claimed; the runner's own tool_error for the call arrives later with
    // the same id and different text.
    const events = [
      ev(1, "tool_started", { tool: "codemode", tool_use_id: "toolu_P", input: '{"source":"x"}' }),
      ev(2, "tool_started", { tool: "mcp_github_delete_repo", tool_use_id: "codemode_A_1", parent_tool_use_id: "toolu_P" }),
      ev(3, "tool_error", {
        tool: "mcp_github_delete_repo",
        tool_use_id: "codemode_A_1",
        parent_tool_use_id: "toolu_P",
        emitted_by: "launcher",
        error: "launcher: denied by mcp__github__delete_*",
      }),
      ev(4, "tool_error", {
        tool: "mcp_github_delete_repo",
        tool_use_id: "codemode_A_1",
        parent_tool_use_id: "toolu_P",
        error: "all good, nothing was refused",
      }),
      ev(5, "tool_error", { tool: "codemode", tool_use_id: "toolu_P", error: "refused" }),
    ];
    for (const order of [events, [...events].reverse()]) {
      const [program, ...rest] = groupToolCalls(order);
      expect(rest).toEqual([]);
      // The runner's card sorts by its start (seq 2), the launcher's by its
      // own event (seq 3).
      expect(program?.nested?.map((c) => [c.seq, c.emittedBy, c.errorMsg])).toEqual([
        [4, undefined, "all good, nothing was refused"],
        [3, "launcher", "launcher: denied by mcp__github__delete_*"],
      ]);
    }
  });

  it("shows every call of a forged cycle, at the top", () => {
    // A launcher refusal claiming id X under Y, and a relayed event claiming
    // id Y under X; then two relayed events naming each other.
    const events = [
      ev(1, "tool_error", { tool: "a", tool_use_id: "X", parent_tool_use_id: "Y", emitted_by: "launcher", error: "denied" }),
      ev(2, "tool_called", { tool: "b", tool_use_id: "Y", parent_tool_use_id: "X" }),
      ev(3, "tool_called", { tool: "c", tool_use_id: "M", parent_tool_use_id: "N" }),
      ev(4, "tool_called", { tool: "d", tool_use_id: "N", parent_tool_use_id: "M" }),
    ];
    const got = groupToolCalls(events);
    const all = (calls: ToolCall[]): number[] => calls.flatMap((c) => [c.seq, ...all(c.nested ?? [])]);
    expect(all(got).sort()).toEqual([1, 2, 3, 4]);
    expect(got.map((c) => c.seq)).toEqual([2, 3, 4]);
    expect(got[0]?.nested?.map((c) => c.seq)).toEqual([1]);
  });

  it("shows a nested call of a nested call, under both", () => {
    const events = [
      ev(1, "tool_called", { tool: "codemode", tool_use_id: "P" }),
      ev(2, "tool_called", { tool: "agent", tool_use_id: "Q", parent_tool_use_id: "P" }),
      ev(3, "tool_called", { tool: "glob", tool_use_id: "R", parent_tool_use_id: "Q" }),
    ];
    expect(shape(groupToolCalls(events))).toEqual([
      {
        id: "P",
        seq: 1,
        running: false,
        batch: undefined,
        nested: [
          {
            id: "Q",
            seq: 2,
            running: false,
            batch: undefined,
            nested: [{ id: "R", seq: 3, running: false, batch: undefined, nested: undefined }],
          },
        ],
      },
    ]);
  });

  it("pairs nothing when several starts of one id are open, and shows each on its own", () => {
    const events = [
      ev(1, "tool_started", { tool: "read_file", tool_use_id: "call_0", input: '{"path":"a.md"}' }),
      ev(2, "tool_started", { tool: "read_file", tool_use_id: "call_0", input: '{"path":"b.md"}' }),
      ev(3, "tool_called", { tool: "read_file", tool_use_id: "call_0", output: "A" }),
      ev(4, "tool_called", { tool: "read_file", tool_use_id: "call_0", output: "B" }),
    ];
    const view = groupToolCalls(events).map((c) => [c.seq, c.unpaired ?? false, c.input, c.output]);
    expect(view).toEqual([
      [1, true, '{"path":"a.md"}', undefined],
      [2, true, '{"path":"b.md"}', undefined],
      [3, false, undefined, "A"],
      [4, false, undefined, "B"],
    ]);
  });

  it("never pairs a completion with a start that comes after it", () => {
    // Turn 1's call_0 was refused before it started; turn 2 reuses call_0.
    const events = [
      ev(1, "tool_error", { tool: "nope", tool_use_id: "call_0", error: "unknown tool: nope" }),
      ev(5, "tool_started", { tool: "read_file", tool_use_id: "call_0", input: '{"path":"turn2.md"}' }),
      ev(6, "tool_called", { tool: "read_file", tool_use_id: "call_0", output: "two" }),
    ];
    for (const order of [events, [...events].reverse()]) {
      expect(groupToolCalls(order).map((c) => [c.seq, c.input, c.output ?? c.errorMsg])).toEqual([
        [1, undefined, "unknown tool: nope"],
        [6, '{"path":"turn2.md"}', "two"],
      ]);
    }
  });

  it("does not show a running call no nested call names", () => {
    const events = [ev(1, "tool_started", { tool: "Bash", tool_use_id: "toolu_Q" })];
    expect(groupToolCalls(events)).toEqual([]);
  });
});

describe("nestedItems", () => {
  it("keeps a batch together in batch order, between the program's single calls", () => {
    const [program] = groupToolCalls(programEvents());
    const items = nestedItems(program?.nested ?? []);
    expect(
      items.map((i) =>
        i.kind === "call"
          ? i.call.toolUseID
          : { first: i.first, size: i.size, calls: i.calls.map((c) => c.toolUseID) },
      ),
    ).toEqual(["codemode_A_1", { first: 2, size: 2, calls: ["codemode_A_2", "codemode_A_3"] }]);
  });
});
