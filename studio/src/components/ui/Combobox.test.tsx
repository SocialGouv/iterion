// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { Combobox } from "./Combobox";

// Radix Popover positioning (floating-ui) needs ResizeObserver, which
// jsdom doesn't ship.
beforeAll(() => {
  class RO {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  vi.stubGlobal("ResizeObserver", RO);
});

afterEach(cleanup);

const OPTIONS = [
  { value: "alpha", label: "Alpha", description: "first" },
  { value: "beta", label: "Beta", description: "second" },
];

describe("Combobox", () => {
  it("portals the popup out of overflow-clipping ancestors", () => {
    // Regression guard for the AddTaskDialog bug: the popup used to be an
    // absolutely-positioned sibling INSIDE the dialog's overflow-y-auto
    // body, so it was clipped to ~1.5 rows. Portaled content must NOT be
    // a DOM descendant of the (clipping) container anymore.
    const { container } = render(
      <div style={{ maxHeight: 40, overflowY: "auto" }}>
        <Combobox value="" options={OPTIONS} onChange={() => {}} />
      </div>,
    );
    const trigger = screen.getByRole("button");
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(trigger);
    expect(trigger.getAttribute("aria-expanded")).toBe("true");

    const listbox = screen.getByRole("listbox");
    expect(container.contains(listbox)).toBe(false);
    expect(document.body.contains(listbox)).toBe(true);
    expect(screen.getByText("Alpha")).toBeTruthy();
    expect(screen.getByText("Beta")).toBeTruthy();
  });

  it("commits a clicked option and closes", () => {
    const onChange = vi.fn();
    render(<Combobox value="" options={OPTIONS} onChange={onChange} />);
    fireEvent.click(screen.getByRole("button"));
    fireEvent.mouseDown(screen.getByText("Beta"));

    expect(onChange).toHaveBeenCalledWith("beta");
    expect(screen.queryByRole("listbox")).toBeNull();
  });

  it("filters options from the search input", () => {
    render(<Combobox value="" options={OPTIONS} onChange={() => {}} />);
    fireEvent.click(screen.getByRole("button"));
    fireEvent.change(screen.getByPlaceholderText("Search…"), {
      target: { value: "bet" },
    });

    expect(screen.queryByText("Alpha")).toBeNull();
    expect(screen.getByText("Beta")).toBeTruthy();
  });

  it("keeps the empty-value affordance on top and commits it", () => {
    const onChange = vi.fn();
    render(
      <Combobox
        value="beta"
        options={OPTIONS}
        onChange={onChange}
        emptyLabel="(dispatcher default)"
      />,
    );
    fireEvent.click(screen.getByRole("button"));
    fireEvent.mouseDown(screen.getByText("(dispatcher default)"));

    expect(onChange).toHaveBeenCalledWith("");
  });
});
