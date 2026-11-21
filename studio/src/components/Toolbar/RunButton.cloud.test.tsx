// @vitest-environment jsdom
//
// The toolbar's Run in cloud (#3416). The host credential probe says nothing
// there — a run is funded by the team's own keys, resolved by the server at
// launch — so it must not hold the button: a team that set its key on the
// team page could not Run, and was sent to Preferences → Backends, which
// configures the host. And a file that is a registry bot's entry leads to
// that bot's NAMED launch form, so the run carries the bot's identity; any
// other file, and a buffer bound to none, launch as they do locally.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

const setLocation = vi.fn();
vi.mock("wouter", () => ({ useLocation: () => ["/editor", setLocation] }));
const botsApi = vi.hoisted(() => ({ listBotsWithDiagnostics: vi.fn() }));
vi.mock("@/api/bots", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/bots")>()),
  listBotsWithDiagnostics: botsApi.listBotsWithDiagnostics,
}));

import type { BackendDetectReport } from "@/api/backends";
import type { BotEntryWithSchema } from "@/api/bots";
import { botSourceEditorPath } from "@/api/client";
import type { ServerInfo } from "@/api/types";
import { createEmptyDocument } from "@/lib/defaults";
import { useBackendDetectStore } from "@/store/backendDetect";
import { useBotsStore } from "@/store/bots";
import { DocumentStoreProvider, createDocumentStore, type DocumentStore } from "@/store/document";
import { useServerInfoStore } from "@/store/serverInfo";

vi.mock("@/auth/AuthContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/auth/AuthContext")>()),
  useMaybeAuth: () => ({ activeTeamID: "team-1" }),
}));

import RunButton from "./RunButton";

const limits = {
  upload: { max_file_size: 1, max_total_size: 1, max_files_per_run: 1, allowed_mime: [] },
};
const cloudInfo: ServerInfo = { mode: "cloud", version: "test", work_dir: "", limits };
const localInfo: ServerInfo = { mode: "local", version: "test", work_dir: "/w", limits };

const teamBot: BotEntryWithSchema = { name: "nestor", path: "", origin: "tenant", is_bundle: true };
const bakedBot: BotEntryWithSchema = {
  name: "feed-watch",
  path: "/app/bots/feed-watch",
  rel_path: "bots/feed-watch",
  is_bundle: true,
  origin: "catalog",
};

function mount(store: DocumentStore) {
  return render(
    <DocumentStoreProvider store={store}>
      <RunButton />
    </DocumentStoreProvider>,
  );
}

function bound(path: string | null): DocumentStore {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath(path);
  if (path !== null) store.getState().markSaved();
  return store;
}

const run = () => screen.getByRole("button", { name: "Run" }) as HTMLButtonElement;
// A control held for a reason stays focusable: it says it is unavailable
// (aria-disabled) and its description is the reason (HeldControl).
const held = (el: HTMLElement | null) =>
  !!el && ((el as HTMLButtonElement).disabled || el.getAttribute("aria-disabled") === "true");
const heldReason = (el: HTMLElement | null) =>
  el?.getAttribute("aria-disabled") === "true"
    ? (document.getElementById(el.getAttribute("aria-describedby") ?? "")?.textContent ?? "")
    : "";
const nudge = () => screen.queryByRole("button", { name: /No LLM credential detected/ });

// The host probe answered and found no credential: the state that greys the
// button out locally.
function noHostCredential() {
  useBackendDetectStore.setState({
    report: { resolved_default: "" } as unknown as BackendDetectReport,
  });
}

beforeEach(() => {
  setLocation.mockClear();
  botsApi.listBotsWithDiagnostics.mockReset();
  noHostCredential();
  useBotsStore.setState({ bots: [teamBot, bakedBot], loading: false, error: null });
  useServerInfoStore.setState({ info: cloudInfo });
});

afterEach(cleanup);

// A held control that takes the focus opens its tooltip, and Radix positioning
// (floating-ui) needs ResizeObserver, which jsdom lacks.
beforeAll(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});

describe("the toolbar's Run in cloud", () => {
  it("is not held by the host credential probe, and raises no nudge about it", () => {
    mount(bound("scratch/x.bot"));
    expect(run().disabled).toBe(false);
    expect(nudge()).toBeNull();
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new?file=scratch%2Fx.bot");
  });

  it("leads a team's bot, opened from its source, to its named launch form", () => {
    mount(bound(botSourceEditorPath("team-1", "nestor")));
    expect(run().disabled).toBe(false);
    expect(run().title).toBe("Launch the bot nestor");
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new?bot=nestor");
  });

  it("leads a catalog bot's entry file to its named launch form", () => {
    mount(bound("bots/feed-watch/main.bot"));
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new?bot=feed-watch");
  });

  it("launches a buffer bound to no file inline", () => {
    mount(bound(null));
    expect(run().disabled).toBe(false);
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new");
  });

  // A tab bound to a bot keeps the rule a bound file always had: the launch
  // form reads what is STORED, so edits not yet saved are not what runs —
  // the named form is the stored bot, as the ?file= form was the stored file.
  it("leads a bot's tab with unsaved edits to the stored bot's named form", () => {
    const store = bound(botSourceEditorPath("team-1", "nestor"));
    store.getState().setDocument({ ...createEmptyDocument(), comments: [] });
    mount(store);
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new?bot=nestor");
  });
});

// Which bot a file is must not default to "none" (rva round 1, S4): launched
// by path, a bot's entry runs as unnamed work — the very thing #1368 rules
// out. A team's bot opened from its source names itself in its path, so it
// needs no catalog; any other bound file waits for the catalog to say.
describe("the toolbar's Run in cloud, before or without the catalog", () => {
  it("names a team's bot from its editor path while the catalog is not there", () => {
    useBotsStore.setState({ bots: null, loading: false, error: null });
    mount(bound(botSourceEditorPath("team-1", "nestor")));
    expect(run().disabled).toBe(false);
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new?bot=nestor");
    // The path said it: no catalog was asked for.
    expect(botsApi.listBotsWithDiagnostics).not.toHaveBeenCalled();
  });

  it("names a team's bot from its editor path when the catalog failed to load", () => {
    useBotsStore.setState({ bots: null, loading: false, error: "catalog 500" });
    mount(bound(botSourceEditorPath("team-1", "nestor")));
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new?bot=nestor");
  });

  it("names a team's bot the catalog does not list yet", () => {
    useBotsStore.setState({ bots: [bakedBot], loading: false, error: null });
    mount(bound(botSourceEditorPath("team-1", "just-created")));
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new?bot=just-created");
  });

  it("holds any other bound file until the catalog says whether it is a bot's entry", async () => {
    botsApi.listBotsWithDiagnostics.mockResolvedValue({ bots: [teamBot, bakedBot], discoveryErrors: [] });
    useBotsStore.setState({ bots: null, loading: false, error: null });
    mount(bound("bots/feed-watch/main.bot"));

    // Held, and saying why where a keyboard reaches it — never launched by
    // path meanwhile.
    expect(run().disabled).toBe(false);
    expect(heldReason(run())).toMatch(/Loading the bot catalog/);
    fireEvent.click(run());
    expect(setLocation).not.toHaveBeenCalled();
    // A keyboard that waits on the button keeps it when the hold lifts.
    const waitingOn = run();
    waitingOn.focus();

    // The button asked for the catalog itself; once it answers, the file is
    // the bot it is.
    await waitFor(() => expect(held(run())).toBe(false));
    expect(run()).toBe(waitingOn);
    expect(document.activeElement).toBe(waitingOn);
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new?bot=feed-watch");
  });

  it("holds any other bound file when the catalog failed to load, and says so", async () => {
    botsApi.listBotsWithDiagnostics.mockRejectedValue(new Error("catalog 500"));
    useBotsStore.setState({ bots: null, loading: false, error: null });
    mount(bound("bots/feed-watch/main.bot"));
    await waitFor(() =>
      expect(heldReason(run())).toMatch(/The bot catalog did not load \(catalog 500\)/),
    );
    fireEvent.click(run());
    expect(setLocation).not.toHaveBeenCalled();
  });

  // A tab left open across a team switch is another team's bot: launched
  // from here it would run as inline, unnamed work under the active team
  // (rva round 2, S3c).
  it("holds another team's bot tab, and says to switch to that team", () => {
    mount(bound(botSourceEditorPath("team-2", "nestor")));
    expect(heldReason(run())).toMatch(
      /the bot nestor of another team than the active one: switch to that team to launch it by name/,
    );
    fireEvent.click(run());
    expect(setLocation).not.toHaveBeenCalled();
    // Nothing is asked of the catalog either: the path alone says it.
    expect(botsApi.listBotsWithDiagnostics).not.toHaveBeenCalled();
  });

  // Only the bot's ENTRY is the bot: a fragment of it open in a tab is a
  // file, and its Run is not the whole bot's named launch (rva round 2, S3f).
  it("does not name a fragment tab of the team's bot as the whole bot", () => {
    const fragment = botSourceEditorPath("team-1", "nestor", "lib/nodes.bot");
    mount(bound(fragment));
    expect(held(run())).toBe(false);
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith(`/runs/new?file=${encodeURIComponent(fragment)}`);
  });

  it("holds no buffer bound to no file: there is nothing for the catalog to say", () => {
    useBotsStore.setState({ bots: null, loading: false, error: null });
    mount(bound(null));
    expect(run().disabled).toBe(false);
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new");
  });
});

describe("the toolbar's Run locally", () => {
  beforeEach(() => useServerInfoStore.setState({ info: localInfo }));

  it("is still held by the host credential probe, with its nudge", () => {
    mount(bound("bots/feed-watch/main.bot"));
    expect(run().disabled).toBe(true);
    expect(run().title).toMatch(/No LLM credentials detected/);
    expect(nudge()).not.toBeNull();
    fireEvent.click(run());
    expect(setLocation).not.toHaveBeenCalled();
  });

  it("launches a registry bot's entry by its path, as before", () => {
    useBackendDetectStore.setState({
      report: { resolved_default: "anthropic" } as unknown as BackendDetectReport,
    });
    mount(bound("bots/feed-watch/main.bot"));
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new?file=bots%2Ffeed-watch%2Fmain.bot");
  });
});
