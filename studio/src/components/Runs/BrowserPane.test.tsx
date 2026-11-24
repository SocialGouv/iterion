// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import BrowserPane from "./BrowserPane";
import { getDefaultRunStore } from "@/store/run";

afterEach(cleanup);

describe("BrowserPane", () => {
  it("names its URL field", () => {
    render(<BrowserPane runId="r1" />);
    expect(
      screen.getByRole("textbox", { name: "Browser URL" }),
    ).toBeTruthy();
  });
});

describe("BrowserPane link names (RGAA 6.1, #2510)", () => {
  it("the open-in-new-tab link states its destination", () => {
    getDefaultRunStore().setState({
      browser: {
        ...getDefaultRunStore().getState().browser,
        currentUrl: "https://example.test/app",
      },
    });
    render(<BrowserPane runId="r1" />);
    const link = screen.getByRole("link");
    expect(link.getAttribute("aria-label")).toMatch(/open .* in a new tab/i);
  });
});
