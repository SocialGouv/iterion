// Once the run socket closed scope_lost (wsState "lost"), the view keeps the
// original run: the ID may name its successor now. The store is the choke
// point every same-ID writer goes through — REST snapshots and event
// hydration (the status poll, a refresh, a response already in flight), the
// optimistic status of an action, and the reconnect token Resume, answer,
// merge and steering bump. Each is refused there, and each is applied while
// the socket is not lost.
import { describe, expect, it } from "vitest";

import type { RunEvent, RunHeader, RunSnapshot } from "@/api/runs";

import { createRunStore } from "./run";

function header(name: string): RunHeader {
  return {
    id: "run-a",
    name,
    workflow_name: `${name}-wf`,
    status: "running",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  } as unknown as RunHeader;
}

function snapshot(name: string, lastSeq: number): RunSnapshot {
  return { run: header(name), executions: [], last_seq: lastSeq };
}

function event(seq: number, node: string): RunEvent {
  return {
    seq,
    timestamp: "2026-01-01T00:00:01Z",
    type: "node_started",
    run_id: "run-a",
    branch_id: "main",
    node_id: node,
    data: { kind: "agent", iteration: 0 },
  };
}

function originStore(lost: boolean) {
  const store = createRunStore();
  store.getState().applySnapshot(snapshot("origin", 10));
  store.getState().applyEventsBatch([event(9, "a")]);
  store.getState().setWsState(lost ? "lost" : "closed");
  return store;
}

describe("a lost run view keeps the original run", () => {
  it("drops a same-ID snapshot, even one newer than the original", () => {
    const store = originStore(true);
    const before = store.getState().snapshot;
    store.getState().applySnapshot(snapshot("successor", 99));
    expect(store.getState().snapshot?.run.name).toBe("origin");
    expect(store.getState().snapshot).toBe(before);
  });

  it("drops REST event hydration", () => {
    const store = originStore(true);
    store.getState().applyEventsBatch([event(100, "successor")]);
    store.getState().applyEvent(event(101, "successor"));
    expect(store.getState().events.map((e) => e.seq)).toEqual([9]);
  });

  it("keeps the original status when a late action completes", () => {
    const store = originStore(true);
    store.getState().setRunStatus("finished");
    expect(store.getState().snapshot?.run.status).toBe("running");
  });

  it("refuses the reconnect token", () => {
    const store = originStore(true);
    const token = store.getState().wsReconnectToken;
    store.getState().requestWsReconnect();
    expect(store.getState().wsReconnectToken).toBe(token);
  });

  it("applies every one of them while the socket is not lost", () => {
    const store = originStore(false);
    const token = store.getState().wsReconnectToken;
    store.getState().applySnapshot(snapshot("successor", 99));
    store.getState().applyEventsBatch([event(100, "next")]);
    store.getState().setRunStatus("finished");
    store.getState().requestWsReconnect();
    const st = store.getState();
    expect(st.snapshot?.run.name).toBe("successor");
    expect(st.events.map((e) => e.seq)).toContain(100);
    expect(st.snapshot?.run.status).toBe("finished");
    expect(st.wsReconnectToken).toBe(token + 1);
  });
});
