// @vitest-environment jsdom
//
// The editor toolbar is one dense row of icon-only controls: its names ARE
// its interface (#2591's surface triage). Mounted the way
// EditorTabs.hiddenTab.test.tsx mounts it — the real Toolbar inside a
// document store, with the heavy panes and the native pickers out of the way.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  saveFile: vi.fn(),
  openFile: vi.fn(),
  listFiles: vi.fn(async () => []),
  listExampleEntries: vi.fn(async () => []),
  parseBotSourceEditorPath: () => null,
  inferCatalogBotId: () => null,
  botSourceEditorPath: () => "",
}));
vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  ...api,
}));
vi.mock("wouter", () => ({
  useLocation: () => ["/editor", vi.fn()],
  useSearch: () => "",
  Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span>,
}));
vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ activeTeamID: "t" }),
  useMaybeAuth: () => null,
}));
vi.mock("@/components/Editor/BundleFilesDrawer", () => ({ default: () => null }));
vi.mock("@/components/shared/ShortcutsHelp", () => ({
  default: ({ open }: { open: boolean }) => (open ? <div /> : null),
}));

import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";
import { createEmptyDocument } from "@/lib/defaults";
import { DocumentStoreProvider, getOrCreateDocumentStore } from "@/store/document";

import Toolbar from "./Toolbar";

afterEach(cleanup);

function mountToolbar() {
  const store = getOrCreateDocumentStore("toolbar-a11y");
  store.getState().setDocument(createEmptyDocument());
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <DocumentStoreProvider store={store}>
        <main>
          <Toolbar />
        </main>
      </DocumentStoreProvider>
    </QueryClientProvider>,
  );
}

describe("toolbar naming", () => {
  it("names every icon-only control it renders", () => {
    mountToolbar();
    for (const name of [
      "Undo (Ctrl+Z)",
      "Redo (Ctrl+Y)",
      "Validate",
      "Toggle source view",
      "Toggle diagnostics panel",
      "Keyboard shortcuts (?)",
    ]) {
      expect(screen.getByRole("button", { name })).toBeTruthy();
    }
    expect(
      screen.getByRole("combobox", {
        name: "Active workflow (switching redraws the canvas)",
      }),
    ).toBeTruthy();
  });

  it("leaves no two controls sharing an accessible name", () => {
    mountToolbar();
    const buttons = screen.getAllByRole("button");
    expect(buttons.length).toBeGreaterThan(6);
    // The toolbar is too wide to enumerate, so the candidate name is READ
    // from the markup and then handed to the real accessible-name algorithm
    // (getAllByRole's `name` option): a candidate the algorithm does not
    // answer to is a reading error and throws here, and more than one answer
    // is the collision this guards against. A name this test computed itself
    // would prove nothing.
    for (const button of buttons) {
      const candidate = button.getAttribute("aria-label") ?? button.textContent ?? "";
      expect(screen.getAllByRole("button", { name: candidate })).toHaveLength(1);
    }
  });
});

describe("toolbar axe", () => {
  it("Editor · toolbar — axe smoke, incompletes included", async () => {
    const { container } = mountToolbar();
    await expectNoViolationsStrict(container, "Toolbar");
  });
});
