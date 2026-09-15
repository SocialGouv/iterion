// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ExecutionState } from "@/api/runs";
import { readReferenceDrop, REFERENCE_MIME } from "@/lib/chatDock/dragReference";

import { DetailHeader } from "./DetailHeader";

vi.mock("@/lib/runChat/useNodeLabel", () => ({
  useNodeLabel: () => (id: string) => `Label ${id}`,
}));

// Import the real header controls directly: the chrome barrel also pulls an
// unrelated Lobe ESM package that cannot be loaded by the test runner.
vi.mock("@/components/ui", async () => ({
  ...await import("@/components/ui/IconButton"),
  ...await import("@/components/ui/CopyButton"),
  ...await import("@/components/ui/StatusBadge"),
  ...await import("@/components/ui/LiveDot"),
  ...await import("@/components/ui/Popover"),
}));
// Iteration colors do not require mounting the canvas and its provider icons.
vi.mock("../IRNode", () => ({ iterationColor: () => "var(--color-iteration-0)" }));

afterEach(cleanup);

function fakeDataTransfer(): DataTransfer {
  const store = new Map<string, string>();
  return {
    setData: (type: string, value: string) => void store.set(type, value),
    getData: (type: string) => store.get(type) ?? "",
    get types() { return Array.from(store.keys()); },
    effectAllowed: "uninitialized",
  } as unknown as DataTransfer;
}

function execution(id: string, attempt = 0): ExecutionState {
  return {
    execution_id: `${id}-${attempt}`, ir_node_id: id, branch_id: "root",
    loop_iteration: attempt, status: "finished", kind: "llm",
    current_event_seq: 2, first_seq: 1, last_seq: 2,
    started_at: "2026-09-13T10:00:00Z", finished_at: "2026-09-13T10:00:02Z",
  };
}

describe("DetailHeader node references", () => {
  it("drags the currently displayed run and node through the real reference protocol", () => {
    const onToggleFollowLive = vi.fn();
    const onSelectIteration = vi.fn();
    const first = execution("plan_review");
    const props = {
      runId: "01a09afe-92d7-7235-b9bd-892ac04caea7", exec: first,
      executions: [first, execution(first.ir_node_id, 1)], selectedIteration: 0,
      onSelectIteration, events: [], onToggleFollowLive, followLive: true,
      filePath: "bots/consumer/main.bot",
    };
    const { rerender } = render(<DetailHeader {...props} />);
    function assertDrag(runId: string, nodeId: string) {
      const dt = fakeDataTransfer();
      const handle = screen.getByRole("img", { name: "Drag node to assistant" });
      expect(handle.getAttribute("draggable")).toBe("true");
      fireEvent.dragStart(handle, { dataTransfer: dt });
      expect(JSON.parse(dt.getData(REFERENCE_MIME))).toEqual({ kind: "node", id: `${runId}/${nodeId}`, label: `Label ${nodeId}` });
      expect(dt.getData("text/plain")).toBe(`node/${runId}/${nodeId}`);
      expect(dt.effectAllowed).toBe("copy");
      expect(readReferenceDrop(dt)).toEqual({ kind: "node", ref: `node/${runId}/${nodeId}`, label: `Label ${nodeId}` });
      expect(screen.getByText(nodeId).closest("[draggable]")).toBeNull();
    }
    assertDrag(props.runId, first.ir_node_id);
    fireEvent.click(screen.getByRole("button", { name: "live" }));
    expect(onToggleFollowLive).toHaveBeenCalledOnce();
    fireEvent.click(screen.getByRole("button", { name: "2" }));
    expect(onSelectIteration).toHaveBeenCalledWith(first.ir_node_id, 1);
    const timeToggle = screen.getByRole("button", { name: "duration: 2s" });
    const previous = timeToggle.getAttribute("aria-pressed");
    fireEvent.click(timeToggle);
    expect(timeToggle.getAttribute("aria-pressed")).not.toBe(previous);
    expect(screen.getByRole("button", { name: "Open in editor" }).closest("[draggable]")).toBeNull();
    const second = execution("repair/verify-2");
    const nextRun = "01a09b00-1234-7235-b9bd-892ac04caea8";
    rerender(<DetailHeader {...props} runId={nextRun} exec={second} executions={[second]} />);
    assertDrag(nextRun, second.ir_node_id);
  });

  it("does not offer a drag that would mint a different or invalid pointer", () => {
    const exec = execution("invalid]node");
    render(<DetailHeader runId="01a09afe-92d7-7235-b9bd-892ac04caea7" exec={exec} executions={[exec]} selectedIteration={0} onSelectIteration={() => {}} events={[]} />);
    expect(screen.queryByRole("img", { name: "Drag node to assistant" })).toBeNull();
    expect(screen.getByText(exec.ir_node_id)).toBeTruthy();
  });
});
