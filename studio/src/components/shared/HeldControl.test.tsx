// @vitest-environment jsdom
//
// A held control is one element at one place whether it is held or not (rva
// round 3): a hold that lifts — a catalog that loaded, a role that changed —
// while the control has the keyboard focus must leave the focus on it, not
// drop it to the document.
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import HeldControl from "./HeldControl";

afterEach(cleanup);

const control = () => screen.getByRole("button", { name: "Go" });

// A held control that takes the focus opens its tooltip, and Radix positioning
// (floating-ui) needs ResizeObserver, which jsdom lacks.
beforeAll(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});

describe("HeldControl", () => {
  // The tooltip is the same sentence for a mouse and for a keyboard — and it
  // is shown to someone. A hold that lifts while the control has the focus
  // closes it; when the hold falls again with the focus elsewhere, nothing is
  // open over a control nobody is on (rva round 4, F7).
  it("does not show its reason again once the hold lifted and the focus left", async () => {
    const tree = (reason: string | null) => (
      <>
        <HeldControl reason={reason}>
          <button type="button">Go</button>
        </HeldControl>
        <button type="button">Elsewhere</button>
      </>
    );
    const { rerender } = render(tree("not yet"));
    act(() => control().focus());
    expect(screen.queryByRole("tooltip")).not.toBeNull();

    rerender(tree(null));
    expect(screen.queryByRole("tooltip")).toBeNull();
    act(() => screen.getByRole("button", { name: "Elsewhere" }).focus());
    rerender(tree("held again"));
    await act(async () => {});
    expect(document.activeElement?.textContent).toBe("Elsewhere");
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("keeps the control, and its focus, when the hold lifts and when it falls", () => {
    const tree = (reason: string | null) => (
      <HeldControl reason={reason}>
        <button type="button">Go</button>
      </HeldControl>
    );
    const { rerender } = render(tree("not yet"));
    const node = control();
    expect(node.getAttribute("aria-disabled")).toBe("true");
    expect(document.getElementById(node.getAttribute("aria-describedby") ?? "")?.textContent).toBe("not yet");
    node.focus();
    expect(document.activeElement).toBe(node);

    rerender(tree(null));
    expect(control()).toBe(node);
    expect(document.activeElement).toBe(node);
    expect(node.getAttribute("aria-disabled")).toBeNull();
    expect(node.getAttribute("aria-describedby")).toBeNull();

    rerender(tree("held again"));
    expect(control()).toBe(node);
    expect(document.activeElement).toBe(node);
    expect(node.getAttribute("aria-disabled")).toBe("true");
  });
});
