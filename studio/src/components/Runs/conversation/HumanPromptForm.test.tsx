// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import HumanPromptForm from "./HumanPromptForm";

// useHumanNodeSchema fetches the run workflow via react-query, so every
// render needs a client. retry:false makes the error path settle on the
// first rejection instead of retrying.
function renderWithClient(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

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

    renderWithClient(
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

    renderWithClient(
      <HumanPromptForm
        runId="run-1"
        nodeId="style_kit_review"
        questions={{}}
        sourceOverride={null}
      />,
    );

    const reject = await waitFor(() =>
      screen.getByRole("button", { name: "Reject" }),
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
});
