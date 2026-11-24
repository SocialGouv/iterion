// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { ForgeConnection } from "@/api/forgeConnections";

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return { ...actual, listForgeIntegrationHooks: vi.fn(async () => []) };
});

import { ConnectionCard } from "../integrations/ConnectionCard";

const conn = {
  id: "c1",
  provider: "gitlab",
  kind: "pat",
  status: "active",
  created_at: "2026-10-01T00:00:00Z",
} as ForgeConnection;

afterEach(cleanup);

describe("ConnectionCard — replace a PAT connection's token", () => {
  // The replacement token is typed into a password input. A browser ignores
  // autocomplete="off" there and fills the saved login's password, which the
  // row would then send to the forge connection as its token.
  it("keeps the browser's saved login out of the token input", () => {
    render(
      <ConnectionCard
        teamID="t1"
        conn={conn}
        integrations={[]}
        repoBots={[]}
        canManage
        onChanged={() => {}}
        onError={() => {}}
        confirm={async () => true}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Replace token" }));
    const token = screen.getByLabelText("New personal access token") as HTMLInputElement;
    expect(token.type).toBe("password");
    expect(token.getAttribute("autocomplete")).toBe("new-password");
  });
});
