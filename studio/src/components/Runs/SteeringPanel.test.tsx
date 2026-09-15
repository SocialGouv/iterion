// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { SteeringPanel } from "./SteeringPanel";

vi.mock("./conversation/RunConversationView", () => ({
  default: ({ runId }: { runId: string }) => <div>Transcript {runId}</div>,
}));

vi.mock("@/components/shared/AgentChatboxInline", () => ({
  default: ({ runId }: { runId: string }) => <div>Composer {runId}</div>,
}));

afterEach(cleanup);

describe("SteeringPanel", () => {
  it("renders as fixed dock content without float or minimise controls", () => {
    render(<SteeringPanel runId="run-1" inputDisabled={false} />);

    expect(screen.getByText("Steering")).toBeTruthy();
    expect(screen.getByText("Transcript run-1")).toBeTruthy();
    expect(screen.getByText("Composer run-1")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /undock/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /minimise/i })).toBeNull();
  });

  it("keeps the transcript while input is disabled", () => {
    render(<SteeringPanel runId="run-2" inputDisabled />);

    expect(screen.getByText("Transcript run-2")).toBeTruthy();
    expect(screen.queryByText("Composer run-2")).toBeNull();
  });
});
