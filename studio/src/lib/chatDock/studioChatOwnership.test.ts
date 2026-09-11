// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";

import type { RunSummary } from "@/api/runs";

import {
  STUDIO_CHAT_CLIENT_KEY,
  planStudioChatReconciliation,
  readOrCreateStudioChatClientId,
} from "./studioChatOwnership";

const now = Date.parse("2026-08-29T12:00:00Z");
const run = (
  id: string,
  conversationId: string,
  overrides: Partial<RunSummary> = {},
): RunSummary => ({
  id,
  workflow_name: "copilot",
  status: "paused_waiting_human",
  active: false,
  created_at: "2026-08-29T11:00:00Z",
  updated_at: "2026-08-29T11:00:00Z",
  source_kind: "studio_chat",
  source: {
    kind: "studio_chat",
    client_id: "client-a",
    conversation_id: conversationId,
  },
  ...overrides,
});

beforeEach(() => localStorage.clear());

describe("Studio chat client identity", () => {
  it("is stable for one browser profile", () => {
    const first = readOrCreateStudioChatClientId();
    expect(first).toBeTruthy();
    expect(localStorage.getItem(STUDIO_CHAT_CLIENT_KEY)).toBe(first);
    expect(readOrCreateStudioChatClientId()).toBe(first);
  });
});

describe("Studio chat reconciliation", () => {
  it("repairs a missing run id from source.conversation_id", () => {
    const plan = planStudioChatReconciliation(
      [run("run-1", "tab-1")],
      [{ id: "tab-1", botId: "copilot" }],
      "client-a",
      now,
    );
    expect(plan.repairs).toEqual([{ conversationId: "tab-1", runId: "run-1" }]);
    expect(plan.cancelRunIds).toEqual([]);
  });

  it("keeps the explicit owner and cancels duplicate and orphan runs", () => {
    const plan = planStudioChatReconciliation(
      [run("owned", "tab-1"), run("duplicate", "tab-1"), run("orphan", "gone")],
      [{ id: "tab-1", botId: "copilot", runId: "owned" }],
      "client-a",
      now,
    );
    expect(plan.repairs).toEqual([]);
    expect(plan.cancelRunIds.sort()).toEqual(["duplicate", "orphan"]);
  });

  it("ignores manual, terminal, historical, and foreign-profile runs", () => {
    const foreign = run("foreign", "gone", {
      source: { kind: "studio_chat", client_id: "client-b", conversation_id: "gone" },
    });
    const manual = run("manual", "gone", { source_kind: "manual", source: undefined });
    const terminal = run("finished", "gone", { status: "finished" });
    const historical = run("historical", "gone", { source: undefined });
    const plan = planStudioChatReconciliation(
      [foreign, manual, terminal, historical],
      [],
      "client-a",
      now,
    );
    expect(plan).toEqual({ repairs: [], cancelRunIds: [], retryAfterMs: null });
  });

  it("repairs terminal history without trying to cancel it", () => {
    const plan = planStudioChatReconciliation(
      [run("finished", "tab", { status: "finished" })],
      [{ id: "tab", botId: "copilot" }],
      "client-a",
      now,
    );
    expect(plan.repairs).toEqual([{ conversationId: "tab", runId: "finished" }]);
    expect(plan.cancelRunIds).toEqual([]);
  });

  it("defers a young orphan until one bounded follow-up scan", () => {
    const young = run("young", "gone", { created_at: "2026-08-29T11:59:30Z" });
    const plan = planStudioChatReconciliation([young], [], "client-a", now);
    expect(plan.cancelRunIds).toEqual([]);
    expect(plan.retryAfterMs).toBe(30_000);
  });

  it("reaps every non-terminal state, including running and resumable", () => {
    const statuses = [
      "queued",
      "running",
      "paused_waiting_human",
      "paused_operator",
      "failed_resumable",
    ] as const;
    const plan = planStudioChatReconciliation(
      statuses.map((status) => run(status, status, { status })),
      [],
      "client-a",
      now,
    );
    expect(plan.cancelRunIds.sort()).toEqual([...statuses].sort());
  });
});
