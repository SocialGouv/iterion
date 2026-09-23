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

afterEach(() => {
  cleanup();
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
  it("drops the buffer and the refusal rather than writing them into the new bot", async () => {
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
