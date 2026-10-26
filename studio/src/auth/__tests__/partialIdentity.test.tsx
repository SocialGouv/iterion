// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AuthProvider, useAuth } from "@/auth/AuthContext";

function Probe() {
  const { status, activeOrgID, activeOrgRole, activeTeamID, activeRole, isRestricted } = useAuth();
  return <span>{status}|{activeOrgID}|{activeOrgRole ?? "none"}|{activeTeamID}|{activeRole}|restricted={String(isRestricted)}</span>;
}

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it.each([[true, "admin"], [false, "admin"], [true, "config_editor"], [false, "config_editor"]] as const)("preserves unavailable identity and the credential's shell role (healthy sibling=%s, role=%s)", async (sibling, role) => {
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input);
    const json = (v: unknown) => new Response(JSON.stringify(v));
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path !== "/api/auth/me") throw new Error(`Unexpected request: ${path}`);
    return json({
      user: { id: "u1", email: "one@example.test", status: "active", is_super_admin: false },
      active_org_id: "broken", active_org_role: "owner", active_team_id: "hidden", active_role: role,
      orgs: [
        { org_id: "broken", org_name: "", org_slug: "", org_role: "", unavailable: true, teams: [] },
        ...(sibling ? [{ org_id: "good", org_name: "Healthy", org_slug: "healthy", org_role: "member", teams: [] }] : []),
      ],
    });
  }));
  render(<AuthProvider><Probe /></AuthProvider>);
  expect(await screen.findByText(`authenticated|broken|none|hidden|${role}|restricted=false`)).toBeTruthy();
});
