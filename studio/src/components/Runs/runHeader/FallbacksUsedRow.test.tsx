// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { FallbackUsage } from "@/api/runs";
import FallbacksUsedRow from "./FallbacksUsedRow";

afterEach(cleanup);

describe("FallbacksUsedRow", () => {
  it("names the node and the route that served it", () => {
    const fallbacks: FallbackUsage[] = [
      {
        node_id: "implement",
        served_by: "api",
        backend: "claw",
        model: "anthropic/claude-opus-5",
      },
    ];
    render(<FallbacksUsedRow fallbacks={fallbacks} />);
    expect(screen.getByText("implement")).toBeTruthy();
    expect(
      screen.getByText("api (claw · anthropic/claude-opus-5)"),
    ).toBeTruthy();
  });

  // The row is the only after-the-fact evidence that a run was degraded,
  // so it must never render on a clean one — its presence has to mean
  // something happened.
  it("renders nothing on a clean run", () => {
    const { container } = render(<FallbacksUsedRow fallbacks={[]} />);
    expect(container.firstChild).toBeNull();
  });

  // A CLI backend that reports no effective model leaves the field
  // empty; the chip must degrade to the route name rather than printing
  // "undefined".
  it("degrades to the route name when the backend reported no model", () => {
    render(
      <FallbacksUsedRow
        fallbacks={[{ node_id: "review", served_by: "run-fallback" }]}
      />,
    );
    expect(screen.getByText("run-fallback")).toBeTruthy();
  });
});
