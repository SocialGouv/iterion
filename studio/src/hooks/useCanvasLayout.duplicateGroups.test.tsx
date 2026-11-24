// @vitest-environment jsdom
//
// #1745 — a hand-written `.bot` can declare the same @group name twice. The
// canvas draws the first annotation and leaves the rest as the author wrote
// them — and SAYS so: one warning toast naming the duplicate, once per set
// of names, not one per render. The layout itself is out of scope here, so
// ELK is stubbed to a passthrough.
import { act, cleanup, render } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/autoLayout", () => ({
  autoLayout: (nodes: unknown[]) => Promise.resolve(nodes),
}));

import type { IterDocument } from "@/api/types";
import { createDocumentStore, DocumentStoreProvider } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { useCanvasLayout } from "./useCanvasLayout";

function docWithDuplicate(): IterDocument {
  return {
    prompts: [],
    schemas: [],
    agents: [
      {
        name: "build",
        model: "m",
        input: "in",
        output: "out",
        system: "s",
        user: "u",
        session: "fresh",
        comments: [{ text: "@group deploy: build, verify" }],
      },
      { name: "ship", model: "m", input: "in", output: "out", system: "s", user: "u", session: "fresh" },
      { name: "verify", model: "m", input: "in", output: "out", system: "s", user: "u", session: "fresh" },
    ],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [{ name: "w", entry: "build", edges: [] }],
    comments: [{ text: "@group deploy: build, ship" }],
  } as IterDocument;
}

function Probe() {
  useCanvasLayout();
  return null;
}

function renderWithDocument(d: IterDocument) {
  const store = createDocumentStore();
  store.getState().setDocument(d);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrap = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>
      <DocumentStoreProvider store={store}>{children}</DocumentStoreProvider>
    </QueryClientProvider>
  );
  return { store, ...render(<Probe />, { wrapper: wrap }) };
}

const warningToasts = () => useUIStore.getState().toasts.filter((t) => t.type === "warning");

beforeEach(() => {
  useUIStore.setState({ toasts: [] });
});
afterEach(cleanup);

describe("a duplicate @group name on the canvas (#1745)", () => {
  it("raises one warning toast naming the duplicate", () => {
    renderWithDocument(docWithDuplicate());
    const toasts = warningToasts();
    expect(toasts).toHaveLength(1);
    expect(toasts[0]!.message).toContain('"deploy"');
    expect(toasts[0]!.message).toContain("declared more than once");
  });

  it("does not repeat itself while the same duplicates stay", () => {
    const { store } = renderWithDocument(docWithDuplicate());
    expect(warningToasts()).toHaveLength(1);
    // A document move that keeps the same duplicate (a form edit, a
    // reload of the same text): the toast does not stack.
    act(() => store.getState().setDocument({ ...docWithDuplicate() }));
    expect(warningToasts()).toHaveLength(1);
  });

  it("says nothing when every name is declared once", () => {
    const d = docWithDuplicate();
    const single = {
      ...d,
      comments: [],
      agents: d.agents.map((a) =>
        a.name === "build" ? { ...a, comments: [{ text: "@group deploy: build, verify" }] } : a,
      ),
    };
    renderWithDocument(single);
    expect(warningToasts()).toHaveLength(0);
  });
});
