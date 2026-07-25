// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import HumanPromptForm from "./HumanPromptForm";

const getRunWorkflow = vi.fn();
const getRun = vi.fn().mockResolvedValue({});
const resumeRun = vi.fn().mockResolvedValue({ run_id: "run-1", status: "running" });

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

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("HumanPromptForm — schema fetch failure (iterion#244)", () => {
  it("surfaces the load error + a Retry instead of silently dropping to a verdict-less fallback", async () => {
    getRunWorkflow.mockRejectedValue(new Error("load workflow: cannot read file"));

    render(
      <HumanPromptForm
        runId="run-1"
        nodeId="style_kit_review"
        // The node's resolved INPUT lands here; the verdict lives in the
        // OUTPUT schema, which failed to load. The old behaviour rendered
        // these input fields (or an empty "Resume" form) — see the bug.
        questions={{ boards: "candidate boards", detail: "the detail" }}
        sourceOverride={null}
      />,
    );

    // The error is surfaced, naming the cause…
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(
        /Couldn't load the answer form/i,
      ),
    );
    // …and a Retry affordance is offered.
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    // The misleading fallback is NOT shown: no empty-questions "Resume"
    // form, and the raw input fields are not presented as the answer.
    expect(
      screen.queryByText(/paused without specific questions/i),
    ).toBeNull();
    // No answer ever left the client on this failed load.
    expect(resumeRun).not.toHaveBeenCalled();

    // Retry re-fetches the schema.
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(getRunWorkflow).toHaveBeenCalledTimes(2));
  });

  it("still answers the verdict when the schema loads (regression)", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "style_kit_review",
          output_schema: [
            { name: "approved", type: "bool" },
            { name: "notes", type: "string" },
          ],
        },
      ],
      stale_hash: false,
    });

    render(
      <HumanPromptForm
        runId="run-1"
        nodeId="style_kit_review"
        questions={{}}
        sourceOverride={null}
      />,
    );

    const reject = await waitFor(() =>
      screen.getByRole("button", { name: "Request changes" }),
    );
    fireEvent.change(screen.getByRole("textbox"), {
      target: { value: "spacing is off" },
    });
    fireEvent.click(reject);

    await waitFor(() =>
      expect(resumeRun).toHaveBeenCalledWith(
        "run-1",
        expect.objectContaining({
          answers: { approved: false, notes: "spacing is off" },
        }),
      ),
    );
  });

  it("hides routing jargon and fills the approval target automatically", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "plan_review",
          output_schema: [
            { name: "approved", type: "bool" },
            {
              name: "rework_target",
              type: "string",
              enum_values: ["none", "plan", "concept"],
            },
            { name: "reviewer", type: "string" },
            { name: "notes", type: "string" },
          ],
        },
      ],
      stale_hash: false,
    });

    render(
      <HumanPromptForm
        runId="run-1"
        nodeId="plan_review"
        questions={{}}
        sourceOverride={null}
      />,
    );

    await waitFor(() =>
      expect(screen.getByText("What should be revised?")).toBeTruthy(),
    );
    expect(screen.queryByText("none")).toBeNull();
    expect(screen.getByText("The plan")).toBeTruthy();
    expect(screen.getByText("The visual concept")).toBeTruthy();
    expect(screen.getByText("Your name")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(resumeRun).toHaveBeenCalledWith(
        "run-1",
        expect.objectContaining({
          answers: expect.objectContaining({
            approved: true,
            rework_target: "none",
          }),
        }),
      ),
    );
  });

  it("requires a concrete correction target before requesting changes", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "plan_review",
          output_schema: [
            { name: "approved", type: "bool" },
            {
              name: "rework_target",
              type: "string",
              enum_values: ["none", "plan", "concept"],
            },
          ],
        },
      ],
      stale_hash: false,
    });

    render(
      <HumanPromptForm
        runId="run-1"
        nodeId="plan_review"
        questions={{}}
        sourceOverride={null}
      />,
    );

    const requestChanges = await waitFor(() =>
      screen.getByRole("button", { name: "Request changes" }),
    );
    fireEvent.click(requestChanges);
    expect(screen.getByRole("alert").textContent).toMatch(
      /Choose what should be revised/,
    );
    expect(resumeRun).not.toHaveBeenCalled();

    fireEvent.click(screen.getByText("The plan"));
    fireEvent.click(requestChanges);
    await waitFor(() =>
      expect(resumeRun).toHaveBeenCalledWith(
        "run-1",
        expect.objectContaining({
          answers: expect.objectContaining({
            approved: false,
            rework_target: "plan",
          }),
        }),
      ),
    );
  });

  it("keeps a terminal rejection available for workflows where none is meaningful", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "concept_review",
          output_schema: [
            { name: "approved", type: "bool" },
            {
              name: "rework_target",
              type: "string",
              enum_values: ["none", "contract", "images"],
            },
          ],
        },
      ],
      stale_hash: false,
    });

    render(
      <HumanPromptForm
        runId="run-1"
        nodeId="concept_review"
        questions={{}}
        sourceOverride={null}
      />,
    );

    fireEvent.click(
      await screen.findByText("Reject without another revision"),
    );
    fireEvent.click(screen.getByRole("button", { name: "Reject" }));

    await waitFor(() =>
      expect(resumeRun).toHaveBeenCalledWith(
        "run-1",
        expect.objectContaining({
          answers: expect.objectContaining({
            approved: false,
            rework_target: "none",
          }),
        }),
      ),
    );
  });

  it("humanizes a large rework selector without dropping its none option", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "asset_review",
          output_schema: [
            { name: "approved", type: "bool" },
            {
              name: "rework_target",
              type: "string",
              enum_values: [
                "none",
                "body_geometry",
                "skeleton",
                "weights",
                "export",
              ],
            },
          ],
        },
      ],
      stale_hash: false,
    });

    render(
      <HumanPromptForm
        runId="run-1"
        nodeId="asset_review"
        questions={{}}
        sourceOverride={null}
      />,
    );

    expect(await screen.findByText("What should be revised?")).toBeTruthy();
    expect(
      screen.getByRole("option", {
        name: "Reject without another revision",
      }),
    ).toBeTruthy();
    expect(
      screen.getByRole("option", { name: "Body geometry" }),
    ).toBeTruthy();
  });
});
