// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import LogLinesView from "./LogLinesView";

afterEach(cleanup);

describe("LogLinesView", () => {
  it("names its search field", () => {
    render(
      <LogLinesView
        runId="r1"
        subscribeLogs={vi.fn()}
        unsubscribeLogs={vi.fn()}
      />,
    );
    expect(screen.getByRole("textbox", { name: "Search log" })).toBeTruthy();
  });
});
