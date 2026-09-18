// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// Mock the auth API + auth context so the page renders without a server.
vi.mock("@/api/auth", async () => {
  const actual = await vi.importActual<typeof import("@/api/auth")>("@/api/auth");
  return {
    ...actual,
    completePendingPasswordChange: vi.fn(async () => ({
      user: { id: "u", email: "a@b", status: "active", is_super_admin: false },
      teams: [],
      active_team_id: "",
      active_role: "",
    })),
  };
});

const reloadIdentity = vi.fn(async () => undefined);
const navigate = vi.fn();

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ reloadIdentity }),
}));

vi.mock("wouter", () => ({
  useLocation: () => ["/auth/password/change", navigate],
}));

import ForcedPasswordChange from "@/views/auth/ForcedPasswordChange";
import * as authApi from "@/api/auth";
import { STUDIO_BASE } from "@/lib/scope";

beforeEach(() => {
  vi.clearAllMocks();
  window.history.replaceState(
    {},
    "",
    "/auth/password/change?email=admin@example.com&temp=initial",
  );
});

afterEach(() => {
  cleanup();
});

describe("ForcedPasswordChange", () => {
  it("keeps the requested run while removing temporary credentials", async () => {
    const next = "/runs/review-123?tab=events#node-converge";
    window.history.replaceState({}, "", `/auth/password/change?${new URLSearchParams({ email: "admin@example.com", temp: "initial", next })}`);
    render(<ForcedPasswordChange />);
    expect(new URLSearchParams(window.location.search).get("next")).toBe(next);
    expect(window.location.href).not.toContain("initial");
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: "ChangedAt#1" } });
    fireEvent.change(screen.getByLabelText("Confirm new password"), { target: { value: "ChangedAt#1" } });
    fireEvent.submit(screen.getByTestId("forced-password-change-form"));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith(next, { replace: true }));
  });

  it("pre-fills email + temp from the URL and clears the URL", () => {
    render(<ForcedPasswordChange />);
    const inputs = screen.getAllByDisplayValue(/.+/);
    expect(inputs.length).toBeGreaterThan(0);
    expect(window.location.search).toBe("");
  });

  it("calls completePendingPasswordChange and navigates on success", async () => {
    render(<ForcedPasswordChange />);
    const inputs = document.querySelectorAll("input");
    // Fill new + confirm with the same valid value.
    const newPwd = inputs[2]!;
    const confirmPwd = inputs[3]!;
    fireEvent.change(newPwd, { target: { value: "ChangedAt#1" } });
    fireEvent.change(confirmPwd, { target: { value: "ChangedAt#1" } });

    const form = screen.getByTestId("forced-password-change-form");
    fireEvent.submit(form);

    await waitFor(() => {
      expect(authApi.completePendingPasswordChange).toHaveBeenCalledWith(
        "admin@example.com",
        "initial",
        "ChangedAt#1",
      );
    });
    expect(reloadIdentity).toHaveBeenCalled();
    expect(navigate).toHaveBeenCalledWith(STUDIO_BASE, { replace: true });
  });

  it("shows an inline error when passwords don't match", async () => {
    render(<ForcedPasswordChange />);
    const inputs = document.querySelectorAll("input");
    fireEvent.change(inputs[2]!, { target: { value: "ChangedAt#1" } });
    fireEvent.change(inputs[3]!, { target: { value: "Different#2" } });

    fireEvent.submit(screen.getByTestId("forced-password-change-form"));

    await screen.findByText(/don't match/i);
    expect(authApi.completePendingPasswordChange).not.toHaveBeenCalled();
  });
});
