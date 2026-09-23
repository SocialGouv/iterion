// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ApiError } from "@/api/client";

// The drawer is the one surface that writes a bundle's files directly. What
// is under test is which TOKEN its writes carry and what happens when the
// store refuses one (#1650) — not Monaco, which is stubbed down to a
// textarea so the buffer is drivable.
const botSources = vi.hoisted(() => ({
  getBotSource: vi.fn(),
  putBotSourceFile: vi.fn(),
  deleteBotSourceFile: vi.fn(),
}));
vi.mock("@/api/botSources", () => botSources);

// `@monaco-editor/react` captures `onMount` at the editor's FIRST render and
// calls it exactly once. The stub reproduces that contract — it is what makes
// a keybinding bound there close over stale state — and exposes the command
// it registered so a test can fire it later. No hook: "once, at the first
// render" is the behaviour under test, and a flag says it without pretending
// the double has a lifecycle.
const keybinding = vi.hoisted(() => ({ run: null as null | (() => void), mounted: false }));
vi.mock("@/lib/monaco", () => ({
  default: function MonacoStub({
    value,
    onChange,
    language,
    onMount,
  }: {
    value?: string;
    onChange?: (v?: string) => void;
    language?: string;
    onMount?: (ed: unknown, monaco: unknown) => void;
  }) {
    if (!keybinding.mounted) {
      keybinding.mounted = true;
      onMount?.(
        {
          addCommand: (_k: number, run: () => void) => {
            keybinding.run = run;
          },
        },
        { KeyMod: { CtrlCmd: 1 }, KeyCode: { KeyS: 2 } },
      );
    }
    return (
      <textarea
        aria-label="file"
        data-language={language}
        value={value ?? ""}
        onChange={(e) => onChange?.(e.target.value)}
      />
    );
  },
}));

import BundleFilesDrawer from "./BundleFilesDrawer";
import { useUIStore } from "@/store/ui";

afterEach(() => {
  cleanup();
  useUIStore.setState({ toasts: [] });
  vi.clearAllMocks();
  keybinding.run = null;
  keybinding.mounted = false;
});

const BUNDLE = {
  id: "b1",
  slug: "demo",
  version: 7,
  files: {
    "main.bot": "workflow w:\n  entry: a\n",
    "lib/nodes.bot": "dsl: 2\n\ntool a:\n  command: \"true\"\n",
    "skills/notes.md": "# notes\n",
  },
};

function open() {
  botSources.getBotSource.mockResolvedValue(BUNDLE);
  render(
    <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
  );
}

async function openForEdit(rel: string) {
  open();
  const row = await screen.findByRole("button", { name: rel });
  fireEvent.click(row);
  return screen.getByLabelText("file") as HTMLTextAreaElement;
}

describe("BundleFilesDrawer writes", () => {
  it("saves with the version it READ, not with none", async () => {
    botSources.putBotSourceFile.mockResolvedValue({ ...BUNDLE, version: 8 });
    const buffer = await openForEdit("skills/notes.md");
    fireEvent.change(buffer, { target: { value: "# edited\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(botSources.putBotSourceFile).toHaveBeenCalledTimes(1));
    expect(botSources.putBotSourceFile).toHaveBeenCalledWith(
      "team-1",
      "demo",
      "skills/notes.md",
      "# edited\n",
      7,
    );
  });

  it("deletes with the version it read", async () => {
    botSources.deleteBotSourceFile.mockResolvedValue({ ...BUNDLE, version: 8 });
    open();
    await screen.findByRole("button", { name: "skills/notes.md" });
    fireEvent.click(screen.getByTitle("Delete skills/notes.md"));
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    await waitFor(() => expect(botSources.deleteBotSourceFile).toHaveBeenCalledTimes(1));
    expect(botSources.deleteBotSourceFile).toHaveBeenCalledWith(
      "team-1",
      "demo",
      "skills/notes.md",
      7,
    );
  });

  it("keeps presenting the version it read across two saves of the same session", async () => {
    // The second save must not quietly adopt a token read at write time:
    // that is the shape that turns a refusal back into an overwrite.
    botSources.putBotSourceFile.mockRejectedValue(
      new ApiError(500, "API error 500: boom", undefined, "boom"),
    );
    const buffer = await openForEdit("skills/notes.md");
    fireEvent.change(buffer, { target: { value: "one" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(botSources.putBotSourceFile).toHaveBeenCalledTimes(1));

    fireEvent.change(buffer, { target: { value: "two" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(botSources.putBotSourceFile).toHaveBeenCalledTimes(2));
    expect(botSources.putBotSourceFile.mock.calls[1]![4]).toBe(7);
  });
});

describe("BundleFilesDrawer on a refused write", () => {
  it("says the bot moved and keeps the typed text", async () => {
    botSources.putBotSourceFile.mockRejectedValue(
      new ApiError(
        409,
        "API error 409: botsource: version conflict — the bot was modified by another write",
        undefined,
        "botsource: version conflict — the bot was modified by another write",
      ),
    );
    const buffer = await openForEdit("skills/notes.md");
    fireEvent.change(buffer, { target: { value: "# mine\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await screen.findByText(/This bot changed in the store/);
    // The author's only copy of what they typed is on screen — a refusal
    // that cleared it would lose exactly what it set out to protect.
    expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe("# mine\n");
  });

  it("shows no such banner when the write is refused for another reason", async () => {
    botSources.putBotSourceFile.mockRejectedValue(
      new ApiError(400, "API error 400: bot does not compile", undefined, "bot does not compile"),
    );
    const buffer = await openForEdit("skills/notes.md");
    fireEvent.change(buffer, { target: { value: "# mine\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(botSources.putBotSourceFile).toHaveBeenCalledTimes(1));
    expect(screen.queryByText(/This bot changed in the store/)).toBeNull();
  });

  it("clears the refusal only through a reload that re-reads the store", async () => {
    botSources.putBotSourceFile.mockRejectedValue(
      new ApiError(409, "API error 409: version conflict", undefined, "version conflict"),
    );
    const buffer = await openForEdit("skills/notes.md");
    fireEvent.change(buffer, { target: { value: "# mine\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText(/This bot changed in the store/);

    botSources.getBotSource.mockResolvedValue({
      ...BUNDLE,
      version: 9,
      files: { ...BUNDLE.files, "skills/notes.md": "# theirs\n" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    // Reloading over a dirty buffer asks first, because it takes the text.
    fireEvent.click(await screen.findByRole("button", { name: "Reload and discard" }));

    await waitFor(() =>
      expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe("# theirs\n"),
    );
    expect(screen.queryByText(/This bot changed in the store/)).toBeNull();

    // …and the next write presents the token the reload read.
    botSources.putBotSourceFile.mockResolvedValue({ ...BUNDLE, version: 10 });
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "# merged\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(botSources.putBotSourceFile).toHaveBeenCalledTimes(2));
    expect(botSources.putBotSourceFile.mock.calls[1]![4]).toBe(9);
  });
});

describe("BundleFilesDrawer editor language", () => {
  it("gives a .bot fragment iterion's own DSL, not plain text", async () => {
    const buffer = await openForEdit("lib/nodes.bot");
    expect(buffer.getAttribute("data-language")).toBe("iter");
  });

  it("leaves a non-workflow file to the inferred language", async () => {
    const buffer = await openForEdit("skills/notes.md");
    expect(buffer.getAttribute("data-language")).toBe("markdown");
  });
});

describe("BundleFilesDrawer and the bundle's main", () => {
  it("edits the main as text, with the file as it is STORED", async () => {
    // The canvas cannot repair a main that does not parse: it opens the
    // document SALVAGED — the file minus the region the parser could not
    // read — and every write site refuses it. On the cloud twin the bundle's
    // files live in the store, so this drawer is the only surface that
    // reaches them (#1659).
    botSources.getBotSource.mockResolvedValue({
      ...BUNDLE,
      files: { ...BUNDLE.files, "main.bot": "workflow w:\n  entry: a\n  a -> !!! broken\n" },
    });
    render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    await screen.findByTitle("Open the workflow in the Canvas editor");
    fireEvent.click(screen.getByRole("button", { name: "Edit as text" }));

    const buffer = screen.getByLabelText("file") as HTMLTextAreaElement;
    expect(buffer.value).toBe("workflow w:\n  entry: a\n  a -> !!! broken\n");
    expect(buffer.getAttribute("data-language")).toBe("iter");
  });

  it("saves a repaired main through the per-file write", async () => {
    botSources.putBotSourceFile.mockResolvedValue({ ...BUNDLE, version: 8 });
    open();
    await screen.findByTitle("Open the workflow in the Canvas editor");
    fireEvent.click(screen.getByRole("button", { name: "Edit as text" }));
    fireEvent.change(screen.getByLabelText("file"), {
      target: { value: "workflow w:\n  entry: a\n  a -> done\n" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(botSources.putBotSourceFile).toHaveBeenCalledTimes(1));
    expect(botSources.putBotSourceFile).toHaveBeenCalledWith(
      "team-1",
      "demo",
      "main.bot",
      "workflow w:\n  entry: a\n  a -> done\n",
      7,
    );
  });

  it("keeps the canvas as the main's row, so the text lane is an ADDITION", async () => {
    open();
    const row = await screen.findByTitle("Open the workflow in the Canvas editor");
    expect(row.textContent).toContain("main.bot");
    fireEvent.click(row);
    // The row still jumps to the canvas: no inline buffer opens from it.
    expect(screen.queryByLabelText("file")).toBeNull();
  });

  it("offers no delete for the main, which is the bundle entry", async () => {
    open();
    await screen.findByTitle("Open the workflow in the Canvas editor");
    expect(screen.queryByTitle("Delete main.bot")).toBeNull();
  });
});

describe("BundleFilesDrawer when the bundle could not be read", () => {
  it("offers no way to write, since nothing here could be checked", async () => {
    botSources.getBotSource.mockRejectedValue(new ApiError(503, "API error 503: down"));
    render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    // A failed read used to leave a fully functional EMPTY file list: every
    // write went out with no if-match token at all, and "New file" typed
    // with an existing path opened an empty buffer over a real file.
    await screen.findByText(/could not be read/i);
    expect(screen.queryByRole("button", { name: "New file" })).toBeNull();
    expect(botSources.putBotSourceFile).not.toHaveBeenCalled();
  });
});

describe("BundleFilesDrawer when its props move to another bot", () => {
  it("drops the buffer and the refusal rather than writing them into the new bot, once asked", async () => {
    botSources.putBotSourceFile.mockRejectedValue(
      new ApiError(409, "API error 409: version conflict", undefined, "version conflict"),
    );
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    const view = render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "skills/notes.md" }));
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "text meant for demo\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText(/This bot changed in the store/);

    // The toolbar derives these props from the active editor file, so they
    // can move while the panel is open. Adopting the new bundle's version
    // under the old bot's text writes that text into the new bot under a
    // token the store has no reason to refuse.
    botSources.getBotSource.mockResolvedValue({ ...BUNDLE, slug: "other", version: 42 });
    view.rerender(
      <BundleFilesDrawer teamID="team-1" slug="other" open onOpenChange={() => {}} />,
    );
    // The reset is no longer silent (#1749): the typed text is the author's
    // only copy, so following the editor to another bot asks first. What the
    // reset must still guarantee is that neither the buffer nor the refusal
    // TRAVELS — bot demo's text under bot other's token is the write the
    // store has no reason to refuse.
    fireEvent.click(await screen.findByRole("button", { name: "Discard" }));

    await screen.findByRole("button", { name: "skills/notes.md" });
    expect(screen.queryByLabelText("file")).toBeNull();
    expect(screen.queryByText(/This bot changed in the store/)).toBeNull();
  });
});

describe("BundleFilesDrawer's Ctrl+S", () => {
  it("saves what is typed NOW, under the version held NOW", async () => {
    botSources.putBotSourceFile.mockResolvedValue({ ...BUNDLE, version: 8 });
    const buffer = await openForEdit("skills/notes.md");
    fireEvent.change(buffer, { target: { value: "the author's work\n" } });

    // The keybinding was registered once, at the editor's first render. Bound
    // to the handler of THAT render it wrote the file's pre-edit text and then
    // cleared the editor from the same stale closure — erasing the typed text
    // from the only place it existed.
    expect(keybinding.run).not.toBeNull();
    keybinding.run!();

    await waitFor(() => expect(botSources.putBotSourceFile).toHaveBeenCalledTimes(1));
    expect(botSources.putBotSourceFile).toHaveBeenCalledWith(
      "team-1",
      "demo",
      "skills/notes.md",
      "the author's work\n",
      7,
    );
  });

  it("presents the token a Reload read, not the one it started with", async () => {
    botSources.putBotSourceFile.mockRejectedValue(
      new ApiError(409, "API error 409: version conflict", undefined, "version conflict"),
    );
    const buffer = await openForEdit("skills/notes.md");
    fireEvent.change(buffer, { target: { value: "# mine\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText(/This bot changed in the store/);

    botSources.getBotSource.mockResolvedValue({ ...BUNDLE, version: 9 });
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    fireEvent.click(await screen.findByRole("button", { name: "Reload and discard" }));
    await waitFor(() =>
      expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe("# notes\n"),
    );

    botSources.putBotSourceFile.mockResolvedValue({ ...BUNDLE, version: 10 });
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "# merged\n" } });
    keybinding.run!();
    await waitFor(() => expect(botSources.putBotSourceFile).toHaveBeenCalledTimes(2));
    expect(botSources.putBotSourceFile.mock.calls[1]![4]).toBe(9);
  });
});

// The drawer's exits drop the typed buffer, which is the author's only copy
// of it: the drawer is a modal, so Escape, a click outside, the Close button
// and Back all reach the same loss. They go through one gate now.
describe("BundleFilesDrawer leaving a file with typed text", () => {
  it("asks before Back takes it, and keeps it when the author declines", async () => {
    const buffer = await openForEdit("skills/notes.md");
    fireEvent.change(buffer, { target: { value: "# mine\n" } });

    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));

    // Still in the file, still holding the text.
    await waitFor(() =>
      expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe("# mine\n"),
    );
  });

  it("takes it once the author confirms", async () => {
    const buffer = await openForEdit("skills/notes.md");
    fireEvent.change(buffer, { target: { value: "# mine\n" } });

    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard" }));
    await waitFor(() => expect(screen.queryByLabelText("file")).toBeNull());
  });

  it("asks before the drawer closes, and does not close while the answer is pending", async () => {
    const onOpenChange = vi.fn();
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={onOpenChange} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "skills/notes.md" }));
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "# mine\n" } });

    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    // `onOpenChange` is a REQUEST — the parent owns `open`. Asking the parent
    // to close before the author answered would unmount the very buffer the
    // dialog is asking about.
    await screen.findByRole("button", { name: "Discard" });
    expect(onOpenChange).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() =>
      expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe("# mine\n"),
    );
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("asks nothing when the buffer holds what was loaded", async () => {
    await openForEdit("skills/notes.md");
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    await waitFor(() => expect(screen.queryByLabelText("file")).toBeNull());
    expect(screen.queryByRole("button", { name: "Discard" })).toBeNull();
  });
});

// #1749 — the ninth member of the same class. The drawer's props follow the
// active editor file, so they can move to another bot while it is open; the
// load effect used to reset the buffer silently. It asks now, and declining
// keeps the drawer on the bundle the text belongs to — which the title names
// — so the author can save it.
describe("BundleFilesDrawer when the editor moves to another bot", () => {
  it("asks before following, and stays put when the author declines", async () => {
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    const view = render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "skills/notes.md" }));
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "text meant for demo\n" } });

    botSources.getBotSource.mockResolvedValue({ ...BUNDLE, slug: "other", version: 42 });
    view.rerender(
      <BundleFilesDrawer teamID="team-1" slug="other" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));

    // The text survives, and a save from here still names the bundle it
    // belongs to — not the bot the editor moved to.
    await waitFor(() =>
      expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe(
        "text meant for demo\n",
      ),
    );
    botSources.putBotSourceFile.mockResolvedValue({ ...BUNDLE, version: 8 });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(botSources.putBotSourceFile).toHaveBeenCalledTimes(1));
    expect(botSources.putBotSourceFile.mock.calls[0]![1]).toBe("demo");
  });

  it("follows once the author confirms", async () => {
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    const view = render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "skills/notes.md" }));
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "text meant for demo\n" } });

    botSources.getBotSource.mockResolvedValue({ ...BUNDLE, slug: "other", version: 42 });
    view.rerender(
      <BundleFilesDrawer teamID="team-1" slug="other" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Discard" }));

    await waitFor(() => expect(screen.queryByLabelText("file")).toBeNull());
    await screen.findByRole("button", { name: "skills/notes.md" });
  });

  it("asks nothing when the buffer holds nothing", async () => {
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    const view = render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    await screen.findByRole("button", { name: "skills/notes.md" });

    botSources.getBotSource.mockResolvedValue({ ...BUNDLE, slug: "other", version: 42 });
    view.rerender(
      <BundleFilesDrawer teamID="team-1" slug="other" open onOpenChange={() => {}} />,
    );
    await screen.findByRole("button", { name: "skills/notes.md" });
    expect(screen.queryByRole("button", { name: "Discard" })).toBeNull();
  });
});

// Round 6's own findings: the gate was reachable only while the parent kept
// the drawer mounted, and the bundle in state was not keyed to the bundle on
// screen.
describe("BundleFilesDrawer's bundle identity", () => {
  it("shows nothing from a bundle it is no longer on", async () => {
    // A save in flight when the editor moves answers AFTER the rebind. An
    // unstamped bundle then put one bot's files under the other's title and
    // carried the first's if-match token into a write aimed at the second —
    // which two bundles at the same version would not even refuse.
    //
    // The two answers are deferred explicitly: left to race, the new
    // bundle's load happens to land last and overwrite the stale write, and
    // the test passes whatever the code does.
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    let settleSave!: (v: unknown) => void;
    botSources.putBotSourceFile.mockReturnValue(new Promise((r) => { settleSave = r; }));
    const view = render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "skills/notes.md" }));
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "# demo\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    const other = {
      ...BUNDLE,
      slug: "other",
      version: 42,
      files: { "main.bot": "x", "docs/readme.md": "y" },
    };
    let settleLoad!: (v: unknown) => void;
    botSources.getBotSource.mockReturnValue(new Promise((r) => { settleLoad = r; }));
    view.rerender(
      <BundleFilesDrawer teamID="team-1" slug="other" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Discard" }));

    // `other` lands first, so the drawer is fully on it — no spinner left to
    // hide what comes next.
    settleLoad(other);
    await screen.findByRole("button", { name: "docs/readme.md" });

    // NOW the stale save answers, naming `demo`. An unstamped bundle takes
    // it: demo's files appear under other's title, and the next write
    // carries demo's version 8 against a bundle at 42.
    settleSave({ ...BUNDLE, version: 8 });
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByRole("button", { name: "skills/notes.md" })).toBeNull();
    expect(screen.getByRole("button", { name: "docs/readme.md" })).toBeTruthy();
    expect(screen.queryByText(/could not be read/i)).toBeNull();

    // The token the next write presents is the one the shown bundle carries.
    botSources.deleteBotSourceFile.mockResolvedValue(other);
    fireEvent.click(screen.getByTitle("Delete docs/readme.md"));
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    await waitFor(() => expect(botSources.deleteBotSourceFile).toHaveBeenCalledTimes(1));
    expect(botSources.deleteBotSourceFile.mock.calls[0]![3]).toBe(42);
  });
});

describe("BundleFilesDrawer's follow latch", () => {
  it("follows again once the buffer is saved, rather than pinning for ever", async () => {
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    const view = render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "skills/notes.md" }));
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "# mine\n" } });

    const other = { ...BUNDLE, slug: "other", version: 42, files: { "docs/readme.md": "y" } };
    botSources.getBotSource.mockResolvedValue(other);
    view.rerender(
      <BundleFilesDrawer teamID="team-1" slug="other" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    await waitFor(() =>
      expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe("# mine\n"),
    );

    // Saving removes the reason the drawer was pinned; it must then follow.
    botSources.putBotSourceFile.mockResolvedValue({ ...BUNDLE, version: 8 });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByRole("button", { name: "docs/readme.md" });
  });

  it("does not ask about a brand-new file nobody has typed in", async () => {
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    render(<BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />);
    fireEvent.click(await screen.findByRole("button", { name: "New file" }));
    fireEvent.change(await screen.findByRole("textbox"), {
      target: { value: "skills/fresh.md" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    await screen.findByLabelText("file");

    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    await waitFor(() => expect(screen.queryByLabelText("file")).toBeNull());
    expect(screen.queryByRole("button", { name: "Discard" })).toBeNull();
  });
});

// Round 7: an open dialog is not a dirty buffer, so the follow gate does not
// see it — the drawer can rebind underneath a prompt and resolve its answer
// against the bundle it left.
describe("BundleFilesDrawer when the editor moves under an open dialog", () => {
  it("refuses to create a file resolved against the bundle it left", async () => {
    const other = {
      ...BUNDLE,
      slug: "other",
      version: 42,
      files: { "main.bot": "x", "docs/readme.md": "REAL CONTENT — must not be truncated\n" },
    };
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    const view = render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "New file" }));

    // The editor moves while the prompt is up. The buffer is not dirty — it
    // does not exist yet — so nothing asks.
    botSources.getBotSource.mockResolvedValue(other);
    view.rerender(
      <BundleFilesDrawer teamID="team-1" slug="other" open onOpenChange={() => {}} />,
    );

    // `docs/readme.md` exists in `other` and not in `demo`, so an answer
    // resolved against the LEFT bundle takes the "create" branch: an empty
    // buffer, Save enabled from birth, and the first click writes "" over a
    // real file under a token the store has every reason to accept.
    fireEvent.change(await screen.findByRole("textbox"), {
      target: { value: "docs/readme.md" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));

    // The answer is dropped, and said out loud rather than swallowed.
    await waitFor(() =>
      expect(
        useUIStore.getState().toasts.map((t) => t.message).join(" "),
      ).toContain("moved to other"),
    );
    expect(screen.queryByLabelText("file")).toBeNull();
    expect(botSources.putBotSourceFile).not.toHaveBeenCalled();
  });
});

// Round 7, a regression of round 6's own latch: releasing it on "the buffer
// is clean" made the conflict Reload — which cleans the buffer WITHOUT
// closing it — rebind the drawer to the bot the author had just refused to
// follow, so the reloaded content they asked to see was never shown.
describe("BundleFilesDrawer reloading after a conflict, having declined to follow", () => {
  it("stays on the bundle the author kept", async () => {
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    botSources.putBotSourceFile.mockRejectedValue(
      new ApiError(409, "API error 409: version conflict", undefined, "version conflict"),
    );
    const view = render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "skills/notes.md" }));
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "# mine\n" } });

    // The editor moves; the author keeps their text.
    const other = { ...BUNDLE, slug: "other", version: 42, files: { "docs/readme.md": "y" } };
    botSources.getBotSource.mockResolvedValue(other);
    view.rerender(
      <BundleFilesDrawer teamID="team-1" slug="other" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    await waitFor(() =>
      expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe("# mine\n"),
    );

    // A 409, then the banner's Reload — which promises "Reload to see it".
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText(/This bot changed in the store/);
    botSources.getBotSource.mockResolvedValue({
      ...BUNDLE,
      version: 9,
      files: { ...BUNDLE.files, "skills/notes.md": "# theirs\n" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    fireEvent.click(await screen.findByRole("button", { name: "Reload and discard" }));

    // The reloaded content of the bundle the author KEPT, not a jump to the
    // one they refused.
    await waitFor(() =>
      expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe("# theirs\n"),
    );
    // The rebind, if it happens, lands after the reload settles — assert
    // once it has had the chance, or the test passes for want of waiting.
    await new Promise((r) => setTimeout(r, 80));
    expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe("# theirs\n");
    expect(screen.queryByRole("button", { name: "docs/readme.md" })).toBeNull();
  });
});

// Round 7: the follow effect can be torn down while the question it asked is
// still on screen (a save lands, the buffer goes clean, `dirty` is in the
// deps). `useConfirm` keeps the dialog up until it is settled, and a Radix
// modal aria-hides and pointer-blocks the whole app — so an unanswerable
// question would outlive the move it was asking about.
describe("BundleFilesDrawer when the follow question is overtaken", () => {
  it("takes its own dialog down instead of leaving a modal nobody can answer", async () => {
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    let settleSave!: (v: unknown) => void;
    botSources.putBotSourceFile.mockReturnValue(new Promise((r) => { settleSave = r; }));
    const view = render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "skills/notes.md" }));
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "# mine\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    const other = { ...BUNDLE, slug: "other", version: 42, files: { "docs/readme.md": "y" } };
    botSources.getBotSource.mockResolvedValue(other);
    view.rerender(
      <BundleFilesDrawer teamID="team-1" slug="other" open onOpenChange={() => {}} />,
    );
    await screen.findByText(/The editor moved to another bot/);

    // The in-flight save lands: the buffer is clean and closed, so the
    // question is moot.
    settleSave({ ...BUNDLE, version: 8 });
    await waitFor(() =>
      expect(screen.queryByText(/The editor moved to another bot/)).toBeNull(),
    );
  });

  it("keeps a brand-new file creatable after a conflict reload", async () => {
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    botSources.putBotSourceFile.mockRejectedValue(
      new ApiError(409, "API error 409: version conflict", undefined, "version conflict"),
    );
    render(<BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />);
    fireEvent.click(await screen.findByRole("button", { name: "New file" }));
    fireEvent.change(await screen.findByRole("textbox"), { target: { value: "skills/fresh.md" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    fireEvent.change(await screen.findByLabelText("file"), { target: { value: "# draft\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText(/This bot changed in the store/);

    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    fireEvent.click(await screen.findByRole("button", { name: "Reload and discard" }));

    // `created` is part of the buffer's identity: rebuilt without it, Save
    // was disabled for ever on a file that does not exist yet.
    await waitFor(() =>
      expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe(""),
    );
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
  });
});
