// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import type { BackendUsage } from "@/api/runs";
import BackendsUsedRow from "./BackendsUsedRow";

afterEach(cleanup);

describe("BackendsUsedRow", () => {
  it("renders one chip per distinct backend/model pair", () => {
    const backends: BackendUsage[] = [
      { backend: "claw", model: "openai/gpt-5.4-mini", node_count: 2 },
      { backend: "claude_code", model: "sonnet", node_count: 1 },
    ];
    render(<BackendsUsedRow backends={backends} />);
    expect(screen.getByText("claw · openai/gpt-5.4-mini")).toBeTruthy();
    expect(screen.getByText("claude_code · sonnet")).toBeTruthy();
  });

  it("shows a ×N multiplier only when a pair covers more than one node", () => {
    const backends: BackendUsage[] = [
      { backend: "claw", model: "openai/gpt-5.4-mini", node_count: 3 },
      { backend: "claude_code", model: "sonnet", node_count: 1 },
    ];
    render(<BackendsUsedRow backends={backends} />);
    // The multi-node pair gets a ×3; the single-node pair gets none.
    expect(screen.getByText("×3")).toBeTruthy();
    expect(screen.queryByText("×1")).toBeNull();
  });

  it("drops the separator + model when the backend reported none", () => {
    render(
      <BackendsUsedRow backends={[{ backend: "claude_code", node_count: 1 }]} />,
    );
    expect(screen.getByText("claude_code")).toBeTruthy();
    expect(screen.queryByText(/·/)).toBeNull();
  });

  it("renders nothing for a tool/compute-only run (empty list)", () => {
    const { container } = render(<BackendsUsedRow backends={[]} />);
    expect(container.firstChild).toBeNull();
  });
});
