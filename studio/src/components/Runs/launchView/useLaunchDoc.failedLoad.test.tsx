// @vitest-environment jsdom
//
// A load of the launch form's document that ends with NO answer must drop the
// previous one. Nothing retries it — the effect re-runs only when its deps
// move — so a document left in place keeps the form armed (`docReady` is
// `!!doc`, LaunchView) over a program the submit cannot carry. What this file
// asserts is the disarmed form and the cause on screen; what the disarmed
// affordances refuse is each witnessed at its own entry point
// (useLaunchSubmit.program, useLaunchSubmit.noSandboxConfirm).
//
// The reload is real: this route reads the ACTIVE editor tab's store
// (LaunchDocStoreProvider), and the sidebar's tab list closes an editor tab
// from any route — electing another one, and rerouting only when the closed
// tab was the one on screen, which under /runs/new it never is (NavLinks'
// closeTabAndReroute). The app-level chat dock activates one directly
// (EditorChangeOffer). Both triggers are driven below, each through the real
// tabs store. The hook then loads again — for the same ?file= path, or for
// another buffer on the inline branch — and the tag cannot tell those apart:
// the inline tag is always "", and a ?file= reload matches its own path.
//
// One scenario per failing branch, isolated: the inline render and the
// ?file= read each clear on their own, and a single scenario would leave one
// of them untested while the other covers for it.
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  openFile: vi.fn(),
  unparse: vi.fn(),
  getWorktreeConfig: vi.fn(),
}));
vi.mock("@/api/client", () => api);
const runsApi = vi.hoisted(() => ({ createRun: vi.fn(), previewRunCost: vi.fn() }));
vi.mock("@/api/runs", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/runs")>()),
  createRun: runsApi.createRun,
  previewRunCost: runsApi.previewRunCost,
}));

import type { IterDocument } from "@/api/types";
import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";

import { emptyBudgetFieldValues } from "./budgetPayload";
import LaunchDocStoreProvider from "./LaunchDocStoreProvider";
import type { UseRepoTargetResult } from "./useRepoTarget";
import type { UseRunOverridesResult } from "./useRunOverrides";
import { useLaunchDoc } from "./useLaunchDoc";
import { useLaunchSubmit } from "./useLaunchSubmit";

const noRepoTarget = { setRepoTargetCreateError: () => {} } as unknown as UseRepoTargetResult;
const noOverrides = {
  modelOverrides: {},
  budgetFields: emptyBudgetFieldValues,
} as unknown as UseRunOverridesResult;

// One var carrying a default, named after the document it belongs to: the
// form on screen says WHICH document the hook is describing, and a var with a
// default is not required — onSubmit's missing-required gate runs first and
// would stop the click before the program gate.
const docNamed = (name: string): IterDocument => ({
  ...createEmptyDocument(),
  vars: {
    fields: [
      { name, type: "string" as const, default: { kind: "string", raw: '"d"', str_val: "d" } },
    ],
  },
});

// The launch surface as LaunchView wires it: the Launch affordance is
// `!!doc`, and ONE error state carries both the hook's cause and the submit's
// refusal (LaunchView passes the same setError to each).
function Harness({ filePath }: { filePath: string }) {
  const [error, setError] = useState<string | null>(null);
  const { doc, currentSource, confirmedDiskPath, fields, values } = useLaunchDoc(
    filePath,
    setError,
  );
  const { onSubmit } = useLaunchSubmit({
    filePath,
    doc,
    currentSource,
    confirmedDiskPath,
    serverInfo: null,
    fields,
    values,
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
      <output aria-label="form">{doc ? fields.map((f) => f.name).join(",") : "LOADING"}</output>
      <output aria-label="program">{currentSource ?? "NO PROGRAM"}</output>
      <output aria-label="error">{error ?? ""}</output>
      <button onClick={onSubmit} disabled={!doc}>
        Launch
      </button>
    </>
  );
}

const form = () => screen.getByLabelText("form").textContent;
const program = () => screen.getByLabelText("program").textContent;
const error = () => screen.getByLabelText("error").textContent;
const launchDisabled = () =>
  (screen.getByRole("button", { name: "Launch" }) as HTMLButtonElement).disabled;

// An editor tab holding an edited buffer: a launch candidate for the inline
// branch (setDocument moves the generation past the saved mark, so it is not
// the pristine scaffold).
function editorTab(varName: string) {
  const id = useTabsStore.getState().newEditorTab(varName);
  getOrCreateDocumentStore(id).getState().setDocument(docNamed(varName));
  return id;
}

beforeEach(() => {
  vi.resetAllMocks();
  runsApi.createRun.mockResolvedValue({ run_id: "run-1" });
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
});

afterEach(cleanup);

describe("an inline render that failed", () => {
  it("disarms the form and keeps the cause on screen", async () => {
    const a = editorTab("var_of_A");
    const b = editorTab("var_of_B");
    useTabsStore.getState().setActive(a);
    api.unparse
      .mockResolvedValueOnce({ source: "RENDER OF A\n" })
      .mockRejectedValueOnce(new Error("unparse exploded: 500 render failed"));

    render(
      <LaunchDocStoreProvider>
        <Harness filePath="" />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(program()).toBe("RENDER OF A\n"));
    expect(form()).toBe("var_of_A");

    // The sidebar's × on the active editor tab, for real: closeTab disposes
    // A's store and elects B, and nothing reroutes because /runs/new is not
    // the closed tab's screen. The hook loads again, on B's buffer.
    useTabsStore.getState().closeTab(a);
    expect(useTabsStore.getState().activeEditorTabId).toBe(b);

    await waitFor(() => expect(error()).toMatch(/unparse exploded/));
    expect(program()).toBe("NO PROGRAM");
    // Nothing to launch, so nothing offers to: no form over the document the
    // view has moved on from, and no armed Launch.
    expect(form()).toBe("LOADING");
    expect(launchDisabled()).toBe(true);
    expect(error()).toMatch(/unparse exploded/);
  });
});

describe("a ?file= read that failed", () => {
  it("disarms the form and keeps the cause on screen, same path or not", async () => {
    const a = editorTab("var_of_A");
    const b = editorTab("var_of_B");
    useTabsStore.getState().setActive(a);
    api.openFile
      .mockResolvedValueOnce({
        source: "workflow x:\n  entry: done\n",
        document: docNamed("var_of_x"),
        diagnostics: [],
        path: "bots/x/main.bot",
      })
      .mockRejectedValueOnce(new Error("openFile exploded: 503 unavailable"));

    render(
      <LaunchDocStoreProvider>
        <Harness filePath="bots/x/main.bot" />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(program()).toBe("workflow x:\n  entry: done\n"));
    expect(form()).toBe("var_of_x");

    // The other reachable trigger — the app-level dock activating an editor
    // tab. The reload is for the SAME path, so the document's tag still
    // matches it: only clearing the answer disarms the form.
    useTabsStore.getState().setActive(b);

    await waitFor(() => expect(error()).toMatch(/openFile exploded/));
    expect(program()).toBe("NO PROGRAM");
    expect(form()).toBe("LOADING");
    expect(launchDisabled()).toBe(true);
    expect(error()).toMatch(/openFile exploded/);
  });
});
