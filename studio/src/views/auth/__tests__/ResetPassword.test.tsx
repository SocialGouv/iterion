// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

// The confirm answers the session alone (no org tree): the view hands the
// identity to completeSignIn, which reads /auth/me — never to a state built
// from the confirm's answer.
vi.mock("@/api/auth", async () => {
  const actual = await vi.importActual<typeof import("@/api/auth")>("@/api/auth");
  return {
    ...actual,
    confirmPasswordReset: vi.fn(async () => ({
      user: { id: "u", email: "a@b", status: "active", is_super_admin: false },
      active_team_id: "t1",
      active_role: "owner",
    })),
  };
});

const completeSignIn = vi.fn(async () => undefined);
const navigate = vi.fn();

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ completeSignIn }),
}));

vi.mock("wouter", () => ({
  useLocation: () => ["/auth/reset", navigate],
}));

import ResetPassword from "@/views/auth/ResetPassword";
import * as authApi from "@/api/auth";

beforeEach(() => {
  vi.clearAllMocks();
  window.history.replaceState({}, "", "/auth/reset?token=iar_reset-token");
});

afterEach(() => {
  cleanup();
});

describe("ResetPassword", () => {
  it("confirms the reset, then loads the identity of the session it delivered", async () => {
    render(<ResetPassword />);
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: "ChangedAt#1" } });
    fireEvent.change(screen.getByLabelText("Confirm new password"), { target: { value: "ChangedAt#1" } });
    fireEvent.submit(screen.getByLabelText("New password").closest("form") as HTMLFormElement);
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/", { replace: true }));
    expect(authApi.confirmPasswordReset).toHaveBeenCalledWith("iar_reset-token", "ChangedAt#1");
    expect(completeSignIn).toHaveBeenCalledTimes(1);
    // The reset is a credential change, not a chosen sign-in: the machine's
    // record stays OWED and resolves GONE — never ABANDONED unpresented.
    expect(completeSignIn).toHaveBeenCalledWith(undefined, "delivery");
  });
});
