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

import { ApiError } from "@/api/client";
import { WORKFLOW_SOURCE_CHANGED_ERROR_CODE } from "@/api/runs";

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

vi.mock("@/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/runs")>();
  return {
    ...actual,
    getRun: (...args: unknown[]) => getRun(...args),
    getRunWorkflow: (...args: unknown[]) => getRunWorkflow(...args),
    resumeRun: (...args: unknown[]) => resumeRun(...args),
  };
});

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

function sourceChangedError() {
  return new ApiError(
    400,
    "API error 400: resume rejected",
    WORKFLOW_SOURCE_CHANGED_ERROR_CODE,
  );
}

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

  it("replays the exact approval answer with force after a stale-workflow rejection", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "approve_audio",
          output_schema: [
            { name: "approved", type: "bool" },
            { name: "reviewer", type: "string" },
            { name: "notes", type: "string" },
          ],
        },
      ],
      stale_hash: true,
    });
    resumeRun
      .mockRejectedValueOnce(
        new Error(
          'runtime: workflow source has changed since run "run-1" was started',
        ),
      )
      .mockResolvedValueOnce({ run_id: "run-1", status: "running" });

    renderWithClient(
      <HumanPromptForm
        runId="run-1"
        nodeId="approve_audio"
        questions={{}}
        sourceOverride={null}
      />,
    );

    const textboxes = await screen.findAllByRole("textbox");
    expect(textboxes).toHaveLength(2);
    const [reviewer, notes] = textboxes;
    if (!reviewer || !notes) {
      throw new Error("reviewer and notes fields were not rendered");
    }
    fireEvent.change(reviewer, { target: { value: "Victor" } });
    fireEvent.change(notes, {
      target: { value: "Corriger la formulation de la scène 6." },
    });
    fireEvent.click(screen.getByRole("button", { name: "Reject" }));

    const answers = {
      approved: false,
      reviewer: "Victor",
      notes: "Corriger la formulation de la scène 6.",
    };
    await waitFor(() =>
      expect(resumeRun).toHaveBeenNthCalledWith(1, "run-1", {
        answers,
        source: undefined,
      }),
    );

    fireEvent.click(
      await screen.findByRole("button", {
        name: "Resume with updated workflow (force)",
      }),
    );

    await waitFor(() =>
      expect(resumeRun).toHaveBeenNthCalledWith(2, "run-1", {
        answers,
        source: undefined,
        force: true,
      }),
    );
  });
});

describe("HumanPromptForm force resume", () => {
  it("re-coerces the current wizard draft when force retry is clicked", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "review",
          output_schema: [{ name: "notes", type: "string" }],
        },
      ],
      stale_hash: false,
    });
    resumeRun
      .mockRejectedValueOnce(sourceChangedError())
      .mockResolvedValueOnce({ run_id: "run-1", status: "running" });

    renderWithClient(
      <HumanPromptForm
        runId="run-1"
        nodeId="review"
        questions={{}}
        sourceOverride={null}
      />,
    );

    const notes = await waitFor(() => screen.getByRole("textbox"));
    fireEvent.change(notes, { target: { value: "before rejection" } });
    fireEvent.click(
      screen.getByRole("button", { name: "Submit & Resume" }),
    );

    const force = await screen.findByRole("button", {
      name: "Resume with updated workflow (force)",
    });
    fireEvent.change(notes, { target: { value: "edited after rejection" } });
    fireEvent.click(force);

    await waitFor(() => expect(resumeRun).toHaveBeenCalledTimes(2));
    expect(resumeRun).toHaveBeenNthCalledWith(2, "run-1", {
      answers: { notes: "edited after rejection" },
      source: undefined,
      force: true,
    });
  });

  it("preserves the rejected verdict while merging the current editable fields", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "review",
          output_schema: [
            { name: "approved", type: "bool" },
            { name: "notes", type: "string" },
          ],
        },
      ],
      stale_hash: false,
    });
    resumeRun
      .mockRejectedValueOnce(sourceChangedError())
      .mockResolvedValueOnce({ run_id: "run-1", status: "running" });

    renderWithClient(
      <HumanPromptForm
        runId="run-1"
        nodeId="review"
        questions={{}}
        sourceOverride={null}
      />,
    );

    const notes = await waitFor(() => screen.getByRole("textbox"));
    fireEvent.change(notes, { target: { value: "initial feedback" } });
    fireEvent.click(screen.getByRole("button", { name: "Reject" }));

    const force = await screen.findByRole("button", {
      name: "Resume with updated workflow (force)",
    });
    fireEvent.change(notes, { target: { value: "updated feedback" } });
    fireEvent.click(force);

    await waitFor(() => expect(resumeRun).toHaveBeenCalledTimes(2));
    expect(resumeRun).toHaveBeenNthCalledWith(2, "run-1", {
      answers: { approved: false, notes: "updated feedback" },
      source: undefined,
      force: true,
    });
  });

  it("preserves the rejected quick-action token while merging the current wizard fields", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "review",
          output_schema: [
            { name: "response", type: "string" },
            { name: "notes", type: "string" },
          ],
        },
      ],
      stale_hash: false,
    });
    resumeRun
      .mockRejectedValueOnce(sourceChangedError())
      .mockResolvedValueOnce({ run_id: "run-1", status: "running" });

    renderWithClient(
      <HumanPromptForm
        runId="run-1"
        nodeId="review"
        questions={{}}
        sourceOverride={null}
      />,
    );

    const quickSkip = await waitFor(() =>
      screen.getByTitle(
        "Submit a skip token; the bot will route accordingly.",
      ),
    );
    fireEvent.click(quickSkip);

    const force = await screen.findByRole("button", {
      name: "Resume with updated workflow (force)",
    });
    fireEvent.click(screen.getByRole("button", { name: "Next →" }));
    fireEvent.change(screen.getByRole("textbox"), {
      target: { value: "current side note" },
    });
    fireEvent.click(force);

    await waitFor(() => expect(resumeRun).toHaveBeenCalledTimes(2));
    expect(resumeRun).toHaveBeenNthCalledWith(2, "run-1", {
      answers: {
        response: "[QA:skip]",
        notes: "current side note",
      },
      source: undefined,
      force: true,
    });
  });
});

describe("HumanPromptForm — required side questions", () => {
  it("validates every required side question before an external action verdict", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "recovery_gate",
          output_schema: [
            {
              name: "action",
              type: "string",
              enum_values: ["retry", "abort"],
            },
            { name: "reviewer_id", type: "string" },
            {
              name: "target",
              type: "string",
              enum_values: ["pipeline", "data"],
            },
          ],
        },
      ],
      stale_hash: false,
    });

    renderWithClient(
      <HumanPromptForm
        runId="run-1"
        nodeId="recovery_gate"
        questions={{}}
        sourceOverride={null}
      />,
    );

    const retry = await screen.findByRole("button", { name: "Retry" });
    expect(screen.getByText("Reviewer ID")).toBeTruthy();
    fireEvent.click(retry);
    expect(screen.getByRole("alert").textContent).toMatch(
      /Complete required fields: Reviewer ID, Target/,
    );
    expect(resumeRun).not.toHaveBeenCalled();

    fireEvent.change(screen.getByPlaceholderText("Required"), {
      target: { value: "operator-42" },
    });
    fireEvent.click(screen.getByText("pipeline"));
    fireEvent.click(retry);

    await waitFor(() =>
      expect(resumeRun).toHaveBeenCalledWith(
        "run-1",
        expect.objectContaining({
          answers: {
            action: "retry",
            reviewer_id: "operator-42",
            target: "pipeline",
          },
        }),
      ),
    );
  });
});

describe("HumanPromptForm — operator uploads at a gate", () => {
  // The 📎 affordance is unconditional: it must be there for the gate
  // whose author never anticipated an attachment, which is most of them.
  it("offers an attach-a-file button on a schema-driven gate", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "review",
          output_schema: [{ name: "feedback", type: "string" }],
        },
      ],
      stale_hash: false,
    });

    renderWithClient(
      <HumanPromptForm
        runId="run-1"
        nodeId="review"
        questions={{}}
        sourceOverride={null}
      />,
    );

    expect(
      await screen.findByRole("button", { name: /Attach a file/i }),
    ).toBeTruthy();
  });

  it("renders a drop zone for a declared `file` field", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "gate_music",
          output_schema: [
            { name: "music", type: "file" },
            { name: "notes", type: "string" },
          ],
        },
      ],
      stale_hash: false,
    });

    renderWithClient(
      <HumanPromptForm
        runId="run-1"
        nodeId="gate_music"
        questions={{}}
        sourceOverride={null}
      />,
    );

    // The declared field gets its own picker, distinct from the generic
    // 📎 button below the form.
    expect(await screen.findByLabelText(/Upload Music/i)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: /Attach a file/i }),
    ).toBeTruthy();
  });

  // A gate with no upload must still resume with no `attachments` key at
  // all — an empty array would send every ordinary answer down the
  // promotion path for nothing.
  it("omits attachments from the resume when nothing was attached", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [
        {
          id: "review",
          output_schema: [{ name: "feedback", type: "string" }],
        },
      ],
      stale_hash: false,
    });

    renderWithClient(
      <HumanPromptForm
        runId="run-1"
        nodeId="review"
        questions={{}}
        sourceOverride={null}
      />,
    );

    const submit = await screen.findByRole("button", {
      name: /Submit & Resume/i,
    });
    fireEvent.click(submit);

    await waitFor(() => expect(resumeRun).toHaveBeenCalled());
    const [, req] = resumeRun.mock.calls[0] as [string, Record<string, unknown>];
    expect("attachments" in req).toBe(false);
  });
  // A board surface has no conversation around the form, so the paused
  // node's `instructions:` text is the ONLY place the operator's question
  // can appear: `questions` holds the node's INBOUND data and the
  // schema-driven form renders its OUTPUT fields. Regression: an
  // app-concept interview gate showed a bare "Message" box on the card
  // while the run console rendered the full question.
  it("renders the resolved instructions above the form", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [{ id: "chat", output_schema: [{ name: "message", type: "string" }] }],
      stale_hash: false,
    });

    renderWithClient(
      <HumanPromptForm
        runId="run-1"
        nodeId="chat"
        questions={{ reply: "inbound data the form never renders" }}
        instructions={"Which scope do you want?\n\n- **A** narrow\n- **B** broad"}
        sourceOverride={null}
      />,
    );

    // findByText throws when absent, so reaching the assertion is the
    // proof; the tag check pins that markdown rendered (a list item),
    // not a raw string dumped into a paragraph.
    const heading = await screen.findByText(/Which scope do you want\?/);
    expect(heading).toBeTruthy();
    expect(screen.getByText("narrow").closest("li")).toBeTruthy();
  });

  it("renders no instructions block when the node declares none", async () => {
    getRunWorkflow.mockResolvedValue({
      nodes: [{ id: "chat", output_schema: [{ name: "message", type: "string" }] }],
      stale_hash: false,
    });

    const { container } = renderWithClient(
      <HumanPromptForm
        runId="run-1"
        nodeId="chat"
        questions={{ reply: "inbound only" }}
        sourceOverride={null}
      />,
    );

    await screen.findByRole("textbox");
    expect(container.textContent).not.toContain("inbound only");
  });
});
