// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import BrowserPane from "./BrowserPane";

afterEach(cleanup);

describe("BrowserPane", () => {
  it("names its URL field", () => {
    render(<BrowserPane runId="r1" />);
    expect(
      screen.getByRole("textbox", { name: "Browser URL" }),
    ).toBeTruthy();
  });
});
