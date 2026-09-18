// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { RunListSkeleton } from "./RunListSkeleton";

afterEach(() => cleanup());

describe("RunListSkeleton", () => {
  it("renders a busy status region for screen readers", () => {
    render(<RunListSkeleton />);
    const region = screen.getByRole("status", { name: "Loading runs" });
    expect(region.getAttribute("aria-busy")).toBe("true");
  });

  it("renders the requested number of placeholder rows in the table", () => {
    const { container } = render(<RunListSkeleton rows={5} />);
    // The desktop <table> gets exactly `rows` <tr> placeholders.
    const rows = container.querySelectorAll("table tbody tr");
    expect(rows.length).toBe(5);
  });

  it("defaults to a screenful of rows when count is omitted", () => {
    const { container } = render(<RunListSkeleton />);
    const rows = container.querySelectorAll("table tbody tr");
    expect(rows.length).toBe(8);
  });
});
