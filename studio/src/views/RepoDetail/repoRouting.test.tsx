// @vitest-environment jsdom
// The repo's routing editor (ADR-129): the shared strict-JSON editor at
// the binding level. Three properties make the write honest: an
// unedited editor stays quiet, a parse-broken draft refuses to save, and
// what leaves the page is the PARSED draft — an object replaces, null
// clears.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import type { ForgeIntegration } from "@/api/forgeConnections";
import { updateForgeRepoBots } from "@/api/forgeConnections";

import RepoRoutingSection from "./RepoRoutingSection";

afterEach(cleanup);
beforeEach(() => {
  // The module-level mock accumulates calls across tests; the "stays
  // quiet" witness needs its silence counted from zero.
  vi.clearAllMocks();
});

vi.mock("@/api/forgeConnections", async () => {
  const actual = await vi.importActual<typeof import("@/api/forgeConnections")>(
    "@/api/forgeConnections",
  );
  return {
    ...actual,
    updateForgeRepoBots: vi.fn(async () => ({ integration_id: "ri-1", webhook_id: "wh", bot_ids: [], created: false })),
  };
});

const integration = (routing?: Record<string, unknown> | null): ForgeIntegration => ({
  id: "ri-1",
  connection_id: "c1",
  provider: "gitlab",
  repo_full_name: "org/api",
  bot_ids: ["review-pr"],
  events_normalized: ["merge_request"],
  webhook_id: "wh-1",
  hook_id: "h-1",
  routing,
  created_at: new Date().toISOString(),
});

describe("RepoRoutingSection", () => {
  it("saves the parsed object, replacing the stored block wholesale", async () => {
    const onSaved = vi.fn(async () => {});
    render(
      <RepoRoutingSection
        teamID="t1"
        integration={integration({ overrides: [{ selector: "*", backend: "claw" }] })}
        canManage
        onSaved={onSaved}
      />,
    );

    const box = screen.getByLabelText(/Routing policy/i) as HTMLTextAreaElement;
    fireEvent.change(box, {
      target: { value: JSON.stringify({ providers: ["anthropic"] }, null, 2) },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(updateForgeRepoBots).toHaveBeenCalledWith(
        "t1",
        "ri-1",
        undefined,
        undefined,
        undefined,
        // The saved draft is the WHOLE truth: the stored overrides cell is
        // gone — never a per-key merge on the client.
        { providers: ["anthropic"] },
      );
    });
    expect(onSaved).toHaveBeenCalled();
  });

  it("saves null to clear the block", async () => {
    const onSaved = vi.fn(async () => {});
    render(
      <RepoRoutingSection
        teamID="t1"
        integration={integration({ providers: ["anthropic"] })}
        canManage
        onSaved={onSaved}
      />,
    );

    const box = screen.getByLabelText(/Routing policy/i) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "null" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(updateForgeRepoBots).toHaveBeenCalledWith(
        "t1",
        "ri-1",
        undefined,
        undefined,
        undefined,
        null,
      );
    });
  });

  it("refuses to save a parse-broken draft and stays quiet until edited", async () => {
    const onSaved = vi.fn(async () => {});
    render(
      <RepoRoutingSection teamID="t1" integration={integration(null)} canManage onSaved={onSaved} />,
    );

    // Unedited: Save is disabled — an editor that saved the echoed stored
    // block would bump the server's write path for nothing.
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);

    const box = screen.getByLabelText(/Routing policy/i) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: '{"providers": [anthropic]' } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(
      true,
    );

    fireEvent.change(box, { target: { value: '{"providers":["anthropic"]}' } });
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(
      false,
    );
    expect(updateForgeRepoBots).not.toHaveBeenCalled();
    expect(onSaved).not.toHaveBeenCalled();
  });

  it("tells the operator a save was PARKED for org approval — a 202 is not a clean write", async () => {
    vi.mocked(updateForgeRepoBots).mockResolvedValueOnce({
      integration_id: "",
      webhook_id: "",
      hook_id: "",
      managed_secret_id: "",
      bot_ids: [],
      created: false,
      pending_approval: true,
      approval_id: "pa-123",
      detail: "an org admin must approve this routing change before it lands",
    });
    const onSaved = vi.fn(async () => {});
    render(
      <RepoRoutingSection
        teamID="t1"
        integration={integration({ providers: ["anthropic"] })}
        canManage
        onSaved={onSaved}
      />,
    );

    const box = screen.getByLabelText(/Routing policy/i) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: '{"providers":["claw"]}' } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      // The parked request must be VISIBLE: the approval id and the server's
      // detail reach the operator, so a parked save is never mistaken for a
      // pin in force.
      expect(screen.getByText(/pa-123/)).toBeTruthy();
      expect(screen.getByText(/org admin must approve/)).toBeTruthy();
    });
  });

  it("renders the blast-radius warnings a direct write carries", async () => {
    vi.mocked(updateForgeRepoBots).mockResolvedValueOnce({
      integration_id: "ri-1",
      webhook_id: "wh",
      hook_id: "h",
      managed_secret_id: "",
      bot_ids: ["review-pr"],
      created: false,
      warnings: [
        'routing: selector "*" pins backend "byo" — bot "review-pr" node "review" declares ask-rules the "byo" backend cannot gate',
      ],
    });
    const onSaved = vi.fn(async () => {});
    render(
      <RepoRoutingSection
        teamID="t1"
        integration={integration(null)}
        canManage
        onSaved={onSaved}
      />,
    );

    const box = screen.getByLabelText(/Routing policy/i) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: '{"overrides":[{"selector":"*","backend":"byo"}]}' } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(screen.getByText(/ask-rules/)).toBeTruthy();
    });
  });

  it("renders read-only for a member who cannot manage", () => {
    render(
      <RepoRoutingSection
        teamID="t1"
        integration={integration(null)}
        canManage={false}
        onSaved={vi.fn()}
      />,
    );
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect((screen.getByLabelText(/Routing policy/i) as HTMLTextAreaElement).readOnly).toBe(true);
  });
});
