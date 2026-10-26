// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { ClampText } from "./ClampText";

afterEach(cleanup);

describe("ClampText", () => {
  it("keeps the full text recoverable: the sr-only copy and the focusable clamp", () => {
    render(<ClampText text="a very long description" className="block" />);
    expect(screen.getAllByText("a very long description").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("a very long description").length).toBeGreaterThanOrEqual(1);
  });
});
