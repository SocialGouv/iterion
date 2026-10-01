// @vitest-environment jsdom
//
// The desktop's and the workspace shell's Edit → Undo / Redo reach the app as
// a message, not a key press — on Linux the desktop's Ctrl+Z accelerator
// takes every Ctrl+Z that way. They act on the editor tab on screen, and on
// nothing when the editor is not what the app shows: the active tab of an
// editor left for another page is a document the author cannot see (#1788).
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";

vi.mock("@/components/shared/AppShell", () => ({
  default: ({ children }: { children: ReactNode }) => <Suspense fallback={null}>{children}</Suspense>,
}));
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: () => <h1>cloud landing</h1>, PublicTopBar: () => null }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/views/ProjectSwitcher", () => ({ default: () => null }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));
// The tab's own content is not what this is about: the editor route is on
// screen once the tabs view is.
vi.mock("@/components/shared/EditorTabHost", () => ({ default: () => <div data-testid="editor-tab" /> }));
vi.mock("@/components/Home/RecentFilesPanel", () => ({ default: () => <div>recent files</div> }));

import App from "@/App";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore, type DocumentStore } from "@/store/document";
import { useServerInfoStore } from "@/store/serverInfo";
import { useTabsStore } from "@/store/tabs";

const marks = (s: DocumentStore) => (s.getState().document?.comments ?? []).map((c) => c.text).join(",");

function editedTab() {
  const id = useTabsStore.getState().openTab("editor", { file: "bots/a.bot" }, "A");
  const store = getOrCreateDocumentStore(id);
  store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "A" }] });
  store.getState().markSaved();
  store.getState().addComment({ text: "e1" });
  store.getState().addComment({ text: "e2" });
  return store;
}
const shellMenu = (menu: "undo" | "redo") =>
  act(async () => {
    window.dispatchEvent(new MessageEvent("message", { data: { source: "iterion-shell", type: "menu", menu } }));
  });

beforeEach(() => {
  window.history.replaceState({}, "", "/");
  useServerInfoStore.setState({ info: null, loading: false, error: null });
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL | Request) => {
      const path = String(input).split("?")[0] ?? "";
      const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
      if (path === "/api/server/info") return json({ auth_required: false, mode: "local" });
      return json({});
    }),
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("Edit → Undo from the menu", () => {
  it("does nothing to an editor tab while another page is on screen", async () => {
    const store = editedTab();
    render(
      <QueryClientProvider client={new QueryClient()}>
        <App />
      </QueryClientProvider>,
    );
    await screen.findByText("Signed-in home", undefined, { timeout: 5000 });
    await shellMenu("undo");
    expect(marks(store)).toBe("A,e1,e2");
    // Redo on its own: an undo made in the editor stays undone.
    act(() => store.getState().undo());
    await shellMenu("redo");
    expect(marks(store)).toBe("A,e1");
  });

  it("undoes and redoes one step in the editor tab on screen", async () => {
    const store = editedTab();
    window.history.replaceState({}, "", "/editor?file=bots%2Fa.bot");
    render(
      <QueryClientProvider client={new QueryClient()}>
        <App />
      </QueryClientProvider>,
    );
    await screen.findByTestId("editor-tab", undefined, { timeout: 5000 });
    await shellMenu("undo");
    expect(marks(store)).toBe("A,e1");
    await shellMenu("redo");
    expect(marks(store)).toBe("A,e1,e2");
  });
});

describe("Edit → Undo from the menu, on the editor's welcome pane", () => {
  // The link names a file another project's tab is on: that tab becomes the
  // active one, but it is not the current project's, so the editor shows its
  // welcome pane — and the menu has no tab on screen to act on.
  it("does nothing to another project's tab", async () => {
    useTabsStore.getState().setCurrentProjectKey("/p1");
    const store = editedTab();
    useTabsStore.getState().setCurrentProjectKey("/p2");
    window.history.replaceState({}, "", "/editor?file=bots%2Fa.bot");
    render(
      <QueryClientProvider client={new QueryClient()}>
        <App />
      </QueryClientProvider>,
    );
    await screen.findByText("The same workflows, visually.", undefined, { timeout: 5000 });
    expect(screen.queryAllByTestId("editor-tab")).toHaveLength(0);
    await shellMenu("undo");
    expect(marks(store)).toBe("A,e1,e2");
  });
});
