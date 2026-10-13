// @vitest-environment jsdom
//
// The launch page's promises, asserted where the operator meets them: the
// real LaunchView, mounted as App.tsx mounts it (LaunchDocStoreProvider over
// the active editor tab's store), driven through its own affordances — the
// Launch button, the no-sandbox dialog's Confirm — and through the real tabs
// store. The sibling files witness the same rules at their hooks
// (useLaunchDoc.*, useLaunchSubmit.*); this one witnesses that the wiring
// delivers them, which only a mount of the view can say.
//
// Three promises, four cases:
//   1. a launch posts the program and the values on screen — once;
//   2. an authorisation is WITHDRAWN, not hidden: neither the question nor a
//      launch comes back when its document does, and a form re-initialised
//      under the open dialog is refused by name instead of posted blank;
//   3. a load that failed offers no launch and claims nothing about the
//      workflow it could not read.
//
// `sandbox: none` throughout, so every launch passes the danger dialog — the
// entry point that reaches the submission directly, past the Launch button's
// own gate.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Route, Router, Switch } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ openFile: vi.fn(), unparse: vi.fn() }));
vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  openFile: api.openFile,
  unparse: api.unparse,
}));
const runsApi = vi.hoisted(() => ({ createRun: vi.fn(), previewRunCost: vi.fn() }));
vi.mock("@/api/runs", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/runs")>()),
  createRun: runsApi.createRun,
  previewRunCost: runsApi.previewRunCost,
}));
// The repo-target section asks the server for the active team's repos; this
// page's launch needs none, and a bot-less document declares no requirement.
vi.mock("@/hooks/useActiveRepo", () => ({
  useActiveRepo: () => ({
    activeRepo: null,
    overview: false,
    repos: [],
    loading: false,
    enabled: false,
    choose: () => {},
    teamID: null,
  }),
}));

import type { IterDocument } from "@/api/types";
import { createEmptyDocument } from "@/lib/defaults";
import { useBotsStore } from "@/store/bots";
import { getOrCreateDocumentStore } from "@/store/document";
import { useServerInfoStore } from "@/store/serverInfo";
import { useTabsStore } from "@/store/tabs";

import LaunchDocStoreProvider from "./launchView/LaunchDocStoreProvider";
import LaunchView from "./LaunchView";

// A REQUIRED var (no default), so it lands in the always-visible primary
// bucket (applyLaunchHints) and the missing-required gate has something to
// hold. Named after the document it belongs to: the field on screen says
// WHICH workflow the form is describing.
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
  vars: { fields: [{ name, type: "string" as const }] },
});

// An editor tab holding an edited buffer — a launch candidate for the inline
// branch (setDocument moves the generation past the saved mark, so it is not
// the pristine scaffold). Its store is the registry's, kept alive by tab id:
// the same document OBJECT comes back when the tab is elected again.
function editorTab(varName: string) {
  const id = useTabsStore.getState().newEditorTab(varName);
  getOrCreateDocumentStore(id).getState().setDocument(docNamed(varName));
  return id;
}

// The trigger, for real: switching project repoints the ACTIVE editor tab to
// the first one visible under the new scope, without navigating — so the
// launch form reloads on another buffer while staying on /runs/new
// (store/tabs' setCurrentProjectKey, driven at the app root by
// useProjectScopeSync). A tab is stamped with the key current when it opens.
function switchProject(key: string) {
  useTabsStore.getState().setCurrentProjectKey(key);
}

let qc: QueryClient;
// The route as App.tsx declares it, Switch included: a launch navigates to
// /runs/:id, and under a Switch that unmounts the form — as it does in the
// app. Mounted bare, the view would stay up on the run's URL with no ?file=
// and re-read the editor buffer instead.
function mount(path: string) {
  const loc = memoryLocation({ path, record: true });
  return render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook}>
        <Switch>
          <Route path="/runs/new">
            <LaunchDocStoreProvider>
              <LaunchView />
            </LaunchDocStoreProvider>
          </Route>
        </Switch>
      </Router>
    </QueryClientProvider>,
  );
}

// Queried, not got: every case that matters here asserts one of these ABSENT.
// The clickers get instead, so a missing affordance fails at the click.
const launchBtn = () => screen.queryByRole("button", { name: "Launch" });
const unsandboxed = () => screen.queryByRole("button", { name: "Launch unsandboxed" });
const clickLaunch = () => fireEvent.click(screen.getByRole("button", { name: "Launch" }));
const clickUnsandboxed = () =>
  fireEvent.click(screen.getByRole("button", { name: "Launch unsandboxed" }));
// The var label carries the name + type + Required pill, so address the
// control by the id LaunchView gives it (`var-<name>`) — the product's own
// handle.
const varInput = (name: string) =>
  document.getElementById(`var-${name}`) as HTMLInputElement | HTMLTextAreaElement;

beforeEach(() => {
  vi.resetAllMocks();
  // jsdom has no matchMedia; DesktopOnlyNotice asks for one. Desktop.
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    configurable: true,
    value: (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }),
  });
  qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  runsApi.createRun.mockResolvedValue({ run_id: "run-1" });
  runsApi.previewRunCost.mockRejectedValue(new Error("no cost preview in this test"));
  useTabsStore.setState({
    tabs: [],
    activeEditorTabId: null,
    activeRunTabId: null,
    currentProjectKey: null,
  });
  useBotsStore.setState({ bots: [] });
  useServerInfoStore.setState({
    info: {
      mode: "local",
      version: "test",
      work_dir: "/workspace/project",
      limits: {
        upload: { max_file_size: 1, max_total_size: 1, max_files_per_run: 1, allowed_mime: [] },
      },
    },
  });
});

afterEach(cleanup);

describe("the launch page posts what the operator saw", () => {
  it("the no-sandbox Confirm launches that program with that value, once", async () => {
    editorTab("ignored");
    api.openFile.mockResolvedValue({
      source: "workflow x:\n  entry: done\n",
      document: docNamed("ticket"),
      diagnostics: [],
      path: "bots/x.bot",
    });

    mount("/runs/new?file=bots/x.bot");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(varInput("ticket"), { target: { value: "TYPED-1" } });
    clickLaunch();
    await waitFor(() => expect(unsandboxed()).not.toBeNull());
    clickUnsandboxed();

    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(runsApi.createRun.mock.calls[0]?.[0]).toMatchObject({
      file_path: "bots/x.bot",
      vars: { ticket: "TYPED-1" },
    });
    // And it leaves: the operator lands on the run it just started.
    await waitFor(() => expect(launchBtn()).toBeNull());
  });
});

describe("an authorisation is withdrawn, not hidden", () => {
  it("a document that comes back does not bring its question back", async () => {
    switchProject("/p1");
    editorTab("ticket");
    switchProject("/p2");
    editorTab("other");
    switchProject("/p1");
    api.unparse.mockImplementation(async (d: IterDocument) => ({
      source: `rendered ${d.vars?.fields?.[0]?.name}\n`,
    }));

    mount("/runs/new");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(varInput("ticket"), { target: { value: "TYPED-1" } });
    clickLaunch();
    await waitFor(() => expect(unsandboxed()).not.toBeNull());

    // Away and back. The tab registry keeps a store per tab id, so the
    // second switch hands the form the SAME document object the question was
    // asked for.
    switchProject("/p2");
    await waitFor(() => expect(varInput("other")).toBeTruthy());
    expect(unsandboxed()).toBeNull();
    switchProject("/p1");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());

    // Nobody pressed Launch since, and the value the question was armed with
    // is gone (the reload re-initialised the form).
    expect(varInput("ticket").value).toBe("");
    expect(unsandboxed()).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(runsApi.createRun).not.toHaveBeenCalled();
  });

  it("a Confirm over a re-initialised form is refused by name, not posted blank", async () => {
    switchProject("/p1");
    editorTab("ticket");
    switchProject("/p2");
    editorTab("other");
    switchProject("/p1");
    // The second buffer's render never lands: its election is superseded
    // before the round trip answers, so the hook's answer is never replaced
    // and the first document keeps its identity across the reload. Nothing a
    // tag on the authorisation can tell — only the submit can.
    api.unparse.mockImplementation(async (d: IterDocument) => {
      const n = d.vars?.fields?.[0]?.name;
      if (n === "other") return new Promise<{ source: string }>(() => {});
      return { source: `rendered ${n}\n` };
    });

    mount("/runs/new");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(varInput("ticket"), { target: { value: "TYPED-1" } });
    clickLaunch();
    await waitFor(() => expect(unsandboxed()).not.toBeNull());

    switchProject("/p2");
    await waitFor(() => expect(api.unparse).toHaveBeenCalledTimes(2));
    switchProject("/p1");
    await waitFor(() => expect(varInput("ticket").value).toBe(""));
    // The dialog is still up — same document, same identity.
    expect(unsandboxed()).not.toBeNull();

    clickUnsandboxed();
    // Refused before any request leaves the browser, by the name of what is
    // missing — not posted with the required var silently dropped. The
    // page-level banner (role alert), not the inline caption the blanked form
    // already carried: that one is on screen before the Confirm.
    expect(runsApi.createRun).not.toHaveBeenCalled();
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toBe("Required input missing: ticket"),
    );
  });
});

describe("a load that failed", () => {
  it("offers no launch and claims nothing about the workflow", async () => {
    editorTab("ignored");
    api.openFile.mockRejectedValue(new Error("openFile exploded: 503 unavailable"));

    mount("/runs/new?file=bots/x.bot");
    await waitFor(() => expect(screen.getByText(/openFile exploded/)).toBeTruthy());

    // No affordance, and no sentence about a document nobody read — neither
    // its vars nor its sandbox posture (`sandboxModeLabel(null)` reads
    // "auto (default)", the reassuring label, for a `sandbox: none` file
    // whose reload failed).
    expect(launchBtn()).toBeNull();
    expect(unsandboxed()).toBeNull();
    const body = document.body.textContent ?? "";
    expect(body).not.toMatch(/declares no input vars/);
    expect(body).not.toMatch(/Sandbox:/);
    expect(runsApi.createRun).not.toHaveBeenCalled();
  });
});
