// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import type { ForgeConnection } from "@/api/forgeConnections";

import { ConnectionCard } from "../integrations/ConnectionCard";
import { WebhookBaseRow } from "../integrations/WebhookBaseRow";

// The pin is the only way onto a forge whose outbound allowlist refuses this
// deployment's public URL, and the server accepts it ALONE: a body that also
// carried security_read_enabled is refused whole (two unrelated operations,
// nothing atomic between them). So these witnesses read the bytes that leave
// the studio, through the real api client — a mocked module function would say
// nothing about the wire.

type Call = { url: string; init?: RequestInit };

interface Answer {
  ok: boolean;
  status: number;
  body: unknown;
}

function mockFetch(answer: Answer = { ok: true, status: 200, body: { id: "c1" } }): Call[] {
  const calls: Call[] = [];
  globalThis.fetch = vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return {
      ok: answer.ok,
      status: answer.status,
      headers: { get: () => null },
      json: async () => answer.body,
      text: async () => JSON.stringify(answer.body),
    };
  }) as unknown as typeof fetch;
  return calls;
}

const connection = (over: Partial<ForgeConnection> = {}) =>
  ({
    id: "c1",
    provider: "gitlab",
    kind: "pat",
    status: "active",
    account_login: "iterion-bot",
    forge_base_url: "https://gitlab.example.gouv.fr",
    created_at: "2026-10-01T00:00:00Z",
    ...over,
  }) as ForgeConnection;

// The errors the operator was shown, in order — the empty string the row
// sends at the start of an attempt ("take the banner down") is not one.
const messages = (onError: ReturnType<typeof vi.fn>) =>
  onError.mock.calls.map(([m]) => m as string).filter((m) => m !== "");

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("WebhookBaseRow — pinning a connection's hook delivery base", () => {
  it("sends the pinned base as the ONLY field of its PATCH", async () => {
    const calls = mockFetch();
    const onChanged = vi.fn();
    const onError = vi.fn();
    render(
      <WebhookBaseRow
        teamID="t1"
        conn={connection()}
        canManage
        onChanged={onChanged}
        onError={onError}
      />,
    );

    // Unpinned: the row says where delivery goes today, and offers the pin.
    screen.getByText("this deployment's public URL");
    fireEvent.click(screen.getByRole("button", { name: "Pin a base URL" }));
    fireEvent.change(screen.getByLabelText("Webhook base URL"), {
      target: { value: "https://iterion.fabrique.social.gouv.fr" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(calls).toHaveLength(1));
    const [call] = calls;
    expect(call?.init?.method).toBe("PATCH");
    expect(call?.url).toBe("/api/teams/t1/forge/connections/c1");
    // Exactly one key: a security_read_enabled riding along would be a 400.
    expect(JSON.parse(String(call?.init?.body))).toEqual({
      webhook_base_url: "https://iterion.fabrique.social.gouv.fr",
    });

    // The connection list is refetched, and the editor closes on success.
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    expect(screen.queryByLabelText("Webhook base URL")).toBeNull();
    expect(messages(onError)).toEqual([]);
  });

  it("hands the connection back to the public URL with an EMPTY base", async () => {
    const calls = mockFetch();
    const onChanged = vi.fn();
    render(
      <WebhookBaseRow
        teamID="t1"
        conn={connection({ webhook_base_url: "https://iterion.fabrique.social.gouv.fr" })}
        canManage
        onChanged={onChanged}
        onError={vi.fn()}
      />,
    );

    // The pin itself is on screen — the operator reads the address, not a flag.
    screen.getByText("https://iterion.fabrique.social.gouv.fr");
    fireEvent.click(screen.getByRole("button", { name: "Unpin" }));

    await waitFor(() => expect(calls).toHaveLength(1));
    // "" is the CLEAR intent; omitting the field would patch nothing (400).
    expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({ webhook_base_url: "" });
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
  });

  it("shows a member the address and no way to change it", () => {
    mockFetch();
    render(
      <WebhookBaseRow
        teamID="t1"
        conn={connection({ webhook_base_url: "https://iterion.fabrique.social.gouv.fr" })}
        canManage={false}
        onChanged={vi.fn()}
        onError={vi.fn()}
      />,
    );

    // Readable: the delivery address explains the hook URLs on the repos.
    screen.getByText("https://iterion.fabrique.social.gouv.fr");
    // Not writable: the server answers 403, so no button may suggest it.
    expect(screen.queryAllByRole("button")).toHaveLength(0);
    expect(screen.queryByLabelText("Webhook base URL")).toBeNull();
  });

  it("renders nothing for a watch-only connection", () => {
    mockFetch();
    const { container } = render(
      <WebhookBaseRow
        teamID="t1"
        conn={connection({
          purpose: "security_read",
          webhook_base_url: "https://iterion.fabrique.social.gouv.fr",
        })}
        canManage
        onChanged={vi.fn()}
        onError={vi.fn()}
      />,
    );

    // It registers no webhook at all: a delivery base would be a lie.
    expect(container.innerHTML).toBe("");
    expect(screen.queryByText(/Webhook delivery/)).toBeNull();
  });

  it("is on the connection card, where the connection is administered", () => {
    mockFetch();
    render(
      <ConnectionCard
        teamID="t1"
        conn={connection({ webhook_base_url: "https://iterion.fabrique.social.gouv.fr" })}
        integrations={[]}
        repoBots={[]}
        canManage
        onChanged={vi.fn()}
        onError={vi.fn()}
        confirm={async () => true}
      />,
    );

    // The card is the only surface that names a connection: a pin reachable
    // from nowhere else is a CLI-only setting.
    screen.getByText("Webhook delivery:");
    screen.getByText("https://iterion.fabrique.social.gouv.fr");
    screen.getByRole("button", { name: "Change" });
  });

  it("reports the server's refusal and keeps the draft on screen", async () => {
    const calls = mockFetch({
      ok: false,
      status: 422,
      body: {
        error:
          'webhook_base_url must be scheme+host only (got path "/hooks") — the /api/webhooks/... route is appended to it',
      },
    });
    const onChanged = vi.fn();
    const onError = vi.fn();
    render(
      <WebhookBaseRow
        teamID="t1"
        conn={connection()}
        canManage
        onChanged={onChanged}
        onError={onError}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Pin a base URL" }));
    fireEvent.change(screen.getByLabelText("Webhook base URL"), {
      target: { value: "https://iterion.fabrique.social.gouv.fr/hooks" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(calls).toHaveLength(1));
    // The reason the server gave, verbatim enough to act on.
    await waitFor(() =>
      expect(messages(onError)).toEqual([expect.stringContaining("scheme+host only")]),
    );
    // Nothing was stored, so nothing is refetched…
    expect(onChanged).not.toHaveBeenCalled();
    // …and the attempt opened by clearing the channel: that empty string is
    // what takes an earlier banner off the card.
    expect(onError.mock.calls.map(([m]) => m)[0]).toBe("");
    // The draft survives the refusal — the operator fixes the URL in place.
    expect(screen.getByLabelText<HTMLInputElement>("Webhook base URL").value).toBe(
      "https://iterion.fabrique.social.gouv.fr/hooks",
    );
  });
});
