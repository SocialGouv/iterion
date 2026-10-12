// @vitest-environment jsdom
//
// The kit's portals render into the page body, outside the subtree that owns
// them. Inside a subtree that is mounted and not shown — an editor tab behind
// another — they render nothing: shown, they would sit over whatever is on
// screen and act on the hidden subtree. Each primitive, opened both ways.
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import type { IterDocument } from "@/api/types";
import { createDocumentStore, DocumentStoreProvider } from "@/store/document";
import { Combobox } from "./Combobox";
import { Dialog } from "./Dialog";
import { Drawer } from "./Drawer";
import { DropdownMenu, DropdownMenuItem } from "./DropdownMenu";
import { HiddenSubtreeContext } from "./hiddenSubtree";
import { Popover } from "./Popover";
import RefAwarePopup from "./RefAwarePopup";
import { Tooltip } from "./Tooltip";

beforeAll(() => {
  // Radix positioning (floating-ui) needs ResizeObserver, which jsdom lacks.
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});
afterEach(cleanup);

const within = (hidden: boolean, node: ReactNode) => (
  <HiddenSubtreeContext.Provider value={hidden}>{node}</HiddenSubtreeContext.Provider>
);

describe.each([false, true])("in a subtree that is hidden: %s", (hidden) => {
  const shown = !hidden;

  it("a dialog", () => {
    render(within(hidden, <Dialog open onOpenChange={() => {}} title="A dialog">body</Dialog>));
    expect(!!screen.queryByText("A dialog")).toBe(shown);
  });

  it("a drawer", () => {
    render(within(hidden, <Drawer open onOpenChange={() => {}} title="A drawer">body</Drawer>));
    expect(!!screen.queryByText("A drawer")).toBe(shown);
  });

  it("a dropdown menu", () => {
    render(
      within(
        hidden,
        <DropdownMenu open trigger={<button type="button">menu</button>}>
          <DropdownMenuItem>An item</DropdownMenuItem>
        </DropdownMenu>,
      ),
    );
    expect(!!screen.queryByText("An item")).toBe(shown);
  });

  it("a popover", () => {
    render(within(hidden, <Popover open trigger={<button type="button">pop</button>}>A popover</Popover>));
    expect(!!screen.queryByText("A popover")).toBe(shown);
  });

  it("a combobox's list", () => {
    render(within(hidden, <Combobox value="" options={[{ value: "a", label: "An option" }]} onChange={() => {}} />));
    fireEvent.click(screen.getByRole("button"));
    expect(!!screen.queryByText("An option")).toBe(shown);
  });

  it("a tooltip", async () => {
    render(
      within(
        hidden,
        <Tooltip content="A tooltip" delayDuration={0}>
          <button type="button">tip</button>
        </Tooltip>,
      ),
    );
    act(() => {
      screen.getByRole("button").focus();
    });
    await act(async () => {
      await new Promise((r) => setTimeout(r, 20));
    });
    expect(screen.queryAllByText("A tooltip").length > 0).toBe(shown);
  });

  it("a reference popup", () => {
    const store = createDocumentStore();
    store.getState().setDocument({
      prompts: [],
      schemas: [],
      agents: [],
      judges: [],
      routers: [],
      humans: [],
      tools: [],
      computes: [],
      vars: { fields: [{ name: "target", type: "string" }] },
      workflows: [{ name: "w", entry: "done", edges: [] }],
      comments: [],
    } as unknown as IterDocument);
    const input = document.createElement("input");
    document.body.appendChild(input);
    render(
      within(
        hidden,
        <DocumentStoreProvider store={store}>
          <RefAwarePopup
            element={input}
            value="{{"
            caret={2}
            refContext={{ kind: "node-prompt", nodeId: "w" }}
            onSelect={() => {}}
            onClose={() => {}}
          />
        </DocumentStoreProvider>,
      ),
    );
    expect(!!screen.queryByRole("listbox")).toBe(shown);
    input.remove();
  });
});
