// @vitest-environment jsdom
//
// The launches in flight and their notices live in module-level stores, so
// they outlive every form — and must not outlive an account (#3416, rva round
// 4): one account's run is neither held nor named in the next one's session.
// Witnessed on the two ways an account leaves the browser.
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { cancelScheduledRefresh } from "@/api/client";
import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { botLaunchKey } from "@/lib/sendLaunch";
import { useLaunchesInFlight } from "@/store/launchesInFlight";
import { useUIStore } from "@/store/ui";

const ALICE = { id: "u-alice", email: "alice@example.test", status: "active", is_super_admin: false };
const ME = {
  user: ALICE,
  orgs: [
    {
      org_id: "o1",
      org_name: "Org",
      org_slug: "org",
      org_role: "admin",
      teams: [{ team_id: "ta", team_name: "TA", team_slug: "ta", role: "admin" }],
    },
  ],
  active_org_id: "o1",
  active_org_role: "admin",
  active_team_id: "ta",
  active_role: "admin",
};
let cookie: boolean;

function Probe() {
  const a = useAuth();
  return (
    <div>
      <span data-testid="state">{a.status}</span>
      <button onClick={() => void a.signOut()}>sign out</button>
      <button onClick={() => a.credentialSignedOut("the credential was revoked")}>credential gone</button>
    </div>
  );
}

beforeEach(() => {
  cookie = true;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL | Request) => {
      const path = String(input).split("?")[0] ?? "";
      const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
      if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
      if (path === "/api/auth/logout") {
        cookie = false;
        return new Response(null, { status: 204 });
      }
      if (path === "/api/auth/refresh") return json({ error: "auth: session not found" }, 401);
      if (path === "/api/auth/me") return cookie ? json(ME) : json({ error: "authentication required" }, 401);
      throw new Error(`unexpected ${path}`);
    }),
  );
  useLaunchesInFlight.setState({ byTarget: {} });
  useUIStore.setState({ toasts: [] });
});

afterEach(() => {
  cancelScheduledRefresh();
  cleanup();
  vi.unstubAllGlobals();
});

// A launch of this account in flight, the notice of one that answered off
// screen, and a notice that is about no launch.
function aliceLaunched() {
  useLaunchesInFlight.getState().begin(botLaunchKey("ta", "nestor"));
  useUIStore.getState().addToast("Run run-1 started for the bot nestor.", "success", {
    persistent: true,
    key: "launch:run-1",
  });
  useUIStore.getState().addToast("Saved.", "success", { persistent: true });
}
const left = () => ({
  inFlight: useLaunchesInFlight.getState().byTarget,
  toasts: useUIStore.getState().toasts.map((t) => t.message),
});

describe("an account that leaves the browser", () => {
  for (const way of ["sign out", "credential gone"]) {
    it(`takes its launches in flight and their notices with it — ${way}`, async () => {
      render(
        <AuthProvider>
          <Probe />
        </AuthProvider>,
      );
      await screen.findByText("authenticated");
      aliceLaunched();
      await act(async () => {
        screen.getByText(way).click();
      });
      await vi.waitFor(() => expect(screen.getByTestId("state").textContent).toBe("anonymous"));
      expect(left()).toEqual({ inFlight: {}, toasts: ["Saved."] });
    });
  }
});
