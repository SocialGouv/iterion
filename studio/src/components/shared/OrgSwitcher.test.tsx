// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import type { MembershipView, OrgTreeView } from "@/api/auth";

import OrgSwitcher from "./OrgSwitcher";

const mocks = vi.hoisted(() => ({
  navigate: vi.fn(),
  selectOrg: vi.fn(),
  selectTeam: vi.fn(),
}));

let mockOrgs: OrgTreeView[] = [];
let mockTeams: MembershipView[] = [];

vi.mock("@/auth/AuthContext", () => ({
  hasOrgRole: () => true,
  useAuth: () => ({
    user: {
      id: "user-1",
      email: "user@example.com",
      status: "active",
      is_super_admin: true,
    },
    orgs: mockOrgs,
    activeOrg: mockOrgs[0],
    activeOrgID: mockOrgs[0]?.org_id ?? "",
    activeOrgRole: "owner",
    activeTeamID: mockTeams[0]?.team_id ?? "",
    teams: mockTeams,
    selectOrg: mocks.selectOrg,
    selectTeam: mocks.selectTeam,
  }),
}));

vi.mock("wouter", () => ({
  useLocation: () => ["/", mocks.navigate],
}));

beforeAll(() => {
  class RO {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  vi.stubGlobal("ResizeObserver", RO);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function orgs(count: number): OrgTreeView[] {
  return Array.from({ length: count }, (_, index) => ({
    org_id: `org-${index + 1}`,
    org_name: `Organization ${index + 1}`,
    org_slug: `organization-${index + 1}`,
    org_role: "member",
    teams: [],
  }));
}

function teams(count: number): MembershipView[] {
  return Array.from({ length: count }, (_, index) => ({
    team_id: `team-${index + 1}`,
    team_name: `Team ${index + 1}`,
    team_slug: `team-${index + 1}`,
    role: "member",
  }));
}

function openSwitcher() {
  render(<OrgSwitcher />);
  fireEvent.click(screen.getByRole("button", { name: /Switch organization/ }));
  return screen.getByRole("dialog");
}

describe("OrgSwitcher", () => {
  it("bounds the entire long popover and keeps the last organization clickable", () => {
    mockOrgs = orgs(40);
    mockTeams = teams(2);
    const content = openSwitcher();

    expect(
      content.classList.contains("max-h-[var(--radix-popover-content-available-height)]"),
    ).toBe(true);
    expect(content.classList.contains("overflow-y-auto")).toBe(true);
    expect(within(content).getByText("Teams")).toBeTruthy();
    expect(within(content).getByRole("button", { name: "Organization settings" })).toBeTruthy();
    expect(within(content).getByRole("button", { name: /New organization/ })).toBeTruthy();

    fireEvent.click(within(content).getByRole("button", { name: /Organization 40/ }));

    expect(mocks.selectOrg).toHaveBeenCalledWith("org-40");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens and renders a short organization list", () => {
    mockOrgs = orgs(2);
    mockTeams = teams(1);
    const content = openSwitcher();

    expect(within(content).getByRole("button", { name: /Organization 1/ })).toBeTruthy();
    expect(within(content).getByRole("button", { name: /Organization 2/ })).toBeTruthy();
    expect(within(content).getByRole("button", { name: "Organization settings" })).toBeTruthy();
  });

  it("names unavailable organizations, refuses selection and keeps healthy siblings usable", () => {
    mockOrgs = orgs(2);
    mockOrgs[0] = { org_id: "org-1", org_name: "", org_slug: "org-1", org_role: "", teams: [], unavailable: true };
    mockTeams = [];
    const content = openSwitcher();
    const unavailable = within(content).getByRole("button", { name: /Organization org-1 unavailable/ });
    expect((unavailable as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(unavailable);
    expect(mocks.selectOrg).not.toHaveBeenCalled();
    expect(within(content).queryByText(/No teams in this organization/)).toBeNull();
    expect(within(content).queryByRole("button", { name: "Organization settings" })).toBeNull();
    fireEvent.click(within(content).getByRole("button", { name: /Organization 2/ }));
    expect(mocks.selectOrg).toHaveBeenCalledWith("org-2");
  });
});
