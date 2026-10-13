// @vitest-environment jsdom
//
// A launch names its program: the inline text of the editor buffer it
// renders, or — outside cloud — the ?file= path the server reads itself. The
// form is up before the inline text is: that render is a round trip away
// (useLaunchDoc), and the document it renders is the ACTIVE editor tab's, so
// the sidebar's tab list elects another one and starts the round trip again
// without leaving the route — its × reroutes only the tab on screen, and
// under /runs/new that is never an editor tab (NavLinks'
// closeTabAndReroute). Submitting in that window would post an
// empty file_path with no source, and the server's refusal names fields this
// form never showed ("file_path, source or bot_id is required",
// pkg/server/runs_launch.go). In CLOUD the path is no better: the pod has no
// operator filesystem, so a bare file_path is either refused the same way
// ("cloud mode: source or a catalog bot_id is required") or resolved to a
// catalog bot's own bytes through the tiered store — another program under
// this form's fields. The gate is on launchRun, the one place every entry
// point passes: the Launch button and Enter in any var field go through
// onSubmit, while the no-sandbox dialog's Confirm — armed when the program
// was still there — calls launchRun itself.
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const runsApi = vi.hoisted(() => ({ createRun: vi.fn() }));
vi.mock("@/api/runs", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/runs")>()),
  createRun: runsApi.createRun,
}));

import type { ServerInfo } from "@/api/types";
import { createEmptyDocument } from "@/lib/defaults";

import { emptyBudgetFieldValues } from "./budgetPayload";
import type { UseRepoTargetResult } from "./useRepoTarget";
import type { UseRunOverridesResult } from "./useRunOverrides";
import { useLaunchSubmit } from "./useLaunchSubmit";

// Only what the launch path reads: no repo target to resolve, no override
// set. Everything the gate itself reads is passed for real.
const noRepoTarget = { setRepoTargetCreateError: () => {} } as unknown as UseRepoTargetResult;
const noOverrides = {
  modelOverrides: {},
  budgetFields: emptyBudgetFieldValues,
} as unknown as UseRunOverridesResult;
const uploadLimits = {
  max_file_size: 1,
  max_total_size: 1,
  max_files_per_run: 1,
  allowed_mime: [],
};
// work_dir is empty in cloud — there is no operator folder to resolve a path
// against, which is the whole reason a cloud path names no program.
const cloudInfo: ServerInfo = {
  mode: "cloud",
  version: "test",
  work_dir: "",
  limits: { upload: uploadLimits },
};
const localInfo: ServerInfo = {
  mode: "local",
  version: "test",
  work_dir: "/workspace/project",
  limits: { upload: uploadLimits },
};

function Form({
  filePath,
  source,
  serverInfo = null,
}: {
  filePath: string;
  source: string | null;
  serverInfo?: ServerInfo | null;
}) {
  const [error, setError] = useState<string | null>(null);
  const { onSubmit, launchRun } = useLaunchSubmit({
    filePath,
    // The last loaded document, still on screen: what makes the form
    // submittable while the program it would send is missing.
    doc: createEmptyDocument(),
    currentSource: source,
    confirmedDiskPath: null,
    serverInfo,
    fields: [],
    values: {},
    attachmentFields: [],
    attachments: {},
    selectedPreset: "",
    selectedPresetMeta: undefined,
    repoTarget: noRepoTarget,
    overrides: noOverrides,
    setError,
  });
  return (
    <>
      <button onClick={onSubmit}>Launch</button>
      {/* What NoSandboxConfirmDialog's onConfirm does: launchRun, past
          onSubmit, on a form the operator armed some time ago. */}
      <button onClick={() => void launchRun()}>Confirm</button>
      <output aria-label="error">{error ?? ""}</output>
    </>
  );
}

const submit = () => fireEvent.click(screen.getByRole("button", { name: "Launch" }));
const confirm = () => fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
const error = () => screen.getByLabelText("error").textContent;

beforeEach(() => {
  runsApi.createRun.mockReset();
  runsApi.createRun.mockResolvedValue({ run_id: "run-1" });
});

afterEach(cleanup);

describe("a submit of the inline form before its program arrived", () => {
  it("sends no run and says the workflow is not ready", () => {
    render(<Form filePath="" source={null} />);

    submit();

    expect(runsApi.createRun).not.toHaveBeenCalled();
    expect(error()).toMatch(/still being prepared/);
  });

  // The window closes by itself when /api/unparse answers; the same click
  // then launches. Guards that refuse the legitimate case are worse than
  // the window they close.
  it("launches as soon as the render is there", () => {
    render(<Form filePath="" source={"workflow x:\n  entry: done\n"} />);

    submit();

    expect(runsApi.createRun).toHaveBeenCalledTimes(1);
    expect(runsApi.createRun.mock.calls[0]?.[0]).toMatchObject({
      file_path: "",
      source: "workflow x:\n  entry: done\n",
    });
    expect(error()).toBe("");
  });

  // The no-sandbox dialog is armed by a form that had its program and
  // confirmed later: it reaches launchRun without passing onSubmit again.
  it("sends no run when the dialog is confirmed after the program went", () => {
    render(<Form filePath="" source={null} />);

    confirm();

    expect(runsApi.createRun).not.toHaveBeenCalled();
    expect(error()).toMatch(/still being prepared/);
  });

  // A ?file= launch names its program by path and sends no source at all in
  // the confirmed-disk case: the gate must not read "no source" as "no
  // program".
  it("launches a ?file= path with no inline source", () => {
    render(<Form filePath="bots/x.bot" source={null} serverInfo={localInfo} />);

    submit();

    expect(runsApi.createRun).toHaveBeenCalledTimes(1);
    expect(runsApi.createRun.mock.calls[0]?.[0]).toMatchObject({ file_path: "bots/x.bot" });
    expect(error()).toBe("");
  });
});

// In cloud the ?file= form is in the same position as the inline one: its
// source is the whole program, and until it is there the path names nothing
// the pod can read. The shape below is exactly what the studio's own ?bot=
// link builds (botPaths.ts), which is the shape the server infers a catalog
// bot id from — so the launch this gate lets through is not refused, it runs
// the tier's bot under this form's fields.
describe("a submit of a ?file= form in cloud before its source arrived", () => {
  it("sends no run for a catalog-shaped path", () => {
    render(<Form filePath="bots/x/main.bot" source={null} serverInfo={cloudInfo} />);

    submit();

    expect(runsApi.createRun).not.toHaveBeenCalled();
    expect(error()).toMatch(/still being prepared/);
  });

  it("sends no run through the no-sandbox dialog either", () => {
    render(<Form filePath="bots/x/main.bot" source={null} serverInfo={cloudInfo} />);

    confirm();

    expect(runsApi.createRun).not.toHaveBeenCalled();
    expect(error()).toMatch(/still being prepared/);
  });

  // The window closes when /files/open answers: cloud always sends the
  // source it read, and the gate must not stand in the way of that launch.
  it("launches as soon as the source is there", () => {
    render(
      <Form
        filePath="bots/x/main.bot"
        source={"workflow x:\n  entry: done\n"}
        serverInfo={cloudInfo}
      />,
    );

    submit();

    expect(runsApi.createRun).toHaveBeenCalledTimes(1);
    expect(runsApi.createRun.mock.calls[0]?.[0]).toMatchObject({
      file_path: "bots/x/main.bot",
      source: "workflow x:\n  entry: done\n",
    });
    expect(error()).toBe("");
  });
});
