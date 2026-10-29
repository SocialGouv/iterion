// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

// #2674: `iterion remote teams switch` opens /cli-auth with ?team=<id> — the
// page mints the token pinned to that team under the signed-in session and
// echoes team_id + the server-stated org_id back to the CLI's loopback.

const createPAT = vi.fn();
vi.mock("@/api/tokens", () => ({
  createPAT: (...args: unknown[]) => createPAT(...args),
}));
vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ user: { email: "ops@example.test" } }),
}));

import CliAuthPage from "@/views/CliAuthPage";

// stubLocation pins the query string wouter reads and captures the
// navigation the approval ends in (jsdom cannot navigate).
function stubLocation(search: string) {
  let href = "";
  Object.defineProperty(window, "location", {
    configurable: true,
    value: {
      search,
      get href() {
        return href;
      },
      set href(v: string) {
        href = v;
      },
    },
  });
  return () => href;
}

describe("CliAuthPage team pin", () => {
  afterEach(cleanup);

  it("mints pinned to the requested team and echoes team_id and org_id to the loopback", async () => {
    const navigated = stubLocation(
      "?redirect_uri=" +
        encodeURIComponent("http://127.0.0.1:41234/callback") +
        "&state=abc&team=team-b-uuid",
    );
    createPAT.mockResolvedValue({ token: "iap_minted", org_id: "o1" });
    render(<CliAuthPage />);

    expect(screen.getByText(/pinned to team/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Authorize" }));

    await waitFor(() => expect(createPAT).toHaveBeenCalledWith("iterion CLI", 0, "team-b-uuid"));
    await waitFor(() => {
      const u = new URL(navigated());
      expect(u.origin + u.pathname).toBe("http://127.0.0.1:41234/callback");
      expect(u.searchParams.get("token")).toBe("iap_minted");
      expect(u.searchParams.get("team_id")).toBe("team-b-uuid");
      expect(u.searchParams.get("org_id")).toBe("o1");
      expect(u.searchParams.get("state")).toBe("abc");
    });
  });

  it("mints unpinned without ?team= (the login flow's page)", async () => {
    const navigated = stubLocation(
      "?redirect_uri=" + encodeURIComponent("http://127.0.0.1:41234/callback") + "&state=abc",
    );
    createPAT.mockResolvedValue({ token: "iap_plain" });
    render(<CliAuthPage />);

    fireEvent.click(screen.getByRole("button", { name: "Authorize" }));

    await waitFor(() => expect(createPAT).toHaveBeenCalledWith("iterion CLI", 0, ""));
    await waitFor(() => {
      const u = new URL(navigated());
      expect(u.searchParams.get("token")).toBe("iap_plain");
      expect(u.searchParams.get("team_id")).toBeNull();
    });
  });
});
