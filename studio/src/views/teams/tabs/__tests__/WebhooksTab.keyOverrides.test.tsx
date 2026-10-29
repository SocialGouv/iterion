// @vitest-environment jsdom
// A webhook's key_overrides (the per-repo credential pin of #2005) was
// settable only through `iterion remote webhooks update --data`: the studio
// typed the field and no .tsx referenced it (#2648). The editor is only
// trustworthy if three things hold, so they are the assertions: the picker of
// a provider offers that provider's keys and NO other (the pin pkg/server
// refuses is the pin the UI must not be able to make), the save puts
// `key_overrides` on the wire, and the preview row names the key a delivery
// would seal — re-read from the server after a save, never the resolution of
// the map that was just replaced.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { ApiKeyView } from "@/api/byok";
import type { WebhookConfig } from "@/api/webhooks";

const webhook = (over: Partial<WebhookConfig> = {}): WebhookConfig =>
  ({
    id: "wh_42",
    tenant_id: "team_1",
    name: "Demo",
    provider: "gitlab",
    enabled: true,
    token_last4: "abcd",
    fingerprint: "fp",
    bot_ids: ["whats-next"],
    wildcard_bots: false,
    default_bot_id: "whats-next",
    project_allowlist: ["org/api"],
    rate_limit: { rate: 1, burst: 10 },
    created_by: "u",
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-01T00:00:00Z",
    ...over,
  }) as WebhookConfig;

const key = (over: Partial<ApiKeyView>): ApiKeyView =>
  ({
    id: "k1",
    provider: "anthropic",
    name: "team key",
    last4: "1234",
    is_default: false,
    created_at: "2026-10-01T00:00:00Z",
    bots: [],
    repos: [],
    ...over,
  }) as ApiKeyView;

let stored: WebhookConfig = webhook();
let teamKeys: ApiKeyView[] = [];
/** The label of the candidate the server says is selected, per preview read. */
let sealedLabels: string[] = [];

vi.mock("@/api/webhooks", async () => {
  const actual = await vi.importActual<typeof import("@/api/webhooks")>("@/api/webhooks");
  return {
    ...actual,
    listWebhooks: vi.fn(async () => [stored]),
    createWebhook: vi.fn(),
    rotateWebhook: vi.fn(),
    deleteWebhook: vi.fn(),
    updateWebhook: vi.fn(async (_t: string, _id: string, input: { key_overrides?: Record<string, string> }) => {
      stored = webhook({ ...stored, key_overrides: input.key_overrides });
      return stored;
    }),
    listWebhookDeliveries: vi.fn(async () => []),
  };
});

vi.mock("@/api/byok", async () => {
  const actual = await vi.importActual<typeof import("@/api/byok")>("@/api/byok");
  return { ...actual, listApiKeys: vi.fn(async () => teamKeys) };
});

vi.mock("@/api/credentialPreview", () => ({
  // One label per read, so a stale preview is visible as a stale NAME.
  previewCredentials: vi.fn(async () => ({
    observed_at: "2026-10-29T00:00:00Z",
    context: {},
    wires: [],
    candidates: [
      {
        id: "cand",
        tier: "team",
        source: "byok",
        provider: "anthropic",
        wire: "ANTHROPIC_API_KEY",
        rank: 1,
        label: sealedLabels.shift() ?? "no more reads",
        pinned: true,
        state: "available",
        selection: "pinned",
        selected: true,
        conditional: false,
        windows: [],
      },
    ],
    pool: {},
    warnings: [],
  })),
}));

vi.mock("@/api/bots", () => ({
  listBots: vi.fn(async () => [{ name: "whats-next", display_name: "Nexie", path: "/x" }]),
}));

import * as webhooksApi from "@/api/webhooks";
import WebhooksTab from "@/views/teams/tabs/WebhooksTab";

const renderTab = () =>
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <WebhooksTab teamID="team_1" canManage />
    </QueryClientProvider>,
  );

const openPins = async () => {
  renderTab();
  await screen.findByText("Demo");
  fireEvent.click(screen.getByRole("button", { name: /^Credential pins/ }));
  return screen.findByRole("dialog", { name: /Credential pins — Demo/ });
};

beforeEach(() => {
  vi.clearAllMocks();
  stored = webhook();
  teamKeys = [
    key({ id: "k_anthropic", provider: "anthropic", name: "anthropic team" }),
    key({ id: "k_openai", provider: "openai", name: "openai team" }),
    key({
      id: "k_gateway",
      provider: "openai_compatible",
      name: "pliage gateway",
      base_url: "https://gw.invalid",
    }),
  ];
  sealedLabels = ["sealed: the org default", "sealed: the org default"];
});

afterEach(cleanup);

describe("WebhooksTab — the key_overrides editor", () => {
  it("offers each provider only its own keys, and never a key no run can spend", async () => {
    const dialog = await openPins();

    const anthropic = await within(dialog).findByLabelText<HTMLSelectElement>(
      "Key for anthropic",
    );
    const anthropicOptions = [...anthropic.options].map((o) => o.textContent ?? "");
    expect(anthropicOptions.some((o) => o.includes("anthropic team"))).toBe(true);
    // The refusal validateKeyOverrides answers ("api key is for provider …")
    // must be unreachable from the picker, not merely reported after a save.
    expect(anthropicOptions.some((o) => o.includes("openai team"))).toBe(false);

    const openai = within(dialog).getByLabelText<HTMLSelectElement>("Key for openai");
    expect([...openai.options].some((o) => (o.textContent ?? "").includes("anthropic team"))).toBe(
      false,
    );

    // A gateway key is storable and no run spends it yet (#2005 PR-C'), so
    // pkg/server refuses the pin: the dialog offers no picker for it at all.
    expect(within(dialog).queryByLabelText("Key for openai_compatible")).toBeNull();
  });

  it("sends key_overrides on the wire, and only the pins that are set", async () => {
    const dialog = await openPins();

    const anthropic = await within(dialog).findByLabelText<HTMLSelectElement>(
      "Key for anthropic",
    );
    fireEvent.change(anthropic, { target: { value: "k_anthropic" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save pins" }));

    await waitFor(() => expect(webhooksApi.updateWebhook).toHaveBeenCalledTimes(1));
    expect(webhooksApi.updateWebhook).toHaveBeenCalledWith("team_1", "wh_42", {
      key_overrides: { anthropic: "k_anthropic" },
    });
  });

  it("drops a cleared pin from the map instead of sending an empty key id", async () => {
    stored = webhook({ key_overrides: { anthropic: "k_anthropic" } });
    const dialog = await openPins();

    const anthropic = await within(dialog).findByLabelText<HTMLSelectElement>(
      "Key for anthropic",
    );
    expect(anthropic.value).toBe("k_anthropic");
    fireEvent.change(anthropic, { target: { value: "" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save pins" }));

    await waitFor(() => expect(webhooksApi.updateWebhook).toHaveBeenCalledTimes(1));
    // An empty string would be looked up as a key id by the resolver.
    expect(webhooksApi.updateWebhook).toHaveBeenCalledWith("team_1", "wh_42", {
      key_overrides: {},
    });
  });

  it("names the key a delivery would seal, re-read after the save", async () => {
    // Two distinct answers from the server, and neither is a substring of a
    // picker option: an assertion that could match the <option> would pass on
    // a preview that never refreshed.
    sealedLabels = ["sealed: before the save", "sealed: after the save"];
    const dialog = await openPins();

    // Before the save: the resolution of the STORED pins, read INSIDE the
    // preview section.
    const preview = within(dialog).getByTestId("webhook-sealed-key");
    await waitFor(() => expect(preview.textContent).toContain("sealed: before the save"));

    const anthropic = within(dialog).getByLabelText<HTMLSelectElement>("Key for anthropic");
    fireEvent.change(anthropic, { target: { value: "k_anthropic" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save pins" }));

    // After it: the second read, which is the only one that can name the key
    // the STORED pin resolves to.
    await waitFor(() =>
      expect(
        within(dialog).getByTestId("webhook-sealed-key").textContent,
      ).toContain("sealed: after the save"),
    );
  });

  it("warns when a pinned key's repository audience excludes this webhook's repo", async () => {
    teamKeys = [
      key({ id: "k_anthropic", provider: "anthropic", name: "anthropic team", repos: ["org/web"] }),
    ];
    stored = webhook({ key_overrides: { anthropic: "k_anthropic" } });
    const dialog = await openPins();

    // Stored fine, withheld at every launch: said before the next save, not
    // discovered in a run's credential log.
    await within(dialog).findByText(/restricted to org\/web.*serves org\/api/s);
  });

  it("does not warn on a case-only difference the server treats as a match", async () => {
    // secrets.ApiKey.ServesRepoForLaunch matches case-insensitively: a warning
    // here would name a key every delivery actually spends.
    teamKeys = [
      key({ id: "k_anthropic", provider: "anthropic", name: "anthropic team", repos: ["Org/API"] }),
    ];
    stored = webhook({ key_overrides: { anthropic: "k_anthropic" } });
    const dialog = await openPins();

    await within(dialog).findByLabelText("Key for anthropic");
    expect(within(dialog).queryByText(/every delivery would find it withheld/)).toBeNull();
  });

  it("says why there is no preview for an ambiguous webhook", async () => {
    stored = webhook({ bot_ids: ["a", "b"], default_bot_id: undefined });
    const dialog = await openPins();

    await within(dialog).findByText(/no single resolution to preview/i);
  });
});
