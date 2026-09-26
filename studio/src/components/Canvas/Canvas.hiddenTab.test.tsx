// @vitest-environment jsdom
//
// Every open editor tab has its own Canvas, mounted while its tab is hidden.
// What a canvas does with something GLOBAL — the Cmd+K shortcut, the one
// Arrange / Fit-view slot the Toolbar calls, the one-shot "centre this node"
// request — must be done by the tab on screen alone (#1788).
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("wouter", () => ({
  useLocation: () => ["/editor", vi.fn()],
  useSearch: () => "",
  Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span>,
}));
// Vendor icon barrels on the node renderers' import path resolve their ESM
// subpaths in a way vitest cannot follow; pure presentation, the same stub
// the form suites use.
vi.mock("@/components/icons/ProviderIcon", () => ({
  ProviderIcon: () => null,
  ProviderLabel: () => null,
}));
vi.mock("@/components/icons/BackendBadge", () => ({
  effectiveBackend: (backend?: string, resolved?: string) => backend || resolved || "claw",
  BackendBadge: () => null,
}));
vi.mock("@/components/shared/CommandPalette", () => ({
  default: ({ open }: { open: boolean }) => (open ? <div data-testid="canvas-palette" /> : null),
}));
// Which canvas a fitView came from: each canvas renders under a tag, and the
// React Flow instance it asks for records the tag of every fitView. `getNodes`
// answers with the node a "centre this node" request names, so the canvas
// that takes the request is the one that records it.
const probe = vi.hoisted(() => ({
  fits: [] as string[],
  centred: [] as string[],
  requested: null as string | null,
  Tag: null as unknown as React.Context<string>,
}));
vi.mock("@xyflow/react", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@xyflow/react")>();
  const React = await import("react");
  probe.Tag = React.createContext("");
  return {
    ...actual,
    useReactFlow: () => {
      const rf = actual.useReactFlow();
      const tag = React.useContext(probe.Tag);
      return React.useMemo(
        () => ({
          ...rf,
          getNodes: () => (probe.requested ? [{ id: probe.requested }] : rf.getNodes()),
          fitView: (...args: Parameters<typeof rf.fitView>) => {
            probe.fits.push(tag);
            const target = args[0]?.nodes?.[0]?.id;
            if (target) probe.centred.push(`${tag}:${target}`);
            return rf.fitView(...args);
          },
        }),
        [rf, tag],
      );
    },
  };
});

import { ReactFlowProvider } from "@xyflow/react";
import { EditorTabActiveContext } from "@/components/Editor/editorTabActive";
import { HiddenSubtreeContext } from "@/components/ui/hiddenSubtree";
import { createEmptyDocument } from "@/lib/defaults";
import { DocumentStoreProvider, createDocumentStore } from "@/store/document";
import { SelectionStoreProvider, createSelectionStore } from "@/store/selection";
import { useUIStore } from "@/store/ui";
import Canvas from "./Canvas";

beforeAll(() => {
  // React Flow measures its container; jsdom has no layout.
  globalThis.ResizeObserver ??= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver;
});

const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const stores = new Map<
  string,
  { doc: ReturnType<typeof createDocumentStore>; sel: ReturnType<typeof createSelectionStore> }
>();
function storesOf(tag: string) {
  let entry = stores.get(tag);
  if (!entry) {
    const doc = createDocumentStore();
    doc.getState().setDocument(createEmptyDocument());
    entry = { doc, sel: createSelectionStore() };
    stores.set(tag, entry);
  }
  return entry;
}

// A tab as EditorView renders it: the canvas under the tab's own stores, and
// the two things EditorView says about the tab — on screen or not.
function canvasTab(tag: string, active: boolean) {
  const { doc, sel } = storesOf(tag);
  return (
    <div key={tag} data-testid={`tab-${tag}`} className={active ? "block" : "hidden"}>
      <probe.Tag.Provider value={tag}>
        <EditorTabActiveContext.Provider value={active}>
          <HiddenSubtreeContext.Provider value={!active}>
            <DocumentStoreProvider store={doc}>
              <SelectionStoreProvider store={sel}>
                <ReactFlowProvider>
                  <Canvas />
                </ReactFlowProvider>
              </SelectionStoreProvider>
            </DocumentStoreProvider>
          </HiddenSubtreeContext.Provider>
        </EditorTabActiveContext.Provider>
      </probe.Tag.Provider>
    </div>
  );
}
const tabs = (...list: [string, boolean][]) => (
  <QueryClientProvider client={qc}>{list.map(([tag, active]) => canvasTab(tag, active))}</QueryClientProvider>
);
// What the Toolbar's Fit-view button calls: whose canvas does it reach?
const fitViaSlot = () => {
  probe.fits.length = 0;
  act(() => {
    useUIStore.getState().canvasActions.fitView?.();
  });
  return [...probe.fits];
};

beforeEach(() => {
  useUIStore.setState({ canvasActions: { arrange: null, fitView: null } });
  stores.clear();
  probe.fits.length = 0;
  probe.centred.length = 0;
  probe.requested = null;
});
afterEach(cleanup);

describe("Cmd+K with two canvases mounted", () => {
  it("opens the palette of the tab on screen only", () => {
    // The hidden tab first, as a tab list puts it when the active tab is not the first.
    render(tabs(["b", false], ["a", true]));
    act(() => {
      fireEvent.keyDown(document.body, { key: "k", ctrlKey: true });
    });
    const palettes = screen.getAllByTestId("canvas-palette");
    expect(palettes).toHaveLength(1);
    expect(screen.getByTestId("tab-a").contains(palettes[0] ?? null)).toBe(true);
  });
});

describe("the Arrange / Fit-view slot with two canvases mounted", () => {
  it("reaches the canvas on screen, before and after the hidden tab mounts, and once it closes", () => {
    const view = render(tabs(["a", true]));
    expect(fitViaSlot()).toEqual(["a"]);

    view.rerender(tabs(["a", true], ["b", false]));
    expect(fitViaSlot()).toEqual(["a"]);

    view.rerender(tabs(["a", true]));
    expect(fitViaSlot()).toEqual(["a"]);
  });

  it("follows the tab brought on screen", () => {
    const view = render(tabs(["a", true], ["b", false]));
    view.rerender(tabs(["a", false], ["b", true]));
    expect(fitViaSlot()).toEqual(["b"]);
  });
});

describe("a request to centre a node, with two canvases mounted", () => {
  const settle = () =>
    act(async () => {
      await new Promise((r) => setTimeout(r, 400));
    });

  it("is taken by the canvas of the tab it was made in, never by another's", async () => {
    render(tabs(["b", true], ["a", false]));
    // b holds a node of the same name: the request is still a's.
    probe.requested = "agent_1";
    act(() => storesOf("a").sel.getState().setPendingFitNodeId("agent_1"));
    await settle();
    expect(probe.centred).toEqual([]);
    expect(storesOf("a").sel.getState().pendingFitNodeId).toBe("agent_1");
  });

  it("waits while its tab is hidden, and is taken when the tab is shown again", async () => {
    const view = render(tabs(["a", true], ["b", false]));
    probe.requested = "agent_1";
    act(() => storesOf("a").sel.getState().setPendingFitNodeId("agent_1"));
    // The author switches tabs before the canvas takes it.
    view.rerender(tabs(["a", false], ["b", true]));
    await settle();
    expect(probe.centred).toEqual([]);

    view.rerender(tabs(["a", true], ["b", false]));
    await settle();
    expect(probe.centred).toEqual(["a:agent_1"]);
    expect(storesOf("a").sel.getState().pendingFitNodeId).toBeNull();
  });
});
