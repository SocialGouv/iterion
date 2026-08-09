import { describe, expect, it } from "vitest";

import type { RunEvent } from "@/api/runs";
import { buildExecutionsAt } from "./snapshotReducer";

function nodeStarted(seq: number, node: string, ts = "2026-05-14T12:00:00Z"): RunEvent {
  return { seq, timestamp: ts, type: "node_started", run_id: "r1", branch_id: "main", node_id: node };
}

describe("snapshotReducer terminal run events", () => {
  it("run_cancelled closes every still-running exec", () => {
    const events: RunEvent[] = [
      nodeStarted(1, "a"),
      nodeStarted(2, "b"),
      {
        seq: 3,
        timestamp: "2026-05-14T12:01:00Z",
        type: "run_cancelled",
        run_id: "r1",
        branch_id: "main",
        data: { reason: "ctrl-c" },
      },
    ];
    const out = buildExecutionsAt(events, 999);
    expect(out).toHaveLength(2);
    for (const e of out) {
      expect(e.status).toBe("failed");
      expect(e.finished_at).toBe("2026-05-14T12:01:00Z");
      expect(e.error).toBe("ctrl-c");
    }
  });

  it("run_finished closes every still-running exec as finished", () => {
    const events: RunEvent[] = [
      nodeStarted(1, "a"),
      nodeStarted(2, "b"),
      {
        seq: 3,
        timestamp: "2026-05-14T12:02:00Z",
        type: "run_finished",
        run_id: "r1",
        branch_id: "main",
      },
    ];
    const out = buildExecutionsAt(events, 999);
    expect(out).toHaveLength(2);
    for (const e of out) {
      expect(e.status).toBe("finished");
    }
  });

  it("run_failed closes parallel-sibling running execs too, not just the current one", () => {
    const events: RunEvent[] = [
      nodeStarted(1, "a"),
      nodeStarted(2, "b"),
      {
        seq: 3,
        timestamp: "2026-05-14T12:03:00Z",
        type: "run_failed",
        run_id: "r1",
        branch_id: "main",
        node_id: "a",
        data: { error: "boom" },
      },
    ];
    const out = buildExecutionsAt(events, 999);
    expect(out).toHaveLength(2);
    const byNode = Object.fromEntries(out.map((e) => [e.ir_node_id, e]));
    expect(byNode.a?.status).toBe("failed");
    expect(byNode.a?.error).toBe("boom");
    expect(byNode.b?.status).toBe("failed");
    expect(byNode.b?.error).toBe("boom");
  });

  it("does not touch already-terminal executions", () => {
    const events: RunEvent[] = [
      nodeStarted(1, "a"),
      {
        seq: 2,
        timestamp: "2026-05-14T12:00:30Z",
        type: "node_finished",
        run_id: "r1",
        branch_id: "main",
        node_id: "a",
      },
      nodeStarted(3, "b"),
      {
        seq: 4,
        timestamp: "2026-05-14T12:01:00Z",
        type: "run_cancelled",
        run_id: "r1",
        branch_id: "main",
      },
    ];
    const out = buildExecutionsAt(events, 999);
    const byNode = Object.fromEntries(out.map((e) => [e.ir_node_id, e]));
    expect(byNode.a?.status).toBe("finished");
    expect(byNode.a?.finished_at).toBe("2026-05-14T12:00:30Z");
    expect(byNode.b?.status).toBe("failed");
  });
});

describe("snapshotReducer run_rewound", () => {
  const rewindEvents = (): RunEvent[] => [
    nodeStarted(1, "analyze"),
    {
      seq: 2,
      timestamp: "2026-05-14T12:00:30Z",
      type: "node_finished",
      run_id: "r1",
      branch_id: "main",
      node_id: "analyze",
    },
    nodeStarted(3, "verify", "2026-05-14T12:01:00Z"),
    {
      seq: 4,
      timestamp: "2026-05-14T12:01:30Z",
      type: "node_finished",
      run_id: "r1",
      branch_id: "main",
      node_id: "verify",
    },
    nodeStarted(5, "report", "2026-05-14T12:02:00Z"),
    {
      seq: 6,
      timestamp: "2026-05-14T12:02:30Z",
      type: "node_finished",
      run_id: "r1",
      branch_id: "main",
      node_id: "report",
    },
    {
      seq: 7,
      timestamp: "2026-05-14T12:03:00Z",
      type: "run_rewound",
      run_id: "r1",
      branch_id: "main",
      node_id: "verify",
      data: { dropped_nodes: ["report", "verify"], to_node: "verify" },
    },
  ];

  it("erases the dropped nodes' execs, keeps the pre-pivot ones", () => {
    const out = buildExecutionsAt(rewindEvents(), 999);
    expect(out).toHaveLength(1);
    expect(out[0]?.ir_node_id).toBe("analyze");
    expect(out[0]?.status).toBe("finished");
  });

  it("scrubbing before the rewind still shows the pre-rewind state", () => {
    const out = buildExecutionsAt(rewindEvents(), 6);
    expect(out).toHaveLength(3);
    const byNode = Object.fromEntries(out.map((e) => [e.ir_node_id, e]));
    expect(byNode.verify?.status).toBe("finished");
    expect(byNode.report?.status).toBe("finished");
  });

  it("a post-rewind node_started recreates a clean exec, renumbered from 0", () => {
    const events = rewindEvents().concat([
      nodeStarted(8, "verify", "2026-05-14T12:04:00Z"),
    ]);
    const out = buildExecutionsAt(events, 999);
    expect(out).toHaveLength(2);
    const verify = out.find((e) => e.ir_node_id === "verify");
    expect(verify?.status).toBe("running");
    expect(verify?.finished_at).toBeUndefined();
    expect(verify?.loop_iteration).toBe(0);
  });

  it("drops every loop iteration of a rewound node", () => {
    const events: RunEvent[] = [];
    for (let i = 0; i < 3; i++) {
      events.push(nodeStarted(i * 2 + 1, "fix"));
      events.push({
        seq: i * 2 + 2,
        timestamp: "2026-05-14T12:00:30Z",
        type: "node_finished",
        run_id: "r1",
        branch_id: "main",
        node_id: "fix",
      });
    }
    expect(buildExecutionsAt(events, 999)).toHaveLength(3);
    events.push({
      seq: 7,
      timestamp: "2026-05-14T12:03:00Z",
      type: "run_rewound",
      run_id: "r1",
      branch_id: "main",
      node_id: "fix",
      data: { dropped_nodes: ["fix"] },
    });
    expect(buildExecutionsAt(events, 999)).toHaveLength(0);
  });

  it("ignores a run_rewound without dropped_nodes", () => {
    const events: RunEvent[] = [
      nodeStarted(1, "a"),
      {
        seq: 2,
        timestamp: "2026-05-14T12:01:00Z",
        type: "run_rewound",
        run_id: "r1",
        branch_id: "main",
      },
    ];
    expect(buildExecutionsAt(events, 999)).toHaveLength(1);
  });
});
