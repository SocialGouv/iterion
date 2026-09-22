// @vitest-environment jsdom
//
// The Source view of a bot in several files. Before #1227 it showed the
// merged program, read-only, and told the author to "open each file from
// the files drawer" — a control that renders only for a cloud
// `botsource://` path, so a local author was sent to something that was
// not on their screen. It is now a picker over the unit's files.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  parseSource: vi.fn(),
  unparse: vi.fn(),
  unparseUnitFile: vi.fn(),
  parseUnitFile: vi.fn(),
}));
vi.mock("@/api/client", () => api);
vi.mock("@/lib/monaco", () => ({
  default: ({
    value,
    onChange,
    options,
  }: {
    value?: string;
    onChange?: (v?: string) => void;
    options?: { readOnly?: boolean };
  }) => (
    <textarea
      aria-label="source"
      data-readonly={String(!!options?.readOnly)}
      value={value ?? ""}
      onChange={(e) => onChange?.(e.target.value)}
    />
  ),
}));
vi.mock("@/lib/iterLanguage", () => ({
  ITER_LANGUAGE_ID: "iter",
  iterLanguageConfig: {},
  iterTokensProvider: {},
}));
vi.mock("@/lib/iterMonacoCompletion", () => ({ registerIterCompletionProvider: vi.fn() }));

import { createEmptyDocument } from "@/lib/defaults";
import { DocumentStoreProvider, createDocumentStore } from "@/store/document";
import type { UnitInfo } from "@/api/types";

import SourceView, { MERGED } from "./SourceView";

const MAIN_TEXT = 'import "lib/nodes.bot"\n\nworkflow w:\n  entry: worker\n';
const FRAGMENT_TEXT = "agent worker:\n  model: \"anthropic/claude-opus-5\"\n";

const unit: UnitInfo = {
  root: "bots/demo",
  main: "main.bot",
  revision: "rev-1",
  files: [
    { rel: "main.bot", imports: ["lib/nodes.bot"] },
    { rel: "lib/nodes.bot" },
  ],
};

function unitStore() {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/demo/main.bot");
  store.getState().setUnit(unit);
  store.getState().setCurrentSource(MAIN_TEXT);
  store.getState().markSaved();
  return store;
}

function renderView(store: ReturnType<typeof unitStore>) {
  return render(
    <DocumentStoreProvider store={store}>
      <SourceView />
    </DocumentStoreProvider>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  api.unparse.mockResolvedValue({ source: "merged program" });
  api.unparseUnitFile.mockImplementation(async (_doc: unknown, _path: string, rel: string) => ({
    source: rel === "main.bot" ? MAIN_TEXT : FRAGMENT_TEXT,
  }));
});

afterEach(cleanup);

describe("the Source view of a bot in several files", () => {
  it("lists the unit's files and opens on its main, not on the merged program", async () => {
    const store = unitStore();
    renderView(store);

    const picker = (await screen.findByTestId("source-view-file-picker")) as HTMLSelectElement;
    // Every file of the unit is offered, plus the merged program — which is
    // an entry, no longer the only thing the view can show.
    expect([...picker.options].map((o) => o.textContent)).toEqual([
      "main.bot (main)",
      "lib/nodes.bot",
      "Merged program of 2 files (read-only)",
    ]);
    expect(picker.value).toBe("main.bot");
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
    // The forbidden alternative, named: the merged program, which is what
    // this view showed before and what no save can take apart.
    expect(api.unparse).not.toHaveBeenCalled();
  });

  it("shows the file the picker is on, and that file alone", async () => {
    const store = unitStore();
    renderView(store);
    await screen.findByTestId("source-view-file-picker");

    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "lib/nodes.bot" },
    });

    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(FRAGMENT_TEXT),
    );
    expect(api.unparseUnitFile).toHaveBeenCalledWith(
      expect.anything(),
      "bots/demo/main.bot",
      "lib/nodes.bot",
    );
  });

  it("applies an edit through the unit, and keeps the revision it opened at", async () => {
    const store = unitStore();
    const EDITED = FRAGMENT_TEXT.replace("opus-5", "opus-4-8");
    api.parseUnitFile.mockResolvedValue({
      document: createEmptyDocument(),
      diagnostics: [],
      bindable: true,
      // The server answers no revision: an overlay moved no file at rest.
      unit: { root: "", main: "main.bot", revision: "", files: unit.files },
    });
    renderView(store);
    await screen.findByTestId("source-view-file-picker");
    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "lib/nodes.bot" },
    });
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(FRAGMENT_TEXT),
    );

    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: EDITED } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));

    await waitFor(() =>
      expect(api.parseUnitFile).toHaveBeenCalledWith("bots/demo/main.bot", "lib/nodes.bot", EDITED),
    );
    // The revision is a claim about the files at REST, and the apply moved
    // none. Taking the server's empty one — or a staged digest — would make
    // the very next save a conflict against a file nobody touched.
    await waitFor(() => expect(store.getState().unit?.revision).toBe("rev-1"));
    // The file list does follow: an edit may add or drop an import.
    expect(store.getState().unit?.files).toHaveLength(2);
    // Never the whole-document parse: that is the path that folds every
    // file into the main.
    expect(api.parseSource).not.toHaveBeenCalled();
  });

  it("shows a file the writer cannot reproduce as it is on disk, and refuses to edit it", async () => {
    const ON_DISK = "dsl: 2\n\ntool t:\n  command: `line one\nline two`\n";
    api.unparseUnitFile.mockResolvedValue({
      source: ON_DISK,
      refused: "the value at line 4 is written over several lines and the writer has no form for one",
    });
    const store = unitStore();
    renderView(store);
    await screen.findByTestId("source-view-file-picker");
    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "lib/nodes.bot" },
    });

    // The FILE's text, not the writer's — which would put the two lines on
    // one and show the author a text their file does not contain.
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(ON_DISK),
    );
    // Asserted through the live region, not a test id: a screen-reader
    // user who changes the picker has to be TOLD the file became
    // read-only, which a hand-rolled div does not do.
    const banner = await screen.findByRole("status");
    expect(banner.textContent).toMatch(/cannot be edited here/i);
    // No Edit: the save of this file is refused too, so offering it would
    // only lead the author to a 422 after they typed.
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  });

  it("keeps the merged program available, and read-only", async () => {
    const store = unitStore();
    renderView(store);
    await screen.findByTestId("source-view-file-picker");

    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: MERGED },
    });

    await waitFor(() =>
      expect(api.unparse).toHaveBeenCalledWith(expect.anything(), {
        flatten: true,
        path: "bots/demo/main.bot",
      }),
    );
    expect(await screen.findByTestId("source-view-unit-note")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  });

  it("falls back to the main when the file it was on is deleted from under it", async () => {
    const store = unitStore();
    renderView(store);
    await screen.findByTestId("source-view-file-picker");
    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "lib/nodes.bot" },
    });
    await waitFor(() =>
      expect((screen.getByTestId("source-view-file-picker") as HTMLSelectElement).value).toBe(
        "lib/nodes.bot",
      ),
    );

    // The watcher reloaded the unit and the fragment is gone.
    store.getState().setUnit({ ...unit, revision: "rev-2", files: [{ rel: "main.bot" }] });

    // Asserted on what the view ASKS FOR, never on `select.value`: a
    // single-select whose value matches no option is reset by the DOM to
    // its first option, which here is the main — so the value assertion
    // stays green with the fallback deleted while the component keeps
    // asking the server for a file that is gone.
    await waitFor(() => {
      const calls = api.unparseUnitFile.mock.calls;
      expect(calls[calls.length - 1]?.[2]).toBe("main.bot");
    });
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
  });

  // The debounce coalesces SELECTION changes, not RESPONSES: once the timer
  // has fired the fetch is in flight, and the effect's cleanup cannot stop
  // it. A late answer for file A landing while the picker says B is not a
  // cosmetic glitch — the next Apply then sends A's text under B's name,
  // and the server cannot tell, because that text parses.
  it("drops a render that arrives after the picker moved on", async () => {
    const store = unitStore();
    let releaseMain: (v: { source: string }) => void = () => {};
    api.unparseUnitFile.mockImplementation(async (_d: unknown, _p: string, rel: string) => {
      if (rel === "main.bot") return new Promise((res) => (releaseMain = res));
      return { source: FRAGMENT_TEXT };
    });
    api.parseUnitFile.mockResolvedValue({
      document: createEmptyDocument(),
      diagnostics: [],
      bindable: true,
      unit: { root: "", main: "main.bot", revision: "", files: unit.files },
    });
    renderView(store);
    await screen.findByTestId("source-view-file-picker");

    // The main's render is in flight and unanswered; the author moves on.
    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "lib/nodes.bot" },
    });
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(FRAGMENT_TEXT),
    );

    // The main answers late.
    releaseMain({ source: MAIN_TEXT });
    await new Promise((r) => setTimeout(r, 50));

    // The forbidden alternative, named: the MAIN's text under the
    // fragment's name.
    expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(FRAGMENT_TEXT);

    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalled());
    expect(api.parseUnitFile.mock.calls[0]).toEqual([
      "bots/demo/main.bot",
      "lib/nodes.bot",
      FRAGMENT_TEXT,
    ]);
  });

  // The same guard, from the other side: a render answering while the
  // author types must not put the file back over what they wrote. The
  // component says it prevents exactly this; the check was on the wrong
  // side of the await.
  it("does not replace what the author is typing with a render that lands mid-edit", async () => {
    const store = unitStore();
    let release: (v: { source: string }) => void = () => {};
    let pending = false;
    api.unparseUnitFile.mockImplementation(async (_d: unknown, _p: string, rel: string) => {
      if (!pending) return { source: rel === "main.bot" ? MAIN_TEXT : FRAGMENT_TEXT };
      return new Promise<{ source: string }>((res) => (release = res));
    });
    renderView(store);
    // The first render has to LAND before Edit can be entered — the buffer
    // must hold the file the view is about, or entering the mode would
    // freeze the previous one's text there. So the in-flight render this
    // test is about is a LATER one, started by a dep change that leaves the
    // buffer's identity alone.
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
    pending = true;
    store.getState().setDocument(createEmptyDocument());
    await waitFor(() => expect(api.unparseUnitFile).toHaveBeenCalledTimes(2));

    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "MY TYPED REPAIR" } });
    release({ source: MAIN_TEXT });
    await new Promise((r) => setTimeout(r, 50));

    expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe("MY TYPED REPAIR");
  });

  // currentSource is what a cloud launch and every resume send inline AS
  // the program (store/document.ts). A fragment declares no workflow, so
  // writing one there would make them run something that is not this bot.
  it("never lets a fragment's text become the program the launch sends", async () => {
    const store = unitStore();
    api.parseUnitFile.mockResolvedValue({
      document: createEmptyDocument(),
      diagnostics: [],
      bindable: true,
      unit: { root: "", main: "main.bot", revision: "", files: unit.files },
    });
    renderView(store);
    await screen.findByTestId("source-view-file-picker");
    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "lib/nodes.bot" },
    });
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(FRAGMENT_TEXT),
    );

    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "agent worker:\n  model: \"m\"\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalled());

    expect(store.getState().currentSource).toBe(MAIN_TEXT);
    // That apply left the document unsaved, so the next one — which
    // rebuilds from the STORED files — asks first.
    store.getState().markSaved();

    // And the main's own edit DOES land there — without this the test
    // would pass on a component that never writes currentSource at all.
    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "main.bot" },
    });
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "EDITED MAIN" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(store.getState().currentSource).toBe("EDITED MAIN"));
  });

  // A salvage is about the MAIN. Left on the merged entry the author faces
  // a disabled picker and no Edit, while every write refuses and points
  // them here — a refusal naming a control they cannot reach, which is the
  // defect this view was changed to remove.
  it("moves to the main when the document becomes a salvage", async () => {
    const store = unitStore();
    renderView(store);
    await screen.findByTestId("source-view-file-picker");
    fireEvent.change(screen.getByTestId("source-view-file-picker"), { target: { value: MERGED } });
    await waitFor(() =>
      expect((screen.getByTestId("source-view-file-picker") as HTMLSelectElement).value).toBe(
        MERGED,
      ),
    );

    // The watcher reloaded a main that no longer parses.
    store.getState().setSalvaged(true);
    store.getState().setUnit({ ...unit, revision: "rev-3" });

    await waitFor(() =>
      expect((screen.getByTestId("source-view-file-picker") as HTMLSelectElement).value).toBe(
        "main.bot",
      ),
    );
    // The file's own text — and NO Edit. A save of a bot in several files
    // re-derives the unit from the files as they are stored and refuses
    // one that does not load, so nothing typed here could reach the main.
    // Offering Edit made it worse: the Apply answered 200 and cleared the
    // salvage, so the refusal that named the way out was gone by the time
    // the Save refused.
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(await screen.findByTestId("source-view-salvaged-unit-note")).toBeTruthy();
  });

  // The toolbar's own file picker is NOT disabled by this view's Edit mode,
  // so the tab can change file while an Apply is in flight. Writing the
  // answer then leaves the store holding the NEW bot's path beside the OLD
  // bot's document and revision, and the next Save presents that revision
  // against the new file. The render effect's generation cannot stand in
  // for this guard: it is bumped by the document change the Apply causes.
  it("applies nothing when the editor changed file while the apply was in flight", async () => {
    const store = unitStore();
    let release: (v: unknown) => void = () => {};
    api.parseUnitFile.mockImplementation(() => new Promise((res) => (release = res)));
    renderView(store);
    await screen.findByTestId("source-view-file-picker");
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );

    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "EDITED" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalled());

    // The author opened another bot while the round trip was out.
    const otherUnit = { root: "bots/other", main: "main.bot", revision: "rev-OTHER", files: [{ rel: "main.bot" }] };
    store.getState().setCurrentFilePath("bots/other/main.bot");
    store.getState().setUnit(otherUnit);

    release({
      document: createEmptyDocument(),
      diagnostics: [],
      bindable: true,
      unit: { root: "", main: "main.bot", revision: "", files: unit.files },
    });
    await new Promise((r) => setTimeout(r, 50));

    // The forbidden alternative, named: the OLD bot's unit under the NEW
    // bot's path, whose revision the next Save would present.
    expect(store.getState().unit?.revision).toBe("rev-OTHER");
    expect(store.getState().unit?.files).toHaveLength(1);
    expect(store.getState().currentFilePath).toBe("bots/other/main.bot");
  });

  // The Source view's buffer is local to this component, so the store's
  // _generation never moves and isDirty() cannot see an open edit. The file
  // watcher reads isDirty() to decide whether to reload a file changed on
  // disk — so without this flag it swaps the document AND the revision
  // under what the author is typing, and the Apply that follows lands on
  // top of whoever wrote the file meanwhile.
  it("tells the store an edit is open, so the watcher cannot reload under it", async () => {
    const store = unitStore();
    const { unmount } = renderView(store);
    await screen.findByTestId("source-view-file-picker");
    expect(store.getState().sourceEditing).toBe(false);

    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    expect(store.getState().sourceEditing).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(store.getState().sourceEditing).toBe(false);

    // And it is released when the view goes away, or the watcher would
    // never reload anything again.
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    expect(store.getState().sourceEditing).toBe(true);
    unmount();
    expect(store.getState().sourceEditing).toBe(false);
  });

  // `editing` is this component's state; `unit` and `salvaged` are the
  // store's, and other surfaces move them — opening another bot mid-edit,
  // above all. Every branch that makes the view read-only also hides Apply
  // and Cancel, so a mode left set has no control to leave it: the editor
  // stays writable under a label saying it is not, the sync keeps returning
  // early, and the PREVIOUS bot's buffer sits on screen as this one's file.
  it("leaves edit mode when the view stops being editable", async () => {
    const store = unitStore();
    renderView(store);
    await screen.findByTestId("source-view-file-picker");
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "TYPED BY THE AUTHOR" } });
    expect(screen.getByLabelText("source")).toHaveProperty("dataset.readonly", "false");

    // The author opens another bot, whose main did not parse.
    const OTHER = 'import "lib/nodes.bot"\n@@@\n';
    store.getState().setCurrentFilePath("bots/other/main.bot");
    store.getState().setCurrentSource(OTHER);
    store.getState().setSalvaged(true);
    store.getState().setUnit({ ...unit, root: "bots/other", revision: "rev-other" });

    // The forbidden alternative, named: the previous bot's typed buffer,
    // still writable, under the read-only note.
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(OTHER),
    );
    expect(screen.getByLabelText("source")).toHaveProperty("dataset.readonly", "true");
    expect(screen.queryByRole("button", { name: "Apply" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Cancel" })).toBeNull();
    expect(await screen.findByTestId("source-view-salvaged-unit-note")).toBeTruthy();
  });

  // `editable` says the view is ABOUT a file a save could land on. It says
  // nothing about what the editor HOLDS, and the two are apart for the
  // whole debounce plus the round trip after every switch. Entering the
  // mode there froze the previous file's text — the render effect returns
  // early while editing, so it never caught up — and Apply sent it under
  // the newly selected name. That text parses, so no server can tell.
  it("does not offer Edit until the buffer holds the file the picker is on", async () => {
    const store = unitStore();
    api.parseUnitFile.mockResolvedValue({
      document: createEmptyDocument(),
      diagnostics: [],
      bindable: true,
      unit: { root: "", main: "main.bot", revision: "", files: unit.files },
    });
    renderView(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();

    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "lib/nodes.bot" },
    });
    // The picker already says lib/nodes.bot while the buffer still holds
    // the main. The forbidden alternative, named: Edit offered here.
    expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT);
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();

    // Once the fragment lands, Edit is back and Apply sends THAT file.
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(FRAGMENT_TEXT),
    );
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "agent worker:\n  model: \"m\"\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalled());
    expect(api.parseUnitFile.mock.calls[0]?.[2]).not.toContain("workflow w:");
  });

  // The case the effect names first — "opening another bot mid-edit" —
  // and the one it missed: a bot in ONE file keeps `editable` true through
  // `!unit`, so the mode survived, the store's mirror was cleared by
  // setCurrentFilePath, and Apply sent bot A's text as bot B's program.
  it("leaves edit mode when another bot is opened, even one in a single file", async () => {
    const store = unitStore();
    renderView(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "TYPED INTO BOT A" } });

    // Bot B: one file, so `unit` is null and `editable` stays true.
    store.getState().setCurrentFilePath("bots/b.bot");
    store.getState().setCurrentSource("workflow b:\n  entry: done\n");

    await waitFor(() => expect(screen.queryByRole("button", { name: "Apply" })).toBeNull());
    expect(screen.getByLabelText("source")).toHaveProperty("dataset.readonly", "true");
    // The forbidden alternative, named: bot A's text committed as bot B's
    // program — which is also what a cloud launch would send inline.
    expect(api.parseSource).not.toHaveBeenCalled();
    expect(store.getState().currentSource).not.toBe("TYPED INTO BOT A");
  });

  // `salvaged` flips `editable` on its own: applyParsedSource sets the
  // document and the flag and moves nothing else, which is exactly what the
  // assistant's Apply-proposal does. A re-check reading only the path and
  // the unit could not see it, so a stale answer overwrote the document the
  // operator had just accepted AND cleared the one flag that refuses every
  // write.
  it("applies nothing when the document became a salvage while the apply was in flight", async () => {
    const store = unitStore();
    let release: (v: unknown) => void = () => {};
    api.parseUnitFile.mockImplementation(() => new Promise((res) => (release = res)));
    renderView(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "REPAIRED MAIN" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalled());

    // The assistant applied a proposal: document + salvaged, nothing else.
    store.getState().setDocument(createEmptyDocument());
    store.getState().setSalvaged(true);

    release({
      document: createEmptyDocument(),
      diagnostics: [],
      bindable: true,
      unit: { root: "", main: "main.bot", revision: "", files: unit.files },
    });
    await new Promise((r) => setTimeout(r, 50));

    // The forbidden alternative, named: the salvage verdict cleared by an
    // answer nobody was waiting for any more.
    expect(store.getState().salvaged).toBe(true);
  });

  // What the render reads has to be what re-renders it. Keyed on a ref, a
  // render that landed byte-identical scheduled nothing — React bails out
  // of setState on an equal value — so the buffer's identity was right and
  // the component never saw it: Edit stayed hidden, and this view has no
  // control that could bring it back.
  it("offers Edit again when the render that lands is byte-identical", async () => {
    const store = unitStore();
    store.getState().setUnit({
      ...unit,
      files: [{ rel: "main.bot", imports: ["lib/nodes.bot"] }, { rel: "lib/twin.bot" }],
    });
    // Two files of one bot with the same stored text — a duplicated stub.
    api.unparseUnitFile.mockResolvedValue({ source: MAIN_TEXT });
    renderView(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();

    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "lib/twin.bot" },
    });
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();

    // The render for the new file lands — with the same bytes.
    await waitFor(() => {
      const calls = api.unparseUnitFile.mock.calls;
      expect(calls[calls.length - 1]?.[2]).toBe("lib/twin.bot");
    });
    // The forbidden alternative, named: Edit never returning, with nothing
    // in this view able to restore it.
    await waitFor(() => expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy());
  });

  // The document can move under the SAME file — the assistant applying a
  // proposal does exactly that, and nothing else. A buffer typed before it
  // and applied after it wrote the pre-proposal text back over what the
  // operator had just accepted: text that parses, so no server refuses it.
  it("applies nothing when the document moved under the same file", async () => {
    const store = unitStore();
    let release: (v: unknown) => void = () => {};
    api.parseUnitFile.mockImplementation(() => new Promise((res) => (release = res)));
    renderView(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "TYPED BEFORE THE PROPOSAL" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalled());

    // The assistant applied a proposal: the document moves, the path, the
    // unit and the salvage verdict do not.
    const proposal = { ...createEmptyDocument(), workflows: [{ name: "from-the-proposal", entry: "done", edges: [] }] };
    store.getState().setDocument(proposal as never);

    release({
      document: createEmptyDocument(),
      diagnostics: [],
      bindable: true,
      unit: { root: "", main: "main.bot", revision: "", files: unit.files },
    });
    await new Promise((r) => setTimeout(r, 50));

    // The forbidden alternative, named: the proposal reverted by an answer
    // written against the document it replaced.
    expect(store.getState().document?.workflows?.[0]?.name).toBe("from-the-proposal");
  });

  // The per-file apply sends one text and no document: the server rebuilds
  // the merged program from the bot's files AS STORED plus that overlay, so
  // an unsaved canvas edit in ANOTHER file of the unit is replaced. The
  // salvage confirm does not cover it — it is gated on `salvaged`, and its
  // reasoning holds for a bot in ONE file, where the buffer is the document.
  it("asks before rebuilding a unit that has unsaved canvas edits", async () => {
    const store = unitStore();
    api.parseUnitFile.mockResolvedValue({
      document: createEmptyDocument(),
      diagnostics: [],
      bindable: true,
      unit: { root: "", main: "main.bot", revision: "", files: unit.files },
    });
    renderView(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );
    // The author edited the canvas: the buffer moved past its saved mark.
    store.getState().setDocument(createEmptyDocument());
    expect(store.getState().isDirty()).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "EDITED MAIN" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));

    // The forbidden alternative, named: applying straight through.
    expect(await screen.findByText(/other files are not included/i)).toBeTruthy();
    expect(api.parseUnitFile).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Rebuild" }));
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalled());
  });

  // `isDirty()` measures "the document moved since the last save"; what
  // decides here is "it holds an overlay for a file OTHER than this one".
  // They part on the commonest loop — edit, Apply, look, edit, Apply — and
  // a danger dialog every time after the first teaches the author to click
  // through it, which is how a real warning stops being read.
  it("does not ask when the only unsaved change is its own apply to this same file", async () => {
    const store = unitStore();
    let served = 0;
    api.parseUnitFile.mockImplementation(async () => {
      served += 1;
      return {
        document: { ...createEmptyDocument(), workflows: [{ name: `applied-${served}`, entry: "done", edges: [] }] },
        diagnostics: [],
        bindable: true,
        unit: { root: "", main: "main.bot", revision: "", files: unit.files },
      };
    });
    renderView(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );

    const applyOnce = async (text: string) => {
      fireEvent.click(screen.getByRole("button", { name: "Edit" }));
      fireEvent.change(screen.getByLabelText("source"), { target: { value: text } });
      fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    };

    await applyOnce("EDIT ONE");
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalledTimes(1));
    // The document is now dirty — from THIS view's own apply, on this file.
    expect(store.getState().isDirty()).toBe(true);

    await applyOnce("EDIT TWO");
    // The forbidden alternative, named: a danger dialog for a rebuild that
    // drops only the text being replaced.
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalledTimes(2));
    expect(screen.queryByText(/other files are not included/i)).toBeNull();

    // And it fires again as soon as the document moved for a reason that
    // is NOT this view's own apply — a canvas edit between two applies to
    // the same file is dropped by the rebuild just the same.
    store.getState().setDocument(createEmptyDocument());
    await applyOnce("EDIT THREE");
    expect(await screen.findByText(/other files are not included/i)).toBeTruthy();
    expect(api.parseUnitFile).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole("button", { name: "Rebuild" }));
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalledTimes(3));

    // And where something IS dropped: another file.
    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "lib/nodes.bot" },
    });
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(FRAGMENT_TEXT),
    );
    await applyOnce("FRAGMENT EDIT");
    expect(await screen.findByText(/other files are not included/i)).toBeTruthy();
    expect(api.parseUnitFile).toHaveBeenCalledTimes(3);
  });

  // A document change landing between the last completed render and the
  // Apply click is invisible to a snapshot taken at the click: `stale`
  // carries no document, so Edit stays clickable over the pre-change
  // render, and entering the mode cancels the pending re-render — the
  // stale text then freezes for as long as the mode is on. Applying it
  // writes that file's whole content and reverts the change silently.
  it("applies nothing when the buffer was rendered from a superseded document", async () => {
    const store = unitStore();
    api.parseUnitFile.mockResolvedValue({
      document: createEmptyDocument(),
      diagnostics: [],
      bindable: true,
      unit: { root: "", main: "main.bot", revision: "", files: unit.files },
    });
    renderView(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT),
    );

    // The canvas moves the document; the author clicks Edit before the
    // re-render lands, so the buffer still holds the old render.
    const moved = { ...createEmptyDocument(), workflows: [{ name: "from-the-canvas", entry: "done", edges: [] }] };
    store.getState().setDocument(moved as never);
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(MAIN_TEXT);
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "APPLIED FROM A STALE RENDER" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    // The rebuild confirm fires first (the document is dirty), and that is
    // not what this test is about — click through it.
    fireEvent.click(await screen.findByRole("button", { name: "Rebuild" }));

    // The forbidden alternative, named: the canvas change reverted by a
    // text rendered before it.
    await waitFor(() => expect(screen.getByText(/changed while this was applying/i)).toBeTruthy());
    // The request goes out and its answer is discarded — moved() is read
    // after the await, which is where the store can be compared. What
    // matters is that nothing landed.
    expect(store.getState().document?.workflows?.[0]?.name).toBe("from-the-canvas");
  });
});
