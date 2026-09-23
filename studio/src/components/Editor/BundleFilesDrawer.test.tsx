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

vi.mock("@/lib/monaco", () => ({
  default: ({
    value,
    onChange,
    language,
  }: {
    value?: string;
    onChange?: (v?: string) => void;
    language?: string;
  }) => (
    <textarea
      aria-label="file"
      data-language={language}
      value={value ?? ""}
      onChange={(e) => onChange?.(e.target.value)}
    />
  ),
}));

import BundleFilesDrawer from "./BundleFilesDrawer";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
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
