// @vitest-environment jsdom
//
// A fork answers what it decided that the caller must see in `notes` — a
// key pin of the parent it dropped because the key no longer exists (the
// child then spends the team's current key), a typed input admitted without a
// pre-check. The dialog shows each one: a fork that changed which key funds
// the child must not read as a plain "Forked →".
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const runs = vi.hoisted(() => ({ forkRun: vi.fn() }));
vi.mock("@/api/runs", () => runs);
vi.mock("wouter", () => ({ useLocation: () => ["/", vi.fn()] }));

import type { RunHeader } from "@/api/runs";
import { useUIStore } from "@/store/ui";

import ForkDialog from "./ForkDialog";

const run = { id: "run-parent", status: "finished", name: "Parent", workflow_name: "wf" } as unknown as RunHeader;

afterEach(() => {
  cleanup();
  useUIStore.setState({ toasts: [] });
  vi.clearAllMocks();
});

function forkWith(notes: string[] | undefined) {
  runs.forkRun.mockResolvedValueOnce({ new_run_id: "run-child", parent_run_id: "run-parent", notes });
  render(<ForkDialog run={run} anchor={{ nodeId: "review", turnIndex: 0 }} open onOpenChange={() => {}} />);
  fireEvent.click(screen.getByRole("button", { name: /Fork/ }));
}

describe("ForkDialog shows what the fork decided", () => {
  it("shows every note the fork answered, as a warning that stays", async () => {
    const note = 'key pin openai_compatible=key-gw dropped: the key no longer exists — the child spends the team\'s current key';
    forkWith([note]);
    await waitFor(() => expect(runs.forkRun).toHaveBeenCalled());
    await waitFor(() => {
      const toasts = useUIStore.getState().toasts;
      expect(toasts.some((t) => t.message.includes("run-child") && t.type === "success")).toBe(true);
      expect(toasts.some((t) => t.message.includes(note) && t.type === "warning" && t.persistent)).toBe(true);
    });
  });

  it("adds nothing when the fork answered no note", async () => {
    forkWith(undefined);
    await waitFor(() => expect(useUIStore.getState().toasts.some((t) => t.type === "success")).toBe(true));
    expect(useUIStore.getState().toasts.filter((t) => t.type === "warning")).toHaveLength(0);
  });
});
