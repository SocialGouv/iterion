// @vitest-environment jsdom
//
// The editor hands the assistant the document's diagnostic counts: the
// readings' counts, the ones a reading did not report past its cap
// included — 5300 errors reported 4000 are 5300.
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("wouter", () => ({
  useSearch: () => "",
  useLocation: () => ["/editor", vi.fn()],
}));
vi.mock("@/components/Canvas/Canvas", () => ({ default: () => <div /> }));
vi.mock("@/components/Inspector/Inspector", () => ({ default: () => <div /> }));
vi.mock("@/components/Toolbar/Toolbar", () => ({ default: () => <div /> }));
vi.mock("@/components/Diagnostics/DiagnosticsPanel", () => ({ default: () => <div /> }));
vi.mock("@/components/Library/LibraryPanel", () => ({ default: () => <div /> }));
vi.mock("@/components/Canvas/SubNodePalette", () => ({ default: () => <div /> }));
vi.mock("@/components/SourceView/SourceView", () => ({ default: () => <div /> }));
vi.mock("@/hooks/useAutoValidation", () => ({ useAutoValidation: () => {} }));
vi.mock("@/hooks/useAutoOpenDiagnosticsOnError", () => ({ useAutoOpenDiagnosticsOnError: () => {} }));
vi.mock("@/hooks/useFileWatcher", () => ({ useFileWatcher: () => {} }));
const contexts: { state?: { diagnosticCount?: number } }[] = [];
vi.mock("@/lib/chatDock/pageContext", () => ({
  useAssistantPageContext: (c: { state?: { diagnosticCount?: number } }) => {
    contexts.push(c);
  },
}));
vi.mock("@/components/ui", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/components/ui")>()),
  DesktopOnlyNotice: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

import { DocumentStoreProvider, getOrCreateDocumentStore } from "@/store/document";
import { SelectionStoreProvider, getOrCreateSelectionStore } from "@/store/selection";
import { useTabsStore } from "@/store/tabs";

import EditorView from "./EditorView";

const NOTE = "a reading reports 1000 of each code and 2000 of each severity, every code's first kept";

afterEach(cleanup);

describe("the editor's diagnostic count", () => {
  it("is the readings' count, not the list's length", () => {
    const list: string[] = [];
    for (let i = 1; i <= 1000; i++) list.push(`main.bot:${i}:3: error [E011]: unknown property`);
    list[999] += ` — and 4300 more E011 errors not reported: ${NOTE}`;
    const tabId = useTabsStore.getState().openTab("editor", { file: "bots/x/main.bot" }, "x");
    const store = getOrCreateDocumentStore(tabId);
    store.getState().setDiagnostics(list);
    render(
      <DocumentStoreProvider store={store}>
        <SelectionStoreProvider store={getOrCreateSelectionStore(tabId)}>
          <EditorView active />
        </SelectionStoreProvider>
      </DocumentStoreProvider>,
    );
    expect(contexts[contexts.length - 1]?.state?.diagnosticCount).toBe(5300);
  });
});
