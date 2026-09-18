// @vitest-environment jsdom
import { type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen } from "@testing-library/react";

import type { AuthResponse } from "@/api/auth";

// selectOrg/selectTeam must drop the OTHER scopes' run caches on switch so
// a poll response that lands under the previous scope's key during the
// switch round-trip can't later be served as authoritative. removeQueries
// targets stale keys (not the freshly-keyed active query, which refetches
// on the key change anyway), so it adds no redundant fetch — and on a
// cross-tenant switch it also means we never flash the previous org's runs.
const switchTeam = vi.fn<(id: string) => Promise<AuthResponse>>();
const switchOrg = vi.fn<(id: string) => Promise<AuthResponse>>();
const okResponse = {
  user: { id: "u", email: "e@x.test", status: "active", is_super_admin: true },
  orgs: [],
  active_role: "owner",
} as unknown as AuthResponse;

vi.mock("@/api/auth", () => ({
  ApiError: class ApiError extends Error {
    status: number;
    constructor(status: number) {
      super("api");
      this.status = status;
    }
  },
  getMe: vi.fn(async () => okResponse),
  login: vi.fn(),
  logout: vi.fn(),
  register: vi.fn(),
  refresh: vi.fn(),
  switchOrg: (id: string) => switchOrg(id),
  switchTeam: (id: string) => switchTeam(id),
}));

import { AuthProvider, useAuth } from "./AuthContext";

let capturedSelectTeam: (id: string) => Promise<void>;
let capturedSelectOrg: (id: string) => Promise<void>;

function Probe() {
  const { selectTeam, selectOrg } = useAuth();
  capturedSelectTeam = selectTeam;
  capturedSelectOrg = selectOrg;
  return <div>probe</div>;
}

beforeEach(() => {
  switchTeam.mockReset().mockResolvedValue(okResponse);
  switchOrg.mockReset().mockResolvedValue(okResponse);
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      new Response(JSON.stringify({ auth_required: false }), { status: 200 }),
    ),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

async function mountWithSpy() {
  const client = new QueryClient();
  const removeSpy = vi.spyOn(client, "removeQueries");
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <AuthProvider>{children}</AuthProvider>
    </QueryClientProvider>
  );
  render(<Probe />, { wrapper });
  await screen.findByText("probe");
  removeSpy.mockClear(); // ignore anything during bootstrap
  return removeSpy;
}

describe("scope switch drops other-scope run caches", () => {
  it("selectTeam removes [runs] and [run-repos] caches", async () => {
    const removeSpy = await mountWithSpy();
    await act(async () => {
      await capturedSelectTeam("team-b");
    });
    const keys = removeSpy.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(["runs"]);
    expect(keys).toContainEqual(["run-repos"]);
  });

  it("selectOrg removes [runs] and [run-repos] caches", async () => {
    const removeSpy = await mountWithSpy();
    await act(async () => {
      await capturedSelectOrg("org-b");
    });
    const keys = removeSpy.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(["runs"]);
    expect(keys).toContainEqual(["run-repos"]);
  });
});
