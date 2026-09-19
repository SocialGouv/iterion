// @vitest-environment jsdom
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { RequireSuperAdmin } from "./RequireSuperAdmin";

// The guard reads exactly two things: the user's is_super_admin from useAuth,
// and the deployment mode from the server-info store. Mock both so each tier
// can be exercised without the whole provider tree / network.
let mockUser: { is_super_admin?: boolean } | null = null;
let mockInfo: { mode?: string } | null = null;

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ user: mockUser }),
}));
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (s: { info: unknown }) => unknown) => sel({ info: mockInfo }),
}));
vi.mock("@/components/shared/CloudOnlyNotice", () => ({
  CloudOnlyNotice: ({ feature }: { feature: string }) => <div>cloud-only: {feature}</div>,
}));

function renderGuard(children: ReactNode) {
  return render(<RequireSuperAdmin>{children}</RequireSuperAdmin>);
}

afterEach(() => {
  cleanup();
  mockUser = null;
  mockInfo = null;
});

describe("RequireSuperAdmin", () => {
  it("renders the children for a super-admin in cloud mode", () => {
    mockUser = { is_super_admin: true };
    mockInfo = { mode: "cloud" };
    renderGuard(<div>admin content</div>);
    expect(screen.getByText("admin content")).toBeTruthy();
  });

  it("shows the cloud-only notice in local mode (never the page)", () => {
    mockUser = { is_super_admin: true }; // local synthesises a super-admin
    mockInfo = { mode: "local" };
    renderGuard(<div>admin content</div>);
    expect(screen.queryByText("admin content")).toBeNull();
    expect(screen.getByText(/cloud-only:/)).toBeTruthy();
  });

  it("shows a 403 notice for a signed-in non-super-admin in cloud mode", () => {
    mockUser = { is_super_admin: false };
    mockInfo = { mode: "cloud" };
    renderGuard(<div>admin content</div>);
    expect(screen.queryByText("admin content")).toBeNull();
    expect(screen.getByText(/super-admin only/i)).toBeTruthy();
  });

  it("renders children while server_info is still loading (each page self-gates)", () => {
    mockUser = { is_super_admin: true };
    mockInfo = null;
    renderGuard(<div>admin content</div>);
    expect(screen.getByText("admin content")).toBeTruthy();
  });
});
