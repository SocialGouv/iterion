// @vitest-environment jsdom
//
// The third road of the launch form (#3416): a bot launched BY NAME reads its
// program from the server's own resolution and keeps the digest that read
// issued — what the launch posts in place of the text. It holds the two other
// roads' discipline: an answer is its own target's and nobody else's, a unit
// the launch would refuse is refused before the form, and a read that ends
// with no answer leaves no form and no digest behind.
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  openFile: vi.fn(),
  unparse: vi.fn(),
}));
vi.mock("@/api/bots", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/bots")>()),
  getBotLaunchSource: vi.fn(),
}));

import * as bots from "@/api/bots";
import type { BotLaunchSource } from "@/api/bots";
import * as client from "@/api/client";
import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";
import LaunchDocStoreProvider from "./LaunchDocStoreProvider";
import { useLaunchDoc } from "./useLaunchDoc";

// A program whose every part is named after its bot, so a render that mixes
// two bots betrays itself.
const programOf = (bot: string, rev = "1"): BotLaunchSource => ({
  bot_id: `${bot}-canonical`,
  tier: "team",
  source: `source of ${bot} r${rev}\n`,
  sha256: `sha-of-${bot}-r${rev}`,
  document: {
    ...createEmptyDocument(),
    vars: { fields: [{ name: `var_of_${bot}`, type: "string" as const }] },
  },
  bindable: true,
});

interface Seen {
  bot: string;
  source: string | null;
  id: string | null;
  sha256: string | null;
  vars: string[];
}
const seen: Seen[] = [];
const errors: string[] = [];
let reloadForm: () => void = () => {};

function Launch({ bot, pending = false }: { bot: string; pending?: boolean }) {
  const { currentSource, namedBot, doc, fields, noSource, reload } = useLaunchDoc(
    "",
    (m) => {
      errors.push(m);
    },
    { bot, pending },
  );
  useEffect(() => {
    reloadForm = reload;
  }, [reload]);
  const vars = fields.map((f) => f.name).join(",");
  const id = namedBot?.id ?? null;
  const sha256 = namedBot?.sha256 ?? null;
  useEffect(() => {
    seen.push({ bot, source: currentSource, id, sha256, vars: vars ? vars.split(",") : [] });
  }, [bot, currentSource, id, sha256, vars]);
  return (
    <>
      <output aria-label="form">{doc ? vars : "NO DOC"}</output>
      <output aria-label="digest">{sha256 ?? "NO DIGEST"}</output>
      <output aria-label="picker">{noSource ? "picker" : "no picker"}</output>
    </>
  );
}
const form = () => screen.getByLabelText("form").textContent;
const digest = () => screen.getByLabelText("digest").textContent;
const mount = (bot: string, pending = false) =>
  render(
    <LaunchDocStoreProvider>
      <Launch bot={bot} pending={pending} />
    </LaunchDocStoreProvider>,
  );

beforeEach(() => {
  seen.length = 0;
  errors.length = 0;
  vi.mocked(bots.getBotLaunchSource).mockReset();
  vi.mocked(client.openFile).mockReset();
  vi.mocked(client.unparse).mockReset();
  useTabsStore.setState({
    tabs: [],
    activeEditorTabId: null,
    activeRunTabId: null,
    currentProjectKey: null,
  });
});
afterEach(cleanup);

describe("the launch form of a bot launched by name", () => {
  it("renders the program the server resolves, with the id and digest it issued", async () => {
    vi.mocked(bots.getBotLaunchSource).mockImplementation(async (name) => programOf(name));
    mount("nestor");
    // Wait on what the hook answered, not on the page: the record of a
    // render lands one effect flush after the DOM shows it.
    await waitFor(() =>
      expect(seen).toContainEqual({
        bot: "nestor",
        source: "source of nestor r1\n",
        id: "nestor-canonical",
        sha256: "sha-of-nestor-r1",
        vars: ["var_of_nestor"],
      }),
    );
    expect(form()).toBe("var_of_nestor");
    // The program comes from the server's resolution: no file is opened, no
    // editor buffer rendered, and the page is not the empty picker.
    expect(client.openFile).not.toHaveBeenCalled();
    expect(client.unparse).not.toHaveBeenCalled();
    expect(screen.getByLabelText("picker").textContent).toBe("no picker");
  });

  it("never hands a bot the program, the digest or the form of the one before it", async () => {
    vi.mocked(bots.getBotLaunchSource).mockImplementation(async (name) => programOf(name));
    const { rerender } = mount("alpha");
    await waitFor(() => expect(digest()).toBe("sha-of-alpha-r1"));

    rerender(
      <LaunchDocStoreProvider>
        <Launch bot="beta" />
      </LaunchDocStoreProvider>,
    );
    expect(form()).toBe("NO DOC");
    expect(digest()).toBe("NO DIGEST");
    await waitFor(() =>
      expect(seen).toContainEqual({
        bot: "beta",
        source: "source of beta r1\n",
        id: "beta-canonical",
        sha256: "sha-of-beta-r1",
        vars: ["var_of_beta"],
      }),
    );
    const ofBeta = seen.filter((s) => s.bot === "beta");
    expect(ofBeta.filter((s) => s.sha256 === "sha-of-alpha-r1")).toEqual([]);
    expect(ofBeta.filter((s) => s.source === "source of alpha r1\n")).toEqual([]);
    expect(ofBeta.filter((s) => s.vars.includes("var_of_alpha"))).toEqual([]);
  });

  it("is refused for a bot whose files do not load as one, with the unit's refusal", async () => {
    const loadError = 'main.bot:1:1: error [E046]: import "lib/missing.bot": no such fragment';
    vi.mocked(bots.getBotLaunchSource).mockResolvedValue({
      ...programOf("nestor"),
      unit: {
        root: "",
        main: "main.bot",
        revision: "r1",
        files: [{ rel: "main.bot" }],
        load_errors: 1,
        load_error: loadError,
      },
    });
    mount("nestor");
    await waitFor(() => expect(errors).toHaveLength(1));
    expect(errors[0]).toContain(loadError);
    expect(form()).toBe("NO DOC");
    expect(digest()).toBe("NO DIGEST");
  });

  it("drops the form and the digest when a reload's read fails", async () => {
    vi.mocked(bots.getBotLaunchSource).mockResolvedValueOnce(programOf("nestor"));
    mount("nestor");
    await waitFor(() => expect(digest()).toBe("sha-of-nestor-r1"));

    vi.mocked(bots.getBotLaunchSource).mockRejectedValueOnce(new Error("read exploded: 503"));
    act(() => reloadForm());

    await waitFor(() => expect(errors).toEqual(["read exploded: 503"]));
    expect(form()).toBe("NO DOC");
    expect(digest()).toBe("NO DIGEST");
  });

  it("reads the program again on reload, and carries no digest across the read", async () => {
    let answer: (p: BotLaunchSource) => void = () => {};
    vi.mocked(bots.getBotLaunchSource)
      .mockResolvedValueOnce(programOf("nestor", "1"))
      .mockImplementationOnce(() => new Promise<BotLaunchSource>((resolve) => (answer = resolve)));
    mount("nestor");
    await waitFor(() => expect(digest()).toBe("sha-of-nestor-r1"));

    act(() => reloadForm());
    // The second read is in flight: the stale digest is gone, not launchable.
    await waitFor(() => expect(bots.getBotLaunchSource).toHaveBeenCalledTimes(2));
    expect(digest()).toBe("NO DIGEST");

    await act(async () => answer(programOf("nestor", "2")));
    await waitFor(() => expect(digest()).toBe("sha-of-nestor-r2"));
  });
});

describe("a launch whose target is not known yet", () => {
  it("loads nothing and offers nothing — neither a program nor the empty picker", async () => {
    // An edited buffer in the active tab: what the inline road would launch.
    const id = useTabsStore.getState().newEditorTab("buffer");
    getOrCreateDocumentStore(id).getState().setDocument(createEmptyDocument());
    vi.mocked(client.unparse).mockResolvedValue({ source: "rendered buffer\n" });

    mount("", true);
    await waitFor(() => expect(seen.length).toBeGreaterThan(0));
    expect(form()).toBe("NO DOC");
    expect(screen.getByLabelText("picker").textContent).toBe("no picker");
    expect(bots.getBotLaunchSource).not.toHaveBeenCalled();
    expect(client.unparse).not.toHaveBeenCalled();
    expect(client.openFile).not.toHaveBeenCalled();
  });
});
