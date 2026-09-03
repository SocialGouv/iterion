// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import DoneStep from "../DoneStep";

afterEach(cleanup);

const props = {
  connectionID: "conn-1",
  repo: "group/api",
  onGoToRepos: vi.fn(),
  onOpenBoard: vi.fn(),
  onLaunchBot: vi.fn(),
  onConnectAnother: vi.fn(),
};

describe("connect wizard DoneStep", () => {
  it("reports success when the repo really was provisioned", () => {
    render(<DoneStep {...props} />);
    expect(screen.getByText(/Repository connected/i)).toBeTruthy();
    expect(
      screen.getByText(/provisioned with the required webhooks and tokens/i),
    ).toBeTruthy();
  });

  // The org-approval gate answers 202 and creates NOTHING on the forge. The
  // wizard used to render the green "Repository connected / bots have been
  // provisioned" summary anyway, so the operator walked away believing
  // automation was live and waited for reviews that could never fire.
  it("says the request is awaiting org approval on a 202", () => {
    render(<DoneStep {...props} pendingApproval />);
    expect(screen.getByText(/Awaiting org approval/i)).toBeTruthy();
    expect(screen.getByText(/nothing is created on the forge/i)).toBeTruthy();
    expect(screen.queryByText(/Repository connected/i)).toBeNull();
    expect(
      screen.queryByText(/provisioned with the required webhooks and tokens/i),
    ).toBeNull();
    // "Open the board" / "Launch a bot" promise a wired repo that does not
    // exist yet — they must not be offered.
    expect(screen.queryByRole("button", { name: /Open the board/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /Launch a bot/i })).toBeNull();
  });
});
