// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { Combobox } from "./Combobox";

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

describe("Combobox", () => {
  it("names its filter field from the placeholder (RGAA 11.1)", () => {
    render(
      <Combobox
        value=""
        options={[{ value: "a", label: "Alpha" }]}
        placeholder="Search bots…"
        onChange={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole("button"));
    expect(screen.getByRole("textbox", { name: "Search bots…" })).toBeTruthy();
  });
});
