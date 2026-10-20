// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import LibraryPanel from "./LibraryPanel";

afterEach(cleanup);

describe("LibraryPanel", () => {
  it("names its search field — a placeholder alone is not a label (RGAA 11.1)", () => {
    render(<LibraryPanel />);
    fireEvent.click(screen.getByRole("button", { name: "Expand library" }));
    expect(screen.getByRole("textbox", { name: "Search library" })).toBeTruthy();
  });
});
