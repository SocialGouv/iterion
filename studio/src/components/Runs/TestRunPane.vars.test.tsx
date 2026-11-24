// @vitest-environment jsdom
//
// The Test pane launches a bot by id, with no pin, from the session's picture
// of its vars (rva round 2, S2). It posts what the operator TOUCHED: a
// pre-filled default echoed back would override a default the program changed
// since, be refused for a var it dropped, and break a default the server
// expands.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

const runsApi = vi.hoisted(() => ({ createRun: vi.fn() }));
vi.mock("@/api/runs", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/runs")>()),
  createRun: runsApi.createRun,
}));
vi.mock("wouter", async (importOriginal) => ({
  ...(await importOriginal<typeof import("wouter")>()),
  useLocation: () => ["/bots/nestor", vi.fn()],
}));

import type { VarField } from "@/api/types";

import TestRunPane from "./TestRunPane";

const str = (v: string) => ({ kind: "string" as const, str_val: v, raw: JSON.stringify(v) });
const vars: VarField[] = [
  { name: "env", type: "string", default: str("session-old-default") },
  { name: "dir", type: "string", default: str("${PROJECT_DIR}/out") },
  { name: "ticket", type: "string" },
  // No declared default: the server's own default for the type is its to
  // apply, so the untouched field is not a value either.
  { name: "dry_run", type: "bool" },
];

function mount() {
  runsApi.createRun.mockResolvedValue({ run_id: "run-1" });
  render(
    <QueryClientProvider client={new QueryClient()}>
      <TestRunPane launch={{ by: "bot", bot: "nestor" }} vars={vars} />
    </QueryClientProvider>,
  );
}
const input = (value: string) => screen.getByDisplayValue(value) as HTMLInputElement;
const posted = () => runsApi.createRun.mock.calls[0]?.[0] as Record<string, unknown>;

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("the Test pane's launch by id", () => {
  it("posts no var the operator did not touch", async () => {
    mount();
    // The fields are pre-filled with the defaults the session knows.
    expect(input("session-old-default")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Test" }));
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted()).toMatchObject({ bot_id: "nestor", merge_into: "none" });
    expect(posted()).not.toHaveProperty("vars");
  });

  it("posts the one it did, and that one alone", async () => {
    mount();
    fireEvent.change(input("session-old-default"), { target: { value: "staging" } });
    fireEvent.click(screen.getByRole("button", { name: "Test" }));
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted().vars).toEqual({ env: "staging" });
  });
});
