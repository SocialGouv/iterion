// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { RunEvent } from "@/api/runs";

import { PauseTab } from "./PauseTab";

const getRunWorkflow = vi.fn();
const getRun = vi.fn().mockResolvedValue({});
const resumeRun = vi.fn();

vi.mock("@/api/runs", () => ({
  getRun: (...args: unknown[]) => getRun(...args),
  getRunWorkflow: (...args: unknown[]) => getRunWorkflow(...args),
  resumeRun: (...args: unknown[]) => resumeRun(...args),
}));

vi.mock("@/store/document", () => ({
  useDocumentStore: (select: (state: { currentSource: string | null }) => unknown) =>
    select({ currentSource: null }),
}));

vi.mock("@/store/run", () => ({
  useRunStore: (select: (state: Record<string, unknown>) => unknown) =>
    select({
      setRunStatus: vi.fn(),
      requestWsReconnect: vi.fn(),
      applySnapshot: vi.fn(),
      resyncEventsAfterResume: vi.fn(),
    }),
}));

function pauseEvent(data: Record<string, unknown>): RunEvent {
  return {
    seq: 1,
    timestamp: "2026-07-17T07:00:00Z",
    type: "human_input_requested",
    run_id: "run-1",
    node_id: "human_review",
    data,
  };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("PauseTab", () => {
  it("renders instructions read-only and answers from the human output schema", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "human_review",
          output_schema: [
            {
              name: "action",
              type: "string",
              enum_values: ["approve", "rework_images", "rework_contract"],
            },
            { name: "rework_asset_ids", type: "json" },
            { name: "notes", type: "string" },
          ],
        },
      ],
      stale_hash: false,
    });

    render(
      <PauseTab
        runId="run-1"
        nodeId="human_review"
        matching={[
          pauseEvent({
            instructions: "# Validate the humanoid\n\nInspect the generated images.",
            questions: {
              review_options: [
                { id: "human_male_body", title: "Body profile" },
                { id: "human_male_heads", title: "Head set" },
              ],
            },
          }),
        ]}
      />,
    );

    expect(screen.getByRole("heading", { name: "Validate the humanoid" })).toBeTruthy();
    expect(screen.getByText("Inspect the generated images.")).toBeTruthy();

    await waitFor(() => expect(screen.getByRole("button", { name: "Approve" })).toBeTruthy());
    expect(screen.getByRole("button", { name: "Rework images" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Rework contract" })).toBeTruthy();
    expect(screen.getByText("Rework asset ids")).toBeTruthy();
    expect(
      (screen.getByRole("checkbox", { name: /Body profile/ }) as HTMLInputElement)
        .checked,
    ).toBe(true);
    expect(
      (screen.getByRole("checkbox", { name: /Head set/ }) as HTMLInputElement)
        .checked,
    ).toBe(true);
    expect(screen.getByText("Notes")).toBeTruthy();
    expect(screen.queryByText("review_options")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Rework images" }));
    await waitFor(() =>
      expect(resumeRun).toHaveBeenCalledWith(
        "run-1",
        expect.objectContaining({
          answers: {
            action: "rework_images",
            rework_asset_ids: ["human_male_body", "human_male_heads"],
            notes: "",
          },
        }),
      ),
    );
  });

  it("keeps ask_user pauses on the questions-driven fallback", () => {
    getRunWorkflow.mockResolvedValue({ nodes: [], stale_hash: false });

    render(
      <PauseTab
        runId="run-1"
        nodeId="agent_step"
        matching={[
          pauseEvent({
            message: "The agent needs a choice.",
            questions: { ask_user_response: "Which variant should be used?" },
          }),
        ]}
      />,
    );

    expect(screen.getByText("The agent needs a choice.")).toBeTruthy();
    expect(screen.getByText("Which variant should be used?")).toBeTruthy();
    expect(screen.getByRole("textbox")).toBeTruthy();
  });
});
