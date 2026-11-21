// @vitest-environment jsdom
//
// A launch started from a bot is NAMED and PINNED in cloud (#3416): the form
// reads the bot's program from the server's own resolution
// (GET /api/v1/bots/{name}/launch-source) and posts the bot's id beside the
// digest that read issued — never the text, never a path — so the server
// launches the stored bot under its own identity, and refuses if it changed
// under the form. Every other road is what it was: a ?file= launch and the
// editor's buffer stay inline and unnamed, and local mode resolves a slug to
// its workflow file.
//
// Asserted on the real LaunchView, mounted as App.tsx mounts it, through the
// payload it hands createRun — the one thing the server reads.
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Route, Router, Switch } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ openFile: vi.fn(), unparse: vi.fn() }));
vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  openFile: api.openFile,
  unparse: api.unparse,
}));
const botsApi = vi.hoisted(() => ({
  getBotLaunchSource: vi.fn(),
  listBotsWithDiagnostics: vi.fn(),
}));
vi.mock("@/api/bots", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/bots")>()),
  getBotLaunchSource: botsApi.getBotLaunchSource,
  listBotsWithDiagnostics: botsApi.listBotsWithDiagnostics,
}));
const runsApi = vi.hoisted(() => ({
  createRun: vi.fn(),
  previewRunCost: vi.fn(),
  getServerInfo: vi.fn(),
}));
vi.mock("@/api/runs", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/runs")>()),
  createRun: runsApi.createRun,
  previewRunCost: runsApi.previewRunCost,
  getServerInfo: runsApi.getServerInfo,
}));
// The repo-target section reads the active team's repo context. Cloud with a
// team and no connected repo: the section shows, with nothing to attach.
const repoContext = vi.hoisted(() => ({ enabled: false }));
// The form's forge-connect action reads the team's connections; none here.
vi.mock("@/api/forgeConnections", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/forgeConnections")>()),
  listForgeConnections: vi.fn(async () => []),
}));
// The session's team scope, which the in-flight key carries; null is a form
// mounted with no session.
const authBox = vi.hoisted(() => ({ value: null as null | { activeTeamID: string } }));
vi.mock("@/auth/AuthContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/auth/AuthContext")>()),
  useMaybeAuth: () => authBox.value,
}));
vi.mock("@/hooks/useActiveRepo", () => ({
  useActiveRepo: () => ({
    activeRepo: null,
    overview: false,
    repos: [],
    loading: false,
    enabled: repoContext.enabled,
    choose: () => {},
    teamID: repoContext.enabled ? "team-1" : null,
  }),
}));

import type { BotEntryWithSchema, BotLaunchSource } from "@/api/bots";
import { apiErrorFrom } from "@/api/client";
import type { IterDocument, ServerInfo } from "@/api/types";
import { createEmptyDocument } from "@/lib/defaults";
import { botLaunchKey } from "@/lib/sendLaunch";
import { useBotsStore } from "@/store/bots";
import { getOrCreateDocumentStore } from "@/store/document";
import { useLaunchesInFlight } from "@/store/launchesInFlight";
import { useServerInfoStore } from "@/store/serverInfo";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";

import LaunchDocStoreProvider from "./launchView/LaunchDocStoreProvider";
import LaunchView from "./LaunchView";

// A sandboxed workflow with one required var named after its document, so the
// field on screen says WHICH program the form describes and the launch goes
// straight from the Launch button.
const docNamed = (name: string): IterDocument => ({
  ...createEmptyDocument(),
  workflows: [
    {
      name: "w",
      entry: "done",
      nodes: [],
      edges: [],
      sandbox: { mode: "auto" },
    } as unknown as IterDocument["workflows"][number],
  ],
  vars: { fields: [{ name, type: "string" as const }] },
});

const program = (name: string, sha256: string): BotLaunchSource => ({
  bot_id: "nestor",
  tier: "team",
  source: `source of ${name}\n`,
  sha256,
  document: docNamed(name),
  bindable: true,
});

const limits = {
  upload: { max_file_size: 1, max_total_size: 1, max_files_per_run: 1, allowed_mime: [] },
};
const cloudInfo: ServerInfo = { mode: "cloud", version: "test", work_dir: "", limits };
const localInfo: ServerInfo = {
  mode: "local",
  version: "test",
  work_dir: "/workspace/project",
  limits,
};

// The server info reaches the page by two roads (the launch form's own query
// and the app store): both say the same mode.
function serverIs(info: ServerInfo) {
  runsApi.getServerInfo.mockResolvedValue(info);
  useServerInfoStore.setState({ info });
}

// A team's own bot: stored, so the registry gives it no workspace path.
const teamBot: BotEntryWithSchema = {
  name: "nestor",
  path: "",
  origin: "tenant",
  editable: true,
  is_bundle: true,
  repo: { mode: "required", purpose: "Nestor watches this repository." },
};
// A workspace bundle, as a local registry lists it.
const workspaceBot: BotEntryWithSchema = {
  name: "nestor",
  path: "/workspace/project/bots/nestor",
  rel_path: "bots/nestor",
  is_bundle: true,
  origin: "catalog",
};

let qc: QueryClient;
let loc: ReturnType<typeof memoryLocation>;
function mount(path: string) {
  loc = memoryLocation({ path, record: true });
  return render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook}>
        <Switch>
          <Route path="/runs/new">
            <LaunchDocStoreProvider>
              <LaunchView />
            </LaunchDocStoreProvider>
          </Route>
        </Switch>
      </Router>
    </QueryClientProvider>,
  );
}

const launchBtn = () => screen.queryByRole("button", { name: "Launch" });
const clickLaunch = () => fireEvent.click(screen.getByRole("button", { name: "Launch" }));
const varInput = (name: string) =>
  document.getElementById(`var-${name}`) as HTMLInputElement | HTMLTextAreaElement;
const posted = (call = 0) => runsApi.createRun.mock.calls[call]?.[0] as Record<string, unknown>;

beforeEach(() => {
  vi.resetAllMocks();
  // jsdom implements neither: the desktop notice asks for matchMedia, and a
  // Launch held by a missing requirement scrolls to it on the next frame.
  Element.prototype.scrollIntoView = vi.fn();
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    configurable: true,
    value: (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }),
  });
  qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  runsApi.createRun.mockResolvedValue({ run_id: "run-1" });
  runsApi.previewRunCost.mockRejectedValue(new Error("no cost preview in this test"));
  repoContext.enabled = false;
  useTabsStore.setState({
    tabs: [],
    activeEditorTabId: null,
    activeRunTabId: null,
    currentProjectKey: null,
  });
  useBotsStore.setState({ bots: [], error: null });
  // A launch in flight and a notice outlive a form: neither outlives a test.
  useLaunchesInFlight.setState({ byTarget: {} });
  useUIStore.setState({ toasts: [] });
  authBox.value = null;
  serverIs(cloudInfo);
});

afterEach(cleanup);

describe("a cloud launch started from a bot", () => {
  it("posts the bot's id and the digest its read issued — no source, no path", async () => {
    useBotsStore.setState({ bots: [teamBot] });
    botsApi.getBotLaunchSource.mockResolvedValue(program("ticket", "sha-issued-1"));

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    expect(botsApi.getBotLaunchSource).toHaveBeenCalledWith("nestor");
    fireEvent.change(varInput("ticket"), { target: { value: "TYPED-1" } });
    clickLaunch();

    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted()).toMatchObject({
      bot_id: "nestor",
      expect_source_sha256: "sha-issued-1",
      vars: { ticket: "TYPED-1" },
    });
    // The text and a path are the two other ways to name a program: a named
    // launch carries neither, or the server reads it as unnamed work.
    expect(posted()).not.toHaveProperty("source");
    expect(posted()).not.toHaveProperty("file_path");
    // The program was read from the server's resolution, never from a file.
    expect(api.openFile).not.toHaveBeenCalled();
  });

  it("posts the canonical id the read answered, whatever the URL spells", async () => {
    botsApi.getBotLaunchSource.mockResolvedValue({
      ...program("ticket", "sha-issued-1"),
      bot_id: "nestor-canonical",
    });

    mount("/runs/new?bot=Nestor_Canonical");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(varInput("ticket"), { target: { value: "v" } });
    clickLaunch();

    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted()).toMatchObject({ bot_id: "nestor-canonical" });
  });

  it("shows the repository a stored bot requires", async () => {
    repoContext.enabled = true;
    useBotsStore.setState({ bots: [teamBot] });
    botsApi.getBotLaunchSource.mockResolvedValue(program("ticket", "sha-issued-1"));

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());

    // The entry is matched by the bot the launch names: a stored bot has no
    // workspace path to be matched by.
    await waitFor(() =>
      expect(screen.getByRole("radiogroup", { name: "Target repository" })).toBeTruthy(),
    );
    expect(screen.getByText("Nestor watches this repository.")).toBeTruthy();
    // And the requirement holds the launch, as it does for a workspace bot.
    fireEvent.change(varInput("ticket"), { target: { value: "v" } });
    clickLaunch();
    await waitFor(() => expect(screen.getByRole("alert").textContent).toBe("Target repository required"));
    expect(runsApi.createRun).not.toHaveBeenCalled();
  });

  // The server's refusal is a whole sentence — who may read this bot's
  // program, and that it can still be launched by id. It is what the form
  // says, as written: not the transport's wrapping of it, and not a dead end.
  it("says a refused read in the server's own sentence and offers no launch", async () => {
    const sentence =
      'bots: "nestor" is your team\'s own bot: its program is readable by a bot editor, a team admin or an owner of the team — you may still launch "nestor" by id (bot_id) from the CLI, the API or MCP, which shows no program';
    botsApi.getBotLaunchSource.mockRejectedValue(apiErrorFrom(403, { message: sentence }));

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(screen.getByRole("alert").textContent).toBe(sentence));
    expect(launchBtn()).toBeNull();
    expect(runsApi.createRun).not.toHaveBeenCalled();
  });

  it("says so when the server issued no digest, instead of waiting for one", async () => {
    botsApi.getBotLaunchSource.mockResolvedValue({ ...program("ticket", ""), sha256: "" });

    mount("/runs/new?bot=nestor");
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/without the id and digest a launch from this form is pinned to/),
    );
    expect(launchBtn()).toBeNull();
    expect(varInput("ticket")).toBeNull();
  });

  // A salvage is the program MINUS what the reader could not read: the form
  // would show part of a program the launch runs whole. Refused as the
  // editor's buffer is, whichever of the two made it one.
  it("refuses a program the server could only salvage", async () => {
    botsApi.getBotLaunchSource.mockResolvedValue({ ...program("ticket", "sha-1"), bindable: false });

    mount("/runs/new?bot=nestor");
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/This bot's program does not parse/),
    );
    expect(launchBtn()).toBeNull();
    expect(varInput("ticket")).toBeNull();
  });

  it("refuses a program the server's reader did not read whole", async () => {
    botsApi.getBotLaunchSource.mockResolvedValue({
      ...program("ticket", "sha-1"),
      read_refusal: { message: "main.bot: past the read budget", hint: "Raise ITERION_READ_BUDGET." },
    });

    mount("/runs/new?bot=nestor");
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toBe(
        "The server did not read this bot whole (main.bot: past the read budget), so this form would show only part of the program the launch runs. Raise ITERION_READ_BUDGET. Then reload this form.",
      ),
    );
    expect(launchBtn()).toBeNull();
  });

  // rva round 1, S1. A preset's values are applied BY THE SERVER, from the
  // program: the form omits every preset-covered value it shows unchanged.
  // So the preset it shows must be the pinned program's — the registry
  // listing is as old as the session, and one that still says OLD would show
  // OLD and run NEW with nothing to catch it.
  it("shows and launches the presets its program was read with, not the registry's", async () => {
    const presetsOf = (value: string) => ({
      entries: [
        { name: "prod", values: [{ key: "ticket", value: { kind: "string" as const, str_val: value, raw: value } }] },
      ],
    });
    // The catalog as listed when the session began: the preset says OLD, and
    // it knows a preset the bot no longer has.
    useBotsStore.setState({
      bots: [
        {
          ...teamBot,
          presets: {
            entries: [...presetsOf("OLD").entries, { name: "gone", values: [] }],
          },
        } as BotEntryWithSchema,
      ],
    });
    // The bot as it is stored now, which the form reads and pins.
    botsApi.getBotLaunchSource.mockResolvedValue({
      ...program("ticket", "sha-new"),
      presets: presetsOf("NEW"),
    });

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    const picker = screen.getByRole("combobox") as HTMLSelectElement;
    expect([...picker.options].map((o) => o.value)).toEqual(["", "prod"]);
    fireEvent.change(picker, { target: { value: "prod" } });
    await waitFor(() => expect(varInput("ticket").value).toBe("NEW"));
    clickLaunch();

    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    // The value on screen is the one the pinned program's preset applies, so
    // omitting it is exact; the stale one is nowhere.
    expect(posted()).toMatchObject({
      bot_id: "nestor",
      expect_source_sha256: "sha-new",
      preset: "prod",
    });
    expect(posted().vars).toBeUndefined();
    expect(JSON.stringify(posted())).not.toContain("OLD");
  });

  // The other things the form takes from the registry entry decide what is
  // OFFERED — here, where the connect wizard sends the operator back to.
  it("comes back to the bot's own form from the forge-connect wizard", async () => {
    repoContext.enabled = true;
    useBotsStore.setState({ bots: [teamBot] });
    botsApi.getBotLaunchSource.mockResolvedValue(program("ticket", "sha-issued-1"));

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(screen.getByRole("button", { name: "Connect a repository" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Connect a repository" }));

    expect(loc.history).toContain(
      `/integrations/connect?returnTo=${encodeURIComponent("/runs/new?bot=nestor")}`,
    );
  });
});

describe("a bot that changed under the form", () => {
  it("shows the server's refusal, and reloads the program on the operator's action", async () => {
    botsApi.getBotLaunchSource
      .mockResolvedValueOnce(program("ticket", "sha-stale"))
      .mockResolvedValueOnce(program("ticket_v2", "sha-fresh"));
    runsApi.createRun.mockRejectedValueOnce(
      apiErrorFrom(409, {
        message: 'bot "nestor" changed since the launch form read it — reload the form',
        errorCode: "bot_source_changed",
      }),
    );

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(varInput("ticket"), { target: { value: "TYPED-1" } });
    clickLaunch();

    // The server's own sentence, and the remedy as an action.
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain(
        'bot "nestor" changed since the launch form read it — reload the form',
      ),
    );
    // The sentence as the server wrote it, not the transport's wrapping of it.
    expect(screen.getByRole("alert").textContent).not.toContain("API error");
    expect(posted()).toMatchObject({ expect_source_sha256: "sha-stale" });
    // Nothing reloads by itself: the fields the operator filled are still theirs.
    expect(botsApi.getBotLaunchSource).toHaveBeenCalledTimes(1);
    expect(varInput("ticket").value).toBe("TYPED-1");

    fireEvent.click(screen.getByRole("button", { name: "Reload the bot" }));

    // The form is the bot as it is now, and the refusal is gone with it.
    await waitFor(() => expect(varInput("ticket_v2")).toBeTruthy());
    expect(botsApi.getBotLaunchSource).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("button", { name: "Reload the bot" })).toBeNull();
    expect(screen.queryByText(/changed since the launch form read it/)).toBeNull();

    fireEvent.change(varInput("ticket_v2"), { target: { value: "TYPED-2" } });
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(2));
    expect(posted(1)).toMatchObject({
      bot_id: "nestor",
      expect_source_sha256: "sha-fresh",
      vars: { ticket_v2: "TYPED-2" },
    });
  });

  // The refusal is the launch's that drew it: the next launch starts from a
  // clean slate, so a second refusal of another kind is not offered the
  // first one's remedy.
  it("withdraws the reload when the next launch is refused for another reason", async () => {
    botsApi.getBotLaunchSource.mockResolvedValue(program("ticket", "sha-1"));
    runsApi.createRun
      .mockRejectedValueOnce(
        apiErrorFrom(409, { message: 'bot "nestor" changed', errorCode: "bot_source_changed" }),
      )
      .mockRejectedValueOnce(
        apiErrorFrom(402, { message: "monthly run quota exceeded", errorCode: "monthly_run_quota_exceeded" }),
      );

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(varInput("ticket"), { target: { value: "v" } });
    clickLaunch();
    await waitFor(() => expect(screen.getByRole("button", { name: "Reload the bot" })).toBeTruthy());

    clickLaunch();
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("monthly run quota exceeded"));
    expect(screen.queryByRole("button", { name: "Reload the bot" })).toBeNull();
  });

  // A reload is a new read: the fields are the new program's, at their
  // defaults — what the operator typed was typed over the other one.
  it("re-initialises the fields on reload, even when the program kept their names", async () => {
    botsApi.getBotLaunchSource
      .mockResolvedValueOnce(program("ticket", "sha-stale"))
      .mockResolvedValueOnce(program("ticket", "sha-fresh"));
    runsApi.createRun.mockRejectedValueOnce(
      apiErrorFrom(409, { message: 'bot "nestor" changed', errorCode: "bot_source_changed" }),
    );

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(varInput("ticket"), { target: { value: "TYPED-1" } });
    clickLaunch();
    await waitFor(() => expect(screen.getByRole("button", { name: "Reload the bot" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Reload the bot" }));

    await waitFor(() => expect(botsApi.getBotLaunchSource).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(varInput("ticket").value).toBe(""));
  });

  // The banner and its remedy are about ONE bot (rva round 1, S5): when the
  // URL moves to another, they go with the form they were raised on.
  it("takes its refusal and its reload away when the URL moves to another bot", async () => {
    botsApi.getBotLaunchSource.mockImplementation(async (name: string) => ({
      ...program(`var_${name}`, `sha-${name}`),
      bot_id: name,
    }));
    runsApi.createRun.mockRejectedValueOnce(
      apiErrorFrom(409, { message: 'bot "alpha" changed since the launch form read it', errorCode: "bot_source_changed" }),
    );

    mount("/runs/new?bot=alpha");
    await waitFor(() => expect(varInput("var_alpha")).toBeTruthy());
    fireEvent.change(varInput("var_alpha"), { target: { value: "x" } });
    clickLaunch();
    await waitFor(() => expect(screen.getByRole("button", { name: "Reload the bot" })).toBeTruthy());

    act(() => loc.navigate("/runs/new?bot=beta"));
    await waitFor(() => expect(varInput("var_beta")).toBeTruthy());
    expect(screen.queryByRole("button", { name: "Reload the bot" })).toBeNull();
    expect(screen.queryByText(/changed since the launch form read it/)).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("offers no reload for any other refusal", async () => {
    botsApi.getBotLaunchSource.mockResolvedValue(program("ticket", "sha-1"));
    runsApi.createRun.mockRejectedValueOnce(
      apiErrorFrom(402, { message: "monthly run quota exceeded", errorCode: "monthly_run_quota_exceeded" }),
    );

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(varInput("ticket"), { target: { value: "v" } });
    clickLaunch();

    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("monthly run quota exceeded"));
    expect(screen.queryByRole("button", { name: "Reload the bot" })).toBeNull();
  });
});

// A program of two vars: one required, which the operator types, and
// `shadow`, which has a declared default and stays out of the primary fields
// — the var a preset sets without the operator seeing it.
const docShadowed = (name: string, presets?: IterDocument["presets"]): IterDocument => ({
  ...docNamed(name),
  vars: {
    fields: [
      { name, type: "string" as const },
      {
        name: "shadow",
        type: "string" as const,
        default: { kind: "string" as const, str_val: "DECLARED", raw: '"DECLARED"' },
      },
    ],
  },
  ...(presets ? { presets } : {}),
});
// A preset `prod` that sets the var nobody sees.
const prodPreset = (value: string) => ({
  entries: [
    {
      name: "prod",
      values: [{ key: "shadow", value: { kind: "string" as const, str_val: value, raw: value } }],
    },
  ],
});
const picker = () => screen.getByRole("combobox") as HTMLSelectElement;
const namedProgram = (bot: string, sha: string, field: string, presets?: BotLaunchSource["presets"]) =>
  ({
    ...program(field, sha),
    bot_id: bot,
    document: docShadowed(field),
    ...(presets ? { presets } : {}),
  }) as BotLaunchSource;
const type = (name: string, value: string) =>
  fireEvent.change(varInput(name), { target: { value } });

// A preset selection belongs to the document it was made on (rva round 2,
// S1). A reload or a move lands another document and resets the fields to its
// declared defaults: a selection that outlived it posted `preset` beside an
// explicit default for every key the preset covers — a var the form does not
// even show among them — so the run got the preset's prompt without its
// values, or a preset the new program does not have.
describe("a preset selection does not outlive the document it was made on", () => {
  it("is cleared by the reload that follows a 409", async () => {
    botsApi.getBotLaunchSource
      .mockResolvedValueOnce(namedProgram("nestor", "sha-1", "ticket", prodPreset("PROD-V1")))
      .mockResolvedValueOnce(namedProgram("nestor", "sha-2", "ticket", prodPreset("PROD-V2")));
    runsApi.createRun
      .mockRejectedValueOnce(
        apiErrorFrom(409, { message: 'bot "nestor" changed since the launch form read it', errorCode: "bot_source_changed" }),
      )
      .mockResolvedValueOnce({ run_id: "run-2" });

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(picker(), { target: { value: "prod" } });
    type("ticket", "T-1");
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    // The premise: with the preset selected its value is the baseline, so
    // the shadow var is not posted.
    expect(posted(0)).toMatchObject({ preset: "prod", vars: { ticket: "T-1" } });

    await waitFor(() => expect(screen.getByRole("button", { name: "Reload the bot" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Reload the bot" }));
    await waitFor(() => expect(botsApi.getBotLaunchSource).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(varInput("ticket").value).toBe(""));

    // The form shows no preset, and posts none — nor a default in its place.
    expect(picker().value).toBe("");
    type("ticket", "T-2");
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(2));
    expect(posted(1)).toMatchObject({ bot_id: "nestor", expect_source_sha256: "sha-2" });
    expect(posted(1).preset).toBeUndefined();
    expect(posted(1).vars).toEqual({ ticket: "T-2" });
  });

  for (const other of [
    { name: "that has a preset of the same name", presets: prodPreset("BETA-PROD") },
    { name: "that has no such preset", presets: undefined },
  ]) {
    it(`is cleared by a move to another bot ${other.name}`, async () => {
      botsApi.getBotLaunchSource.mockImplementation(async (name: string) =>
        name === "alpha"
          ? namedProgram("alpha", "sha-alpha", "var_alpha", prodPreset("ALPHA-PROD"))
          : namedProgram("beta", "sha-beta", "var_beta", other.presets),
      );

      mount("/runs/new?bot=alpha");
      await waitFor(() => expect(varInput("var_alpha")).toBeTruthy());
      fireEvent.change(picker(), { target: { value: "prod" } });
      expect(picker().value).toBe("prod");

      act(() => loc.navigate("/runs/new?bot=beta"));
      await waitFor(() => expect(varInput("var_beta")).toBeTruthy());
      if (other.presets) expect(picker().value).toBe("");
      type("var_beta", "B-1");
      clickLaunch();
      await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
      expect(posted()).toMatchObject({ bot_id: "beta", expect_source_sha256: "sha-beta" });
      expect(posted().preset).toBeUndefined();
      expect(posted().vars).toEqual({ var_beta: "B-1" });
    });
  }

  // The same document object can land twice (rva round 3): the editor's
  // buffer of tab A, then tab B, then tab A again before B's render answered.
  // The second landing resets the values; the selection goes with them.
  it("is cleared when the very same buffer lands again — tab A, tab B, tab A", async () => {
    const a = useTabsStore.getState().newEditorTab("a");
    getOrCreateDocumentStore(a).getState().setDocument(docShadowed("var_a", prodPreset("A-PROD")));
    const b = useTabsStore.getState().newEditorTab("b");
    getOrCreateDocumentStore(b).getState().setDocument(docShadowed("var_b"));
    useTabsStore.setState({ activeEditorTabId: a });
    // B's render never answers; A's does.
    api.unparse.mockImplementation((doc: IterDocument) =>
      doc.vars?.fields?.[0]?.name === "var_b"
        ? new Promise(() => {})
        : Promise.resolve({ source: "rendered a\n" }),
    );

    mount("/runs/new");
    await waitFor(() => expect(varInput("var_a")).toBeTruthy());
    fireEvent.change(picker(), { target: { value: "prod" } });
    type("var_a", "A-1");

    act(() => useTabsStore.setState({ activeEditorTabId: b }));
    act(() => useTabsStore.setState({ activeEditorTabId: a }));
    await waitFor(() => expect(varInput("var_a").value).toBe(""));
    expect(picker().value).toBe("");
    type("var_a", "A-2");
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted().preset).toBeUndefined();
    expect(posted().vars).toEqual({ var_a: "A-2" });
  });

  // What is NOT a landing leaves both alone: the catalog answering again.
  it("is kept, with the values, when the bot catalog is fetched again", async () => {
    useBotsStore.setState({ bots: [teamBot] });
    botsApi.getBotLaunchSource.mockResolvedValue(
      namedProgram("nestor", "sha-1", "ticket", prodPreset("PROD-V1")),
    );
    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(picker(), { target: { value: "prod" } });
    type("ticket", "T-1");

    act(() => useBotsStore.setState({ bots: [{ ...teamBot }] }));
    expect(picker().value).toBe("prod");
    expect(varInput("ticket").value).toBe("T-1");
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted()).toMatchObject({ preset: "prod", vars: { ticket: "T-1" } });
  });

  // The class is every road's: a file's in-source preset, across a ?file= move.
  it("is cleared by a move from one file to another", async () => {
    api.openFile.mockImplementation(async (path: string) => ({
      source: `workflow of ${path}\n`,
      document:
        path === "bots/a.bot"
          ? docShadowed("var_a", prodPreset("A-PROD"))
          : docShadowed("var_b", prodPreset("B-PROD")),
      diagnostics: [],
      path,
    }));

    mount("/runs/new?file=bots/a.bot");
    await waitFor(() => expect(varInput("var_a")).toBeTruthy());
    fireEvent.change(picker(), { target: { value: "prod" } });
    expect(picker().value).toBe("prod");

    act(() => loc.navigate("/runs/new?file=bots/b.bot"));
    await waitFor(() => expect(varInput("var_b")).toBeTruthy());
    expect(picker().value).toBe("");
    type("var_b", "B-1");
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted()).toMatchObject({ file_path: "bots/b.bot" });
    expect(posted().preset).toBeUndefined();
    expect(posted().vars).toEqual({ var_b: "B-1" });
  });
});

// An answer belongs to the target its launch was sent for (rva rounds 2 and
// 3). The form can leave the screen while a launch is in flight — the URL
// moves to another bot, or away — and the answer is then neither applied to
// what is on screen (no navigation, no banner, no reload offered to another
// bot) nor lost: the run exists and is billed, the refusal was needed. It is
// announced, and the target stays held until it lands.
describe("a launch still in flight when its form leaves the screen", () => {
  const twoBots = () =>
    botsApi.getBotLaunchSource.mockImplementation(async (name: string) =>
      namedProgram(name, `sha-${name}`, `var_${name}`),
    );
  const toasts = () => useUIStore.getState().toasts;
  const launchIsHeld = () => (launchBtn() as HTMLButtonElement).disabled;
  // Launches alpha with an answer the test gives later, then moves away.
  async function launchAlphaThenGoTo(path: string) {
    twoBots();
    let accept: (r: { run_id: string }) => void = () => {};
    let refuse: (e: unknown) => void = () => {};
    runsApi.createRun.mockImplementationOnce(
      () =>
        new Promise((resolve, reject) => {
          accept = resolve;
          refuse = reject;
        }),
    );
    mount("/runs/new?bot=alpha");
    await waitFor(() => expect(varInput("var_alpha")).toBeTruthy());
    type("var_alpha", "A-1");
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    act(() => loc.navigate(path));
    return {
      accept: (run_id: string) => act(async () => accept({ run_id })),
      refuse: (e: unknown) => act(async () => refuse(e)),
    };
  }

  it("names the run it started, with the way to it, and leaves the operator where they went", async () => {
    const alpha = await launchAlphaThenGoTo("/runs/new?bot=beta");
    await waitFor(() => expect(varInput("var_beta")).toBeTruthy());
    await alpha.accept("run-alpha");

    await waitFor(() => expect(toasts()).toHaveLength(1));
    expect(toasts()[0]).toMatchObject({ type: "success", persistent: true });
    expect(toasts()[0]?.message).toContain("run-alpha");
    expect(toasts()[0]?.message).toContain("the bot alpha");
    // Not taken off beta's form — and one gesture away from the run.
    expect(loc.history).not.toContain("/runs/run-alpha");
    expect(varInput("var_beta")).toBeTruthy();
    expect(toasts()[0]?.action?.label).toBe("Open the run");
    act(() => toasts()[0]?.action?.onClick());
    expect(loc.history).toContain("/runs/run-alpha");
  });

  it("offers the other bot no reload for its refusal, and says the refusal in a notice", async () => {
    const alpha = await launchAlphaThenGoTo("/runs/new?bot=beta");
    await waitFor(() => expect(varInput("var_beta")).toBeTruthy());
    await alpha.refuse(
      apiErrorFrom(409, { message: 'bot "alpha" changed since the launch form read it', errorCode: "bot_source_changed" }),
    );

    await waitFor(() => expect(toasts()).toHaveLength(1));
    expect(toasts()[0]).toMatchObject({ type: "error", persistent: true });
    expect(toasts()[0]?.message).toContain("the bot alpha");
    expect(toasts()[0]?.message).toContain('bot "alpha" changed since the launch form read it');
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByRole("button", { name: "Reload the bot" })).toBeNull();
  });

  it("says a refusal the operator needed — a quota — though its form is gone", async () => {
    const alpha = await launchAlphaThenGoTo("/runs/new?bot=beta");
    await waitFor(() => expect(varInput("var_beta")).toBeTruthy());
    await alpha.refuse(
      apiErrorFrom(402, { message: "monthly run quota exceeded", errorCode: "monthly_run_quota_exceeded" }),
    );

    await waitFor(() => expect(toasts()).toHaveLength(1));
    expect(toasts()[0]?.type).toBe("error");
    expect(toasts()[0]?.message).toContain("the bot alpha");
    expect(toasts()[0]?.message).toContain("monthly run quota exceeded");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  // No second run while the first is pending: the hold is the target's, so
  // it is still there when the operator comes back — and it is not the other
  // bot's.
  it("keeps the bot held on return, the other bot free, and names the run when it answers", async () => {
    const alpha = await launchAlphaThenGoTo("/runs/new?bot=beta");
    await waitFor(() => expect(varInput("var_beta")).toBeTruthy());
    type("var_beta", "B-1");
    expect(launchIsHeld()).toBe(false);

    act(() => loc.navigate("/runs/new?bot=alpha"));
    await waitFor(() => expect(varInput("var_alpha")).toBeTruthy());
    expect(launchIsHeld()).toBe(true);
    fireEvent.click(launchBtn() as HTMLButtonElement);
    expect(runsApi.createRun).toHaveBeenCalledTimes(1);

    // The form that asked is gone — the one back is another, with nothing of
    // the first: the answer is named, the operator is not moved, and the bot
    // is theirs to launch again.
    await alpha.accept("run-alpha");
    await waitFor(() => expect(toasts()).toHaveLength(1));
    expect(toasts()[0]?.message).toBe("Run run-alpha started for the bot alpha.");
    expect(loc.history).not.toContain("/runs/run-alpha");
    expect(launchIsHeld()).toBe(false);
    expect(useLaunchesInFlight.getState().byTarget).toEqual({});
  });

  it("does not pull the operator back when the form left the screen altogether", async () => {
    const alpha = await launchAlphaThenGoTo("/elsewhere");
    await alpha.accept("run-alpha");

    await waitFor(() => expect(toasts()).toHaveLength(1));
    expect(toasts()[0]?.message).toContain("run-alpha");
    expect(loc.history).not.toContain("/runs/run-alpha");
  });
});

// A load resets the form to its declared defaults, so a load the operator
// did not ask for takes away what they typed — the options folded out of
// sight included (rva round 4, F2). Only the unsaved-buffer road reads the
// editor: on a named bot and on a ?file= launch, nothing that happens in an
// editor tab is a reason to read again, and none remounts the form.
describe("an editor event is not a reason to read a launch again", () => {
  const settle = () => act(async () => {});
  // A named bot's form with a preset chosen and a value typed, beside two
  // editor tabs that have nothing to do with it.
  async function typedFormBesideTwoTabs() {
    const a = useTabsStore.getState().newEditorTab("a");
    getOrCreateDocumentStore(a).getState().setDocument(docNamed("of_a"));
    const b = useTabsStore.getState().newEditorTab("b");
    getOrCreateDocumentStore(b).getState().setDocument(docNamed("of_b"));
    useTabsStore.setState({ activeEditorTabId: a });
    botsApi.getBotLaunchSource.mockResolvedValue(
      namedProgram("nestor", "sha-1", "ticket", prodPreset("PROD")),
    );
    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(picker(), { target: { value: "prod" } });
    type("ticket", "T-1");
    return { a, b, input: varInput("ticket") };
  }
  async function untouched(input: HTMLElement) {
    await settle();
    expect(botsApi.getBotLaunchSource).toHaveBeenCalledTimes(1);
    expect(varInput("ticket")).toBe(input);
    expect(varInput("ticket").value).toBe("T-1");
    expect(picker().value).toBe("prod");
  }

  it("keeps the form through a switch of editor tab", async () => {
    const { b, input } = await typedFormBesideTwoTabs();
    act(() => useTabsStore.setState({ activeEditorTabId: b }));
    await untouched(input);
  });

  it("keeps the form through an edit of the active tab's document", async () => {
    const { a, input } = await typedFormBesideTwoTabs();
    act(() => getOrCreateDocumentStore(a).getState().setDocument(docNamed("edited")));
    await untouched(input);
  });

  // The buffer's own refusal is the inline road's business too: a tab that
  // stops being launchable says nothing of a bot read from the server.
  it("keeps the form when the active tab's buffer becomes unlaunchable", async () => {
    const { a, input } = await typedFormBesideTwoTabs();
    act(() => getOrCreateDocumentStore(a).getState().setSalvaged(true));
    await untouched(input);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps the form through the active tab closing, and the last one after it", async () => {
    const { a, b, input } = await typedFormBesideTwoTabs();
    act(() => useTabsStore.getState().closeTab(a));
    await untouched(input);
    act(() => useTabsStore.getState().closeTab(b));
    expect(useTabsStore.getState().activeEditorTabId).toBeNull();
    await untouched(input);

    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted()).toMatchObject({ bot_id: "nestor", preset: "prod", vars: { ticket: "T-1" } });
  });

  // The value nobody sees: set under the folded "Bot options", it must still
  // be what the launch posts after an edit elsewhere.
  it("posts an override set under the folded options, whatever the editor did meanwhile", async () => {
    const a = useTabsStore.getState().newEditorTab("a");
    getOrCreateDocumentStore(a).getState().setDocument(docNamed("of_a"));
    botsApi.getBotLaunchSource.mockResolvedValue(namedProgram("nestor", "sha-1", "ticket"));
    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Bot options/ }));
    await waitFor(() => expect(varInput("shadow")).toBeTruthy());
    type("shadow", "MINE");
    fireEvent.click(screen.getByRole("button", { name: /Bot options/ }));

    act(() => getOrCreateDocumentStore(a).getState().setDocument(docNamed("edited")));
    await settle();
    type("ticket", "T-1");
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted().vars).toEqual({ ticket: "T-1", shadow: "MINE" });
  });

  it("keeps a ?file= form through a switch of editor tab too", async () => {
    const a = useTabsStore.getState().newEditorTab("a");
    getOrCreateDocumentStore(a).getState().setDocument(docNamed("of_a"));
    const b = useTabsStore.getState().newEditorTab("b");
    getOrCreateDocumentStore(b).getState().setDocument(docNamed("of_b"));
    useTabsStore.setState({ activeEditorTabId: a });
    api.openFile.mockResolvedValue({
      source: "workflow x:\n  entry: done\n",
      document: docNamed("ticket"),
      diagnostics: [],
      path: "bots/x.bot",
    });
    mount("/runs/new?file=bots/x.bot");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    type("ticket", "T-1");
    const input = varInput("ticket");

    act(() => useTabsStore.setState({ activeEditorTabId: b }));
    await settle();
    expect(api.openFile).toHaveBeenCalledTimes(1);
    expect(varInput("ticket")).toBe(input);
    expect(varInput("ticket").value).toBe("T-1");
  });
});

// A launch in flight holds what the SERVER resolved, in the team it was
// resolved for (rva round 4, F3): the URL's spelling of a bot is not the bot,
// and the same slug in another team is another bot.
describe("the target a launch in flight holds", () => {
  it("is the bot the read named, whatever the URL spells", async () => {
    // The server folds both spellings onto one bot.
    botsApi.getBotLaunchSource.mockImplementation(async () =>
      namedProgram("feed-watch", "sha-1", "ticket"),
    );
    let accept: (r: { run_id: string }) => void = () => {};
    runsApi.createRun.mockImplementationOnce(() => new Promise((resolve) => (accept = resolve)));

    mount("/runs/new?bot=Feed_Watch");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    type("ticket", "T-1");
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(useLaunchesInFlight.getState().byTarget).toEqual({ [botLaunchKey("", "feed-watch")]: 1 });

    act(() => loc.navigate("/runs/new?bot=feed-watch"));
    await waitFor(() => expect(botsApi.getBotLaunchSource).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    type("ticket", "T-2");
    expect((launchBtn() as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(launchBtn() as HTMLButtonElement);
    expect(runsApi.createRun).toHaveBeenCalledTimes(1);

    // Its answer is named — never lost, never a second run.
    await act(async () => accept({ run_id: "run-1" }));
    await waitFor(() => expect(useUIStore.getState().toasts).toHaveLength(1));
    expect(useUIStore.getState().toasts[0]?.message).toBe("Run run-1 started for the bot feed-watch.");
    expect(useLaunchesInFlight.getState().byTarget).toEqual({});
  });

  it("is that bot in the team it was launched for, and no other team's", async () => {
    authBox.value = { activeTeamID: "team-1" };
    botsApi.getBotLaunchSource.mockResolvedValue(namedProgram("nestor", "sha-1", "ticket"));
    let accept: (r: { run_id: string }) => void = () => {};
    runsApi.createRun.mockImplementationOnce(() => new Promise((resolve) => (accept = resolve)));

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    type("ticket", "T-1");
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(useLaunchesInFlight.getState().byTarget).toEqual({ [botLaunchKey("team-1", "nestor")]: 1 });
    expect((launchBtn() as HTMLButtonElement).disabled).toBe(true);

    // The session moves to another team that has a bot of that name.
    authBox.value = { activeTeamID: "team-2" };
    act(() => loc.navigate("/runs/new?bot=nestor&team=2"));
    await waitFor(() => expect((launchBtn() as HTMLButtonElement).disabled).toBe(false));

    // Team 1's answer is not this form's: it is named, and nobody is moved.
    await act(async () => accept({ run_id: "run-1" }));
    await waitFor(() => expect(useUIStore.getState().toasts).toHaveLength(1));
    expect(useUIStore.getState().toasts[0]?.message).toBe("Run run-1 started for the bot nestor.");
    expect(loc.history).not.toContain("/runs/run-1");
  });

  it("is held by a launch of the same bot from its Test pane", async () => {
    // What the pane holds while its launch is pending (TestRunPane).
    useLaunchesInFlight.getState().begin(botLaunchKey("", "nestor"));
    botsApi.getBotLaunchSource.mockResolvedValue(namedProgram("nestor", "sha-1", "ticket"));
    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    type("ticket", "T-1");
    expect((launchBtn() as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(launchBtn() as HTMLButtonElement);
    expect(runsApi.createRun).not.toHaveBeenCalled();
  });
});

// One form per target (rva round 5, F1): everything the form holds is about
// one launch, and a move to another bot starts from nothing — the options
// folded out of sight included.
describe("a form does not carry one bot's choices to another", () => {
  const engineOption = (knob: string) => {
    const name = Array.from(document.querySelectorAll("div.font-mono")).find(
      (d) => d.textContent?.trim() === knob,
    );
    return name?.closest("div.grid")?.querySelector("select") as HTMLSelectElement;
  };

  it("posts nothing of the engine options set on the bot before it", async () => {
    botsApi.getBotLaunchSource.mockImplementation(async (name: string) =>
      namedProgram(name, `sha-${name}`, "ticket"),
    );
    mount("/runs/new?bot=alpha");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Engine options/ }));
    await waitFor(() => expect(engineOption("permission")).toBeTruthy());
    fireEvent.change(engineOption("permission"), { target: { value: "deny" } });
    expect(engineOption("permission").value).toBe("deny");

    act(() => loc.navigate("/runs/new?bot=beta"));
    await waitFor(() => expect(botsApi.getBotLaunchSource).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    type("ticket", "B-1");
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted()).toMatchObject({ bot_id: "beta", vars: { ticket: "B-1" } });
    expect(posted().permission).toBeUndefined();
  });
});

// What an outcome means for the run is said the same on screen as in a
// notice (rva round 5, F2): a launch that drew no answer is not a plain
// failure. The button is free again — and the operator has been told what a
// second click risks, with the way to look first.
describe("a launch that drew no answer, with its form still there", () => {
  for (const [name, error] of [
    ["a gateway timeout", apiErrorFrom(504, { message: "Gateway Timeout" })],
    ["a network failure", new TypeError("Failed to fetch")],
  ] as const) {
    it(`says the run may have started after ${name}, and where to look`, async () => {
      botsApi.getBotLaunchSource.mockResolvedValue(namedProgram("nestor", "sha-1", "ticket"));
      runsApi.createRun.mockRejectedValueOnce(error);
      mount("/runs/new?bot=nestor");
      await waitFor(() => expect(varInput("ticket")).toBeTruthy());
      type("ticket", "T-1");
      clickLaunch();

      await waitFor(() =>
        expect(screen.getByRole("alert").textContent).toMatch(
          /No answer came for the launch of the bot nestor \(.+\): the run may have started — check its runs before launching again\./,
        ),
      );
      expect((launchBtn() as HTMLButtonElement).disabled).toBe(false);
      fireEvent.click(screen.getByRole("button", { name: "Open its runs" }));
      expect(loc.history).toContain("/runs?bot=nestor");
    });
  }

  it("offers no such look for a refusal, which says no run exists", async () => {
    botsApi.getBotLaunchSource.mockResolvedValue(namedProgram("nestor", "sha-1", "ticket"));
    runsApi.createRun.mockRejectedValueOnce(
      apiErrorFrom(402, { message: "monthly run quota exceeded", errorCode: "monthly_run_quota_exceeded" }),
    );
    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    type("ticket", "T-1");
    clickLaunch();
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("monthly run quota exceeded"));
    expect(screen.getByRole("alert").textContent).not.toMatch(/may have started/);
    expect(screen.queryByRole("button", { name: "Open its runs" })).toBeNull();
  });
});

// Locally a bot is its workflow file — the resolved one (rva round 5, F3).
// The same file reached by three spellings is one target: a launch of it in
// flight holds them all.
describe("a local file reached by several spellings", () => {
  it("is one target, held under each of them", async () => {
    serverIs(localInfo);
    api.openFile.mockImplementation(async (path: string) => ({
      source: "workflow x:\n  entry: done\n",
      document: docNamed("ticket"),
      diagnostics: [],
      path,
    }));
    runsApi.createRun.mockImplementationOnce(() => new Promise(() => {}));

    mount("/runs/new?file=bots/nestor/main.bot");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    type("ticket", "T-1");
    clickLaunch();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));

    for (const spelling of ["./bots/nestor/main.bot", "/workspace/project/bots/nestor/main.bot"]) {
      act(() => loc.navigate(`/runs/new?file=${encodeURIComponent(spelling)}`));
      await waitFor(() => expect(api.openFile).toHaveBeenCalledWith(spelling));
      await waitFor(() => expect(varInput("ticket")).toBeTruthy());
      expect((launchBtn() as HTMLButtonElement).disabled).toBe(true);
    }
    expect(Object.keys(useLaunchesInFlight.getState().byTarget)).toHaveLength(1);
    expect(runsApi.createRun).toHaveBeenCalledTimes(1);
  });
});

// The reload is the remedy of ONE refusal — "the bot changed". A move takes
// it away even when what follows is another refusal (rva round 2, S3f): a
// read the server refuses is not something a reload of the bot mends.
describe("a refused read that follows a 409 on another bot", () => {
  it("carries no reload of the bot", async () => {
    botsApi.getBotLaunchSource.mockImplementation(async (name: string) => {
      if (name === "alpha") return namedProgram("alpha", "sha-alpha", "var_alpha");
      throw apiErrorFrom(403, { message: 'bots: "beta" is your team\'s own bot: its program is readable by a bot editor, a team admin or an owner of the team' });
    });
    runsApi.createRun.mockRejectedValueOnce(
      apiErrorFrom(409, { message: 'bot "alpha" changed since the launch form read it', errorCode: "bot_source_changed" }),
    );

    mount("/runs/new?bot=alpha");
    await waitFor(() => expect(varInput("var_alpha")).toBeTruthy());
    type("var_alpha", "A-1");
    clickLaunch();
    await waitFor(() => expect(screen.getByRole("button", { name: "Reload the bot" })).toBeTruthy());

    act(() => loc.navigate("/runs/new?bot=beta"));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("your team's own bot"));
    expect(screen.queryByRole("button", { name: "Reload the bot" })).toBeNull();
  });
});

describe("the roads that name no bot are what they were, in cloud", () => {
  it("a ?file= launch posts its path and its text inline", async () => {
    api.openFile.mockResolvedValue({
      source: "workflow x:\n  entry: done\n",
      document: docNamed("ticket"),
      diagnostics: [],
      path: "bots/x.bot",
    });

    mount("/runs/new?file=bots/x.bot");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(varInput("ticket"), { target: { value: "v" } });
    clickLaunch();

    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted()).toMatchObject({
      file_path: "bots/x.bot",
      source: "workflow x:\n  entry: done\n",
    });
    expect(posted()).not.toHaveProperty("bot_id");
    expect(posted()).not.toHaveProperty("expect_source_sha256");
    expect(botsApi.getBotLaunchSource).not.toHaveBeenCalled();
  });

  it("?file= beats ?bot= when both are present", async () => {
    api.openFile.mockResolvedValue({
      source: "workflow x:\n  entry: done\n",
      document: docNamed("ticket"),
      diagnostics: [],
      path: "bots/x.bot",
    });

    mount("/runs/new?file=bots/x.bot&bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(varInput("ticket"), { target: { value: "v" } });
    clickLaunch();

    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted()).toMatchObject({ file_path: "bots/x.bot" });
    expect(posted()).not.toHaveProperty("bot_id");
    expect(botsApi.getBotLaunchSource).not.toHaveBeenCalled();
  });

  it("the editor's unsaved buffer posts its render inline", async () => {
    const id = useTabsStore.getState().newEditorTab("buffer");
    getOrCreateDocumentStore(id).getState().setDocument(docNamed("ticket"));
    api.unparse.mockResolvedValue({ source: "rendered buffer\n" });

    mount("/runs/new");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    fireEvent.change(varInput("ticket"), { target: { value: "v" } });
    clickLaunch();

    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted()).toMatchObject({ file_path: "", source: "rendered buffer\n" });
    expect(posted()).not.toHaveProperty("bot_id");
    expect(posted()).not.toHaveProperty("expect_source_sha256");
  });
});

describe("a local launch started from a bot", () => {
  it("resolves the slug to its workflow file and posts that path", async () => {
    serverIs(localInfo);
    useBotsStore.setState({ bots: [workspaceBot] });
    api.openFile.mockResolvedValue({
      source: "workflow nestor:\n  entry: done\n",
      document: docNamed("ticket"),
      diagnostics: [],
      path: "bots/nestor/main.bot",
    });

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(varInput("ticket")).toBeTruthy());
    expect(api.openFile).toHaveBeenCalledWith("bots/nestor/main.bot");
    fireEvent.change(varInput("ticket"), { target: { value: "v" } });
    clickLaunch();

    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(posted()).toMatchObject({
      file_path: "bots/nestor/main.bot",
      source: "workflow nestor:\n  entry: done\n",
    });
    expect(posted()).not.toHaveProperty("bot_id");
    expect(posted()).not.toHaveProperty("expect_source_sha256");
    expect(botsApi.getBotLaunchSource).not.toHaveBeenCalled();
  });
});

// Under a bot's URL nothing but that bot may be launched (rva round 1, S2).
// While the facts that decide are on their way the form waits; when the slug
// names nothing this form may launch it refuses by name. Either way it arms
// no submit and never renders the editor's buffer in the bot's place — the
// edited buffer below is the inline road's launch candidate, sitting in the
// active tab for the form to fall through to.
describe("a ?bot= slug that names nothing this form may launch", () => {
  beforeEach(() => {
    const id = useTabsStore.getState().newEditorTab("buffer");
    getOrCreateDocumentStore(id).getState().setDocument(docNamed("buffer_var"));
    api.unparse.mockResolvedValue({ source: "rendered buffer\n" });
  });

  const nothingWasLoadedOrOffered = () => {
    expect(varInput("buffer_var")).toBeNull();
    expect(launchBtn()).toBeNull();
    expect(api.unparse).not.toHaveBeenCalled();
    expect(api.openFile).not.toHaveBeenCalled();
    expect(botsApi.getBotLaunchSource).not.toHaveBeenCalled();
    expect(runsApi.createRun).not.toHaveBeenCalled();
  };

  it("waits while the mode is unknown", async () => {
    runsApi.getServerInfo.mockReturnValue(new Promise<ServerInfo>(() => {}));

    mount("/runs/new?bot=nestor");
    await waitFor(() => expect(screen.getByText("Loading workflow…")).toBeTruthy());
    nothingWasLoadedOrOffered();
  });

  // Settled with no answer is UNKNOWN, never "local": a cloud server read as
  // local would resolve the slug to a baked file, or to this buffer.
  it("refuses when the server info did not load", async () => {
    runsApi.getServerInfo.mockRejectedValue(new Error("server info: 503"));
    useBotsStore.setState({ bots: [workspaceBot] });

    mount("/runs/new?bot=nestor");
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(
        /Cannot open the launch form of bot “nestor”: the server did not say whether it runs in cloud or local mode/,
      ),
    );
    nothingWasLoadedOrOffered();
  });

  it("refuses, locally, when the catalog did not load", async () => {
    serverIs(localInfo);
    useBotsStore.setState({ bots: null, error: null });
    botsApi.listBotsWithDiagnostics.mockRejectedValue(new Error("catalog 500"));

    mount("/runs/new?bot=nestor");
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/the bot catalog did not load \(catalog 500\)/),
    );
    nothingWasLoadedOrOffered();
  });

  it("refuses, locally, a slug the catalog does not list", async () => {
    serverIs(localInfo);
    useBotsStore.setState({ bots: [workspaceBot] });

    mount("/runs/new?bot=ghost");
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/No bot named “ghost” in this workspace's catalog/),
    );
    nothingWasLoadedOrOffered();
  });

  it("refuses, locally, a bot with no workflow file under the workspace", async () => {
    serverIs(localInfo);
    useBotsStore.setState({ bots: [teamBot] });

    mount("/runs/new?bot=nestor");
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/Bot “nestor” has no workflow file under this workspace/),
    );
    nothingWasLoadedOrOffered();
  });

  // Last in the file on purpose: the bots store keeps its in-flight load in
  // module state, and this one never answers.
  it("waits, locally, while the catalog is loading", async () => {
    serverIs(localInfo);
    useBotsStore.setState({ bots: null, error: null });
    botsApi.listBotsWithDiagnostics.mockReturnValue(new Promise(() => {}));

    mount("/runs/new?bot=nestor");
    // The mode has answered — local — and the catalog has been asked for and
    // has not: this wait is the catalog's, not the mode's.
    await waitFor(() => expect(qc.getQueryState(["server-info"])?.status).toBe("success"));
    await waitFor(() => expect(botsApi.listBotsWithDiagnostics).toHaveBeenCalled());
    await act(async () => {});
    expect(screen.getByText("Loading workflow…")).toBeTruthy();
    nothingWasLoadedOrOffered();
  });
});
