// @vitest-environment jsdom
//
// The no-sandbox confirmation authorises host execution of ONE program: the
// workflow whose form the operator read. This view outlives that program —
// the document reloads under the open form when the ACTIVE editor tab
// changes (LaunchDocStoreProvider), and the ?file= path moves without
// leaving the route (the command palette's recent-file entries) — and the
// dialog's Confirm reaches launchRun directly, past onSubmit.
//
// Kept across either move, that Confirm launches a payload built from a form
// that is gone: `fields` is derived from the document, so an empty one yields
// no `vars` key at all (buildVarsPayload) and no required-var gate — the
// operator's inputs silently replaced by every declared default, under a
// program they never confirmed.
//
// One case per move, plus the two controls that keep the fix honest: the
// legitimate confirmation still launches with the typed value, and an
// unrelated re-render does not withdraw the question.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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

import type { IterDocument, ServerInfo } from "@/api/types";
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

// Local mode: outside cloud a bare file_path IS a program, so the submit gate
// lets the stale Confirm through — the payload is the only thing left to
// notice the move (launchCarriesProgram).
const localInfo: ServerInfo = {
  mode: "local",
  version: "test",
  work_dir: "/workspace/project",
  limits: {
    upload: { max_file_size: 1, max_total_size: 1, max_files_per_run: 1, allowed_mime: [] },
  },
};

// `sandbox: none` is what arms the dialog (isSandboxActive), and one var with
// a default names the document on screen while staying non-required — the
// missing-required gate runs before the sandbox check.
const docNamed = (name: string): IterDocument => ({
  ...createEmptyDocument(),
  workflows: [
    {
      name: "w",
      entry: "done",
      nodes: [],
      edges: [],
      sandbox: { mode: "none" },
    } as unknown as IterDocument["workflows"][number],
  ],
  vars: {
    fields: [
      { name, type: "string" as const, default: { kind: "string", raw: '"d"', str_val: "d" } },
    ],
  },
});

// The launch surface as LaunchView wires it: the Launch affordance is
// `!!doc`, the dialog is rendered from `showNoSandboxConfirm`, and its
// Confirm closes it then calls launchRun — never onSubmit again.
function Harness({ filePath }: { filePath: string }) {
  const [error, setError] = useState<string | null>(null);
  const { doc, currentSource, confirmedDiskPath, fields, values, setValues } = useLaunchDoc(
    filePath,
    setError,
  );
  const { onSubmit, launchRun, showNoSandboxConfirm, setShowNoSandboxConfirm } = useLaunchSubmit({
    filePath,
    doc,
    currentSource,
    confirmedDiskPath,
    serverInfo: localInfo,
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
      <output aria-label="error">{error ?? ""}</output>
      {fields.map((f) => (
        <input
          key={f.name}
          aria-label={`var-${f.name}`}
          value={values[f.name] ?? ""}
          onChange={(e) => setValues((p) => ({ ...p, [f.name]: e.target.value }))}
        />
      ))}
      <button onClick={onSubmit} disabled={!doc}>
        Launch
      </button>
      {showNoSandboxConfirm && (
        <button
          onClick={() => {
            setShowNoSandboxConfirm(false);
            void launchRun();
          }}
        >
          Launch without sandbox
        </button>
      )}
    </>
  );
}

const form = () => screen.getByLabelText("form").textContent;
const error = () => screen.getByLabelText("error").textContent;
const confirmBtn = () => screen.queryByRole("button", { name: "Launch without sandbox" });
const clickLaunch = () => fireEvent.click(screen.getByRole("button", { name: "Launch" }));
// getByRole, not the queried handle: the click requires the question to be
// on screen, so a withdrawn dialog fails here instead of silently no-opping.
const clickConfirm = () =>
  fireEvent.click(screen.getByRole("button", { name: "Launch without sandbox" }));

// An editor tab holding an edited buffer — what this route reads, and what
// electing another one reloads the form from.
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

describe("the no-sandbox confirmation", () => {
  it("launches the program it was given for, with the operator's typed value", async () => {
    useTabsStore.getState().setActive(editorTab("ignored"));
    api.openFile.mockResolvedValue({
      source: "workflow x:\n  entry: done\n",
      document: docNamed("ticket"),
      diagnostics: [],
      path: "bots/x.bot",
    });

    render(
      <LaunchDocStoreProvider>
        <Harness filePath="bots/x.bot" />
      </LaunchDocStoreProvider>,
    );
    // Nobody is asked before they ask: no dialog while the form loads, where
    // there is no document yet — the state a dismissed one is tagged with.
    expect(form()).toBe("LOADING");
    expect(confirmBtn()).toBeNull();

    await waitFor(() => expect(form()).toBe("ticket"));
    fireEvent.change(screen.getByLabelText("var-ticket"), { target: { value: "TYPED-123" } });
    clickLaunch();
    expect(confirmBtn()).not.toBeNull();

    // A re-render while the question is up does not withdraw it: only the
    // document behind it moving does.
    fireEvent.change(screen.getByLabelText("var-ticket"), { target: { value: "TYPED-456" } });
    expect(confirmBtn()).not.toBeNull();

    clickConfirm();

    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(runsApi.createRun.mock.calls[0]?.[0]).toMatchObject({
      file_path: "bots/x.bot",
      vars: { ticket: "TYPED-456" },
    });
  });

  it("is withdrawn when the reload under it leaves no document", async () => {
    const a = editorTab("ignored_a");
    const b = editorTab("ignored_b");
    useTabsStore.getState().setActive(a);
    api.openFile
      .mockResolvedValueOnce({
        source: "workflow x:\n  entry: done\n",
        document: docNamed("ticket"),
        diagnostics: [],
        path: "bots/x.bot",
      })
      .mockRejectedValueOnce(new Error("openFile exploded: 503 unavailable"));

    render(
      <LaunchDocStoreProvider>
        <Harness filePath="bots/x.bot" />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(form()).toBe("ticket"));
    fireEvent.change(screen.getByLabelText("var-ticket"), { target: { value: "TYPED-123" } });
    clickLaunch();
    expect(confirmBtn()).not.toBeNull();

    // The app-level dock elects another editor tab, this path is read again
    // — and fails. The form is gone (`!!doc`), so the question it answered
    // is gone too.
    useTabsStore.getState().setActive(b);
    await waitFor(() => expect(error()).toMatch(/openFile exploded/));
    expect(form()).toBe("LOADING");

    expect(confirmBtn()).toBeNull();
    expect(runsApi.createRun).not.toHaveBeenCalled();
    // The cause stays on screen: nothing here speaks for a failure it did
    // not cause, and nothing navigates away from it.
    expect(error()).toMatch(/openFile exploded/);
  });

  it("is withdrawn when the URL moves to another workflow", async () => {
    useTabsStore.getState().setActive(editorTab("ignored_c"));
    api.openFile.mockImplementation(async (path: string) => ({
      source: `source of ${path}\n`,
      document: docNamed(`var_of_${path}`),
      diagnostics: [],
      path,
    }));

    const { rerender } = render(
      <LaunchDocStoreProvider>
        <Harness filePath="bots/x.bot" />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(form()).toBe("var_of_bots/x.bot"));
    fireEvent.change(screen.getByLabelText("var-var_of_bots/x.bot"), {
      target: { value: "TYPED-123" },
    });
    clickLaunch();
    expect(confirmBtn()).not.toBeNull();

    // The command palette's recent-file entry, which moves ?file= without
    // leaving /runs/new (the route matches with any query), inside ONE mount
    // of this form.
    rerender(
      <LaunchDocStoreProvider>
        <Harness filePath="bots/z.bot" />
      </LaunchDocStoreProvider>,
    );

    expect(confirmBtn()).toBeNull();
    await waitFor(() => expect(form()).toBe("var_of_bots/z.bot"));
    // Z is never launched under a confirmation given for X — not before its
    // form arrives, and not after.
    expect(runsApi.createRun).not.toHaveBeenCalled();
    expect(confirmBtn()).toBeNull();
  });
});
