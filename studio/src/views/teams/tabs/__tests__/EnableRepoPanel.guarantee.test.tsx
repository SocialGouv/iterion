// @vitest-environment jsdom
// The guarantee the Enable panel owes, over EVERY state it can be in.
//
// Enabling a repo registers a webhook at an address and pins a merge-gate
// posture that survives re-provisioning. Both are read from answers that
// arrive late and can fail, and each round of review found one more state
// where a missing answer was read as a fact ("[] integrations" = a new repo,
// "no placeholder" = a fresh preview, a caller's snapshot = a current list).
// So the invariant is checked as a matrix — the panel's two unknowns crossed —
// rather than state by state:
//
//   * the posture radios render ONLY with a fresh integrations list that does
//     not carry this repo AND a fresh preview that gates merges;
//   * `launch_vars` travels ONLY when "Advisory only" is the visible checked
//     radio — never from a posture no one could see;
//   * Enable is disabled whenever there is no fresh preview for the current
//     selection, so no hook is registered at an unread address — and while a
//     gating selection's posture cannot be decided (the integrations are being
//     read and have not failed), so the default posture is never sent in place
//     of a choice on screen.
//
// The integrations axis is the panel's OWN read: the freshness of a list is a
// fact about the read that produced it, so the panel holds that read rather
// than taking a list from a caller. Its states are the states of that one
// cache entry.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { BotEntryWithSchema } from "@/api/bots";
import type {
  ForgeConnection,
  ForgeEnablePreview,
  ForgeIntegration,
  ForgeRepo,
} from "@/api/forgeConnections";

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    listForgeIntegrations: vi.fn(),
    listForgeRepos: vi.fn(),
    previewForgeEnable: vi.fn(),
    enableForgeRepoBots: vi.fn(),
  };
});

import {
  enableForgeRepoBots,
  listForgeIntegrations,
  listForgeRepos,
  previewForgeEnable,
} from "@/api/forgeConnections";

import { EnableRepoPanel } from "../integrations/EnableRepoPanel";

const reposMock = vi.mocked(listForgeRepos);
const previewMock = vi.mocked(previewForgeEnable);
const enableMock = vi.mocked(enableForgeRepoBots);
const integrationsMock = vi.mocked(listForgeIntegrations);

const conn = {
  id: "c1",
  provider: "gitlab",
  kind: "pat",
  status: "active",
  account_login: "iterion-bot",
  created_at: "2026-10-01T00:00:00Z",
} as ForgeConnection;

const reviewBot = {
  name: "review-pr",
  display_name: "Revi",
  invocations: [{ kind: "forge", forge: { event: "merge_request" } }],
} as unknown as BotEntryWithSchema;

const converseBot = {
  name: "revi-converse",
  display_name: "Revi (converse)",
  invocations: [{ kind: "command", command: { name: "revi" } }],
} as unknown as BotEntryWithSchema;

const preview = (over: Partial<ForgeEnablePreview> = {}): ForgeEnablePreview => ({
  events_normalized: ["merge_request"],
  forge_native_events: ["merge_request"],
  scopes: {},
  secrets: [],
  commands: [],
  identity: { handle: "iterion-bot", provider: "gitlab", base_url: "https://gitlab.example" },
  gates_merges: true,
  hook_base_url: "https://iterion.fabrique.social.gouv.fr",
  conflicts: [],
  ...over,
});

// The repo IS integrated here, advisory-only, and pins a review tier that a
// restated launch_vars would drop — the state a posture offered by mistake
// turns into a refused enable.
const repoIntegration = {
  id: "i1",
  connection_id: "c1",
  provider: "gitlab",
  repo_full_name: "org/api",
  bot_ids: ["review-pr"],
  events_normalized: ["merge_request"],
  webhook_id: "w1",
  hook_id: "h1",
  created_at: "2026-10-01T00:00:00Z",
  launch_vars: { gate_enabled: "false", review_tier: "audit" },
} as ForgeIntegration;

const ADDRESS = "https://iterion.fabrique.social.gouv.fr/api/webhooks/gitlab/…";
const INTEGRATIONS_KEY = ["forge-integrations", "t1"];
const UNREAD_LINE = /this team.s enabled repos could not be read/;
const pending = <T,>() => new Promise<T>(() => {});
const checked = (el: HTMLElement) => (el as HTMLInputElement).checked;
const newClient = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });

/** A state of the panel's own integrations read, and what that state IS. */
interface IntegrationsCase {
  name: string;
  /** How the read answers. */
  read: "none" | "thisRepo" | "pending" | "rejects";
  /** A snapshot an earlier mount left under the shared cache key — what
   *  query-core keeps beside an in-flight refetch, and beside its error. It
   *  predates this repo's integration, which is what makes it dangerous. */
  seedCache?: ForgeIntegration[];
  /** True when the panel holds a list it has seen settle. */
  fresh: boolean;
  /** True when that settled list carries an integration for the selected repo. */
  integrated: boolean;
  /** True when the panel can name why it knows nothing. */
  failed?: boolean;
}

const INTEGRATIONS_CASES: IntegrationsCase[] = [
  { name: "a fresh read carries no integration for this repo", read: "none", fresh: true, integrated: false },
  {
    name: "a fresh read carries this repo's integration",
    read: "thisRepo",
    fresh: true,
    integrated: true,
  },
  { name: "the first read is in flight", read: "pending", fresh: false, integrated: false },
  {
    name: "a stale snapshot is being refetched",
    read: "pending",
    seedCache: [],
    fresh: false,
    integrated: false,
  },
  {
    name: "the read failed with no list at all",
    read: "rejects",
    fresh: false,
    integrated: false,
    failed: true,
  },
  {
    name: "the read failed next to the stale snapshot it kept",
    read: "rejects",
    seedCache: [],
    fresh: false,
    integrated: false,
    failed: true,
  },
];

/** How the preview for the current selection reaches the panel. */
interface PreviewCase {
  name: string;
  drive: "noBot" | "firstInFlight" | "keptInFlight" | "rejects" | "fresh" | "freshNoGate";
  /** True when the panel holds a preview for THIS selection. */
  fresh: boolean;
  /** True when that preview says a selected bot posts a merge-gate status. */
  gating: boolean;
}

const PREVIEW_CASES: PreviewCase[] = [
  { name: "no preview is asked for yet", drive: "noBot", fresh: false, gating: false },
  { name: "the first preview is in flight", drive: "firstInFlight", fresh: false, gating: false },
  {
    name: "a preview replacing a kept answer is in flight",
    drive: "keptInFlight",
    fresh: false,
    gating: false,
  },
  { name: "the preview failed", drive: "rejects", fresh: false, gating: false },
  { name: "a fresh preview gates merges", drive: "fresh", fresh: true, gating: true },
  { name: "a fresh preview gates nothing", drive: "freshNoGate", fresh: true, gating: false },
];

async function mountCase(ic: IntegrationsCase, pc: PreviewCase) {
  const client = newClient();
  if (ic.seedCache) client.setQueryData(INTEGRATIONS_KEY, ic.seedCache);
  switch (ic.read) {
    case "none":
      integrationsMock.mockResolvedValue([]);
      break;
    case "thisRepo":
      integrationsMock.mockResolvedValue([repoIntegration]);
      break;
    case "pending":
      integrationsMock.mockImplementation(() => pending());
      break;
    case "rejects":
      integrationsMock.mockRejectedValue(new Error("API error 500: store down"));
      break;
  }

  switch (pc.drive) {
    case "keptInFlight":
      previewMock.mockResolvedValueOnce(preview()).mockImplementation(() => pending());
      break;
    case "firstInFlight":
      previewMock.mockImplementation(() => pending());
      break;
    case "rejects":
      previewMock.mockRejectedValue(new Error("API error 502: upstream"));
      break;
    case "freshNoGate":
      previewMock.mockResolvedValue(preview({ gates_merges: false }));
      break;
    default:
      previewMock.mockResolvedValue(preview());
  }

  render(
    <QueryClientProvider client={client}>
      <EnableRepoPanel
        teamID="t1"
        conn={conn}
        repoBots={[reviewBot, converseBot]}
        onDone={vi.fn()}
        onCancel={vi.fn()}
        onError={vi.fn()}
      />
    </QueryClientProvider>,
  );

  await screen.findByRole("option", { name: /org\/api/ });
  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "org/api" } });
  if (pc.drive !== "noBot") {
    fireEvent.click(screen.getByRole("checkbox", { name: /review-pr/ }));
    await waitFor(() => expect(previewMock).toHaveBeenCalledTimes(1));
  }

  // Drive BOTH axes to their steady state through signals of the state itself,
  // so a later assertion of ABSENCE cannot be an assertion made too early.
  if (pc.drive === "fresh" || pc.drive === "freshNoGate") {
    await screen.findByText(ADDRESS);
  } else if (pc.drive === "rejects") {
    await screen.findByText(/could not be read \(API error 502: upstream\)/);
  } else if (pc.drive === "keptInFlight") {
    await screen.findByText(ADDRESS);
    // A second selection: query-core keeps the first answer on screen as a
    // placeholder, and it describes the selection the operator just left.
    fireEvent.click(screen.getByRole("checkbox", { name: /revi-converse/ }));
    await waitFor(() => expect(previewMock).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByText(ADDRESS)).toBeNull());
  }

  if (ic.read === "pending") {
    await waitFor(() => expect(client.isFetching({ queryKey: INTEGRATIONS_KEY })).toBe(1));
  } else {
    const status = ic.read === "rejects" ? "error" : "success";
    await waitFor(() => expect(client.getQueryState(INTEGRATIONS_KEY)?.status).toBe(status));
    await waitFor(() => expect(client.isFetching({ queryKey: INTEGRATIONS_KEY })).toBe(0));
  }
}

beforeEach(() => {
  vi.clearAllMocks();
  reposMock.mockResolvedValue([{ full_name: "org/api", can_admin: true } as ForgeRepo]);
  enableMock.mockResolvedValue({
    integration_id: "i1",
    webhook_id: "w1",
    hook_id: "h1",
    managed_secret_id: "s1",
    bot_ids: ["review-pr"],
    created: true,
  });
});

afterEach(cleanup);

describe("EnableRepoPanel — the guarantee, over the integrations × preview matrix", () => {
  for (const ic of INTEGRATIONS_CASES) {
    for (const pc of PREVIEW_CASES) {
      // What the invariant demands of THIS row, derived from the two states —
      // never from what the panel happens to render.
      const offersPosture = ic.fresh && !ic.integrated && pc.fresh && pc.gating;
      const showsStoredPosture = ic.fresh && ic.integrated;
      // The panel names the failed read where it would otherwise offer the
      // choice: a disabled-looking screen that says nothing is a bug report.
      const saysUnread = !!ic.failed && pc.fresh && pc.gating;
      // A gating selection whose integrations are still being read: the
      // posture is undecided, so Enable waits rather than sending the default.
      const postureUndecided = pc.fresh && pc.gating && !ic.fresh && !ic.failed;
      const enableDisabled = !pc.fresh || postureUndecided;

      it(`${ic.name}, ${pc.name}`, async () => {
        await mountCase(ic, pc);

        // 1. The radios: a choice only for a repo this connection provably
        //    does not carry yet, while a fresh answer says a bot gates.
        const radio = (name: RegExp) => screen.queryByRole("radio", { name });
        if (offersPosture) {
          await waitFor(() => expect(radio(/Gate merges/)).not.toBeNull());
          expect(checked(screen.getByRole("radio", { name: /Gate merges/ }))).toBe(true);
        } else {
          expect(radio(/Gate merges/)).toBeNull();
          expect(radio(/Advisory only/)).toBeNull();
        }

        // 2. The stored posture of an integrated repo is read, not offered.
        if (showsStoredPosture) {
          await screen.findByText(/Merge gate: Advisory only/);
          expect(
            screen.getByRole("link", { name: "change it on the repo page" }).getAttribute("href"),
          ).toBe("/repos/c1%3A%3Aorg%2Fapi");
        } else {
          expect(screen.queryByText(/Merge gate: Advisory only/)).toBeNull();
        }

        // 3. A read that failed is said so, not left as a silent absence.
        if (saysUnread) await screen.findByText(UNREAD_LINE);
        else expect(screen.queryByText(UNREAD_LINE)).toBeNull();

        // 4. The address is announced from a fresh answer only.
        expect(screen.queryByText(ADDRESS) !== null).toBe(pc.fresh);

        // 5. The panel read the list itself, in every state: a list it was
        //    handed would be a snapshot whose freshness it cannot check.
        expect(integrationsMock).toHaveBeenCalledWith("t1");

        // 6. Enable provisions only from a fresh preview, and only once the
        //    posture a gating selection needs can be decided…
        const enableBtn = () =>
          screen.getByRole("button", { name: "Enable" }) as HTMLButtonElement;
        expect(enableBtn().disabled).toBe(enableDisabled);
        if (enableDisabled) {
          fireEvent.click(enableBtn());
          await Promise.resolve();
          expect(enableMock).not.toHaveBeenCalled();
          return;
        }

        // …with no posture pinned unless one was on screen, checked…
        fireEvent.click(enableBtn());
        await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
        expect(enableMock.mock.calls[0]?.[5]).toBeUndefined();
        if (!offersPosture) return;

        // …and "Advisory only" travels exactly when it is the checked radio.
        fireEvent.click(screen.getByRole("radio", { name: /Advisory only/ }));
        expect(checked(screen.getByRole("radio", { name: /Advisory only/ }))).toBe(true);
        await waitFor(() => expect(enableBtn().disabled).toBe(false));
        fireEvent.click(enableBtn());
        await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(2));
        expect(enableMock.mock.calls[1]?.[5]).toEqual({ gate_enabled: "false" });
      });
    }
  }
});

// The panel takes no integrations from a caller at all: the prop is gone, so
// the type check refuses one — and the JSX of every caller is read here too,
// since a snapshot forwarded from a surface that cannot say whether its read
// settled is the state this whole matrix exists to refuse.
const SOURCES = import.meta.glob("/src/**/*.tsx", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

/** Every `<EnableRepoPanel …/>` element of a source file, as raw text. */
function panelElements(src: string): string[] {
  const tags: string[] = [];
  for (let from = 0; ; ) {
    const open = src.indexOf("<EnableRepoPanel", from);
    if (open === -1) return tags;
    const close = src.indexOf("/>", open);
    tags.push(src.slice(open, close === -1 ? src.length : close));
    from = close === -1 ? src.length : close + 2;
  }
}

describe("EnableRepoPanel — what callers may hand it", () => {
  it("is rendered by callers this scan can see", () => {
    // A scan that found no caller would prove nothing about any of them.
    const callers = Object.entries(SOURCES)
      // Test files are the type checker's business (`tsc -b` is part of
      // `task studio:check`); this row is about the app's own wiring.
      .filter(([path]) => !/\.test\.tsx$/.test(path) && !path.includes("/__tests__/"))
      .filter(([, src]) => src.includes("<EnableRepoPanel"));
    expect(callers.map(([path]) => path).sort()).toEqual([
      "/src/views/integrations/wizard/connect/ReposStep.tsx",
      "/src/views/teams/tabs/integrations/ConnectionCard.tsx",
    ]);
  });

  it("is handed no integrations by any caller", () => {
    const offenders: string[] = [];
    for (const [path, src] of Object.entries(SOURCES)) {
      if (/\.test\.tsx$/.test(path) || path.includes("/__tests__/")) continue;
      for (const tag of panelElements(src)) {
        if (/\bintegrations=/.test(tag)) offenders.push(path);
      }
    }
    expect(offenders).toEqual([]);
  });
});
