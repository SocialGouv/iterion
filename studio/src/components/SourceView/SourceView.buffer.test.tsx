// @vitest-environment jsdom
//
// The two measured residuals of #1662, from the outside: the Source view's
// buffer was component-local, so (1) no discard prompt anywhere could see an
// un-applied edit, and (2) leaving edit mode destroyed the typed text with no
// prompt and no undo — measured on the documented way out of a salvage, where
// the watcher's reload discovers the bot is in several files.
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  parseSource: vi.fn(),
  unparse: vi.fn(),
  unparseUnitFile: vi.fn(),
  saveFile: vi.fn(),
}));
vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  ...api,
}));
vi.mock("@/lib/monaco", () => ({
  default: ({ value, onChange }: { value?: string; onChange?: (v?: string) => void }) => (
    <textarea aria-label="source" value={value ?? ""} onChange={(e) => onChange?.(e.target.value)} />
  ),
}));
vi.mock("@/lib/iterLanguage", () => ({
  ITER_LANGUAGE_ID: "iter",
  iterLanguageConfig: {},
  iterTokensProvider: {},
}));
vi.mock("@/lib/iterMonacoCompletion", () => ({ registerIterCompletionProvider: vi.fn() }));

import { createEmptyDocument } from "@/lib/defaults";
import { applyOpenedFile } from "@/lib/openedFile";
import {
  DocumentStoreProvider,
  createDocumentStore,
  getOrCreateDocumentStore,
} from "@/store/document";

import SourceView, { MERGED } from "./SourceView";
import { useDropEditorTab } from "@/hooks/useDropEditorTab";
import DocumentSaveAsDialog from "@/components/DocumentSaveAs/DocumentSaveAsDialog";
import { useDocumentSaveAs } from "@/components/DocumentSaveAs/useDocumentSaveAs";
import { useUIStore } from "@/store/ui";



const RENDERED = "workflow w:\n  entry: a\n";

function openStore() {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/demo/main.bot");
  store.getState().markSaved();
  return store;
}

function mount(store: ReturnType<typeof openStore>) {
  return render(
    <DocumentStoreProvider store={store}>
      <SourceView />
    </DocumentStoreProvider>,
  );
}

async function startEditing(store: ReturnType<typeof openStore>) {
  mount(store);
  await screen.findByRole("button", { name: "Edit" });
  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  return screen.getByLabelText("source") as HTMLTextAreaElement;
}

beforeEach(() => {
  vi.resetAllMocks();
  api.unparse.mockResolvedValue({ source: RENDERED });
  api.unparseUnitFile.mockResolvedValue({ source: "agent worker:\n  model: \"m\"\n" });
});

afterEach(() => {
  cleanup();
  useUIStore.setState({ toasts: [] });
  useUIStore.setState({ sourceViewOpen: true, expanded: false });
});

describe("the Source view's buffer is visible outside the component", () => {
  it("publishes an open editor before anything is typed, and calls it clean", async () => {
    const store = openStore();
    await startEditing(store);
    await waitFor(() => expect(store.getState().sourceBuffer).not.toBeNull());
    // Present, so the watcher will not reload under it; not dirty, so no
    // prompt fires for an editor nobody has typed in.
    expect(store.getState().isSourceDirty()).toBe(false);
    expect(store.getState().hasUnsavedWork()).toBe(false);
  });

  it("reports unsaved work the moment the author types, though no document moved", async () => {
    const store = openStore();
    const buffer = await startEditing(store);
    fireEvent.change(buffer, { target: { value: `${RENDERED}  a -> done\n` } });

    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));
    expect(store.getState().isDirty()).toBe(false);
    expect(store.getState().hasUnsavedWork()).toBe(true);
    expect(store.getState().sourceBuffer?.text).toBe(`${RENDERED}  a -> done\n`);
    expect(store.getState().sourceBuffer?.base).toBe(RENDERED);
  });

  it("names the file the text is at stake for", async () => {
    const store = openStore();
    const buffer = await startEditing(store);
    fireEvent.change(buffer, { target: { value: "typed" } });
    await waitFor(() => expect(store.getState().sourceBuffer?.path).toBe("bots/demo/main.bot"));
  });

  it("outlives the view when it holds work, so the unmount takes nothing", async () => {
    const store = openStore();
    const buffer = await startEditing(store);
    fireEvent.change(buffer, { target: { value: "typed" } });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));
    cleanup();
    expect(store.getState().sourceBuffer).not.toBeNull();
    expect(store.getState().hasUnsavedWork()).toBe(true);
  });
});

describe("leaving edit mode", () => {
  it("asks before taking the typed text, and keeps it when the author says no", async () => {
    const store = openStore();
    const buffer = await startEditing(store);
    fireEvent.change(buffer, { target: { value: "a repair the author typed" } });

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    // The prompt exists at all, which is the residual: before, the text was
    // simply gone 500 ms later.
    const keep = await screen.findByRole("button", { name: "Cancel" });
    expect(screen.getByText(/has not been applied/i)).toBeTruthy();

    fireEvent.click(keep);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
        "a repair the author typed",
      ),
    );
    expect(store.getState().isSourceDirty()).toBe(true);
  });

  it("takes it once the author confirms, and closes the buffer", async () => {
    const store = openStore();
    const buffer = await startEditing(store);
    fireEvent.change(buffer, { target: { value: "a repair the author typed" } });

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard" }));

    await waitFor(() => expect(store.getState().sourceBuffer).toBeNull());
    expect(store.getState().hasUnsavedWork()).toBe(false);
  });

  it("asks nothing when nothing was typed", async () => {
    const store = openStore();
    await startEditing(store);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(store.getState().sourceBuffer).toBeNull());
    expect(screen.queryByText(/has not been applied/i)).toBeNull();
  });
});

// Closing a tab DISPOSES its store, which is the one unmount the view
// cannot adopt its buffer back from — and the two close controls sit outside
// the tab's DocumentStoreProvider, so a guard reading the contextual store
// there is inert.
describe("dropping the editor tab that holds typed text", () => {
  function TabProbe({ tabId }: { tabId?: string | null }) {
    const { guardDroppingEditorTab, dialog } = useDropEditorTab();
    return (
      <>
        <button onClick={() => void guardDroppingEditorTab(() => setDropped(true), tabId)}>
          drop
        </button>
        {dialog}
      </>
    );
  }

  let dropped = false;
  const setDropped = (v: boolean) => {
    dropped = v;
  };

  beforeEach(() => {
    dropped = false;
  });

  it("asks before dropping a tab whose Source pane holds typed text", async () => {
    const tabStore = getOrCreateDocumentStore("tab-1");
    tabStore.getState().setDocument(createEmptyDocument());
    tabStore.getState().setCurrentFilePath("bots/demo/main.bot");
    tabStore.getState().markSaved();
    tabStore.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "a repair the author typed",
      base: "workflow w:\n  entry: a\n",
      doc: null,
      session: 1,
    });

    // Rendered OUTSIDE any DocumentStoreProvider, as the sidebar and the tab
    // strip are: the guard has to find the tab's store in the registry.
    render(<TabProbe tabId="tab-1" />);
    fireEvent.click(screen.getByRole("button", { name: "drop" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    expect(dropped).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: "drop" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard" }));
    await waitFor(() => expect(dropped).toBe(true));
  });

  it("asks nothing for a tab with no un-applied text", async () => {
    const tabStore = getOrCreateDocumentStore("tab-2");
    tabStore.getState().setDocument(createEmptyDocument());
    tabStore.getState().markSaved();
    render(<TabProbe tabId="tab-2" />);
    fireEvent.click(screen.getByRole("button", { name: "drop" }));
    await waitFor(() => expect(dropped).toBe(true));
    expect(screen.queryByRole("button", { name: "Discard" })).toBeNull();
  });
});

// The chokepoint that replaced a guard per exit. The pane is mounted by
// conditions it does not own — the view toggle, the canvas expand, the
// active tab, the route — and guarding each exit found one more exit for
// three rounds running. The buffer outlives the unmount instead, and the
// next mount adopts it: hiding the pane no longer loses anything, so it no
// longer needs to ask.
describe("the buffer across an unmount of the pane", () => {
  it("comes back with the text, still dirty, still in edit mode", async () => {
    const store = openStore();
    const buffer = await startEditing(store);
    fireEvent.change(buffer, { target: { value: "a repair the author typed" } });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));

    cleanup(); // the toggle, the expand, the route change — all this unmount
    expect(store.getState().isSourceDirty()).toBe(true);

    mount(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
        "a repair the author typed",
      ),
    );
    // Still an open edit: Apply and Cancel are there, not Edit.
    expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy();
    expect(store.getState().isSourceDirty()).toBe(true);
  });

  it("keeps the provenance, so an Apply over a moved document is still refused", async () => {
    const store = openStore();
    const buffer = await startEditing(store);
    fireEvent.change(buffer, { target: { value: "a repair" } });
    await waitFor(() => expect(store.getState().sourceBuffer?.doc).not.toBeUndefined());
    const renderedFrom = store.getState().sourceBuffer!.doc;

    cleanup();
    // The canvas moved while the pane was shut.
    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "moved" }] });
    mount(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe("a repair"),
    );
    // The buffer still names the document it was rendered FROM. Re-seeded to
    // the current one, the Apply would overwrite what moved with no refusal.
    expect(store.getState().sourceBuffer!.doc).toBe(renderedFrom);
    expect(store.getState().document).not.toBe(renderedFrom);

    api.parseSource.mockResolvedValue({ document: createEmptyDocument(), diagnostics: [] });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await screen.findByText(/The editor changed while this was applying/);
  });

  it("drops a clean buffer, so the watcher may auto-reload again", async () => {
    const store = openStore();
    await startEditing(store);
    expect(store.getState().sourceBuffer).not.toBeNull();
    cleanup();
    expect(store.getState().sourceBuffer).toBeNull();
  });

  it("does not adopt a buffer belonging to another file", async () => {
    const store = openStore();
    const buffer = await startEditing(store);
    fireEvent.change(buffer, { target: { value: "typed for main" } });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));
    cleanup();

    // Each tab has its own store, and every path change drops the buffer
    // in the same `set`, so a buffer naming another file is built by hand:
    // it is text this tab has moved away from. It is not adopted over the
    // file on screen — and not held either, where nothing could show it.
    store.setState({
      sourceBuffer: { ...store.getState().sourceBuffer!, path: "bots/other/main.bot" },
    });
    mount(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(RENDERED),
    );
    await waitFor(() => expect(store.getState().sourceBuffer).toBeNull());
    expect(useUIStore.getState().toasts.map((t) => t.message).join(" ")).toContain(
      "The text you had not applied for bots/other/main.bot was discarded",
    );
  });
});

// Round 4's own findings: keeping the buffer across an unmount created a
// second class — a buffer that survives but is never reached, and an exit
// from edit mode that deleted it under the author's eyes.
describe("the buffer when the view turns read-only under it", () => {
  it("is not deleted in silence when a unit arrives under a whole-file edit — it is held, and the banner answers it", async () => {
    const store = openStore();
    const buffer = await startEditing(store);
    fireEvent.change(buffer, { target: { value: "a repair the author typed" } });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));

    // Built by hand: a real reload replaces the file through
    // `setCurrentFilePath`, which drops the buffer — and only after the
    // author chose "Reload and discard". A unit's view offers one file at a
    // time and never the whole, so this text can no longer be shown — but it
    // is the author's text, so it is HELD and the banner names it, rather
    // than a toast deciding to let it go (#1735). The view leaves edit mode;
    // Discard is the author's own answer now.
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });

    await waitFor(() => expect(screen.queryByRole("button", { name: "Apply" })).toBeNull());
    // Held, not dropped: `hasUnsavedWork` keeps answering for it, and the
    // banner is the surface the question lights for.
    expect(store.getState().sourceBuffer).not.toBeNull();
    expect(store.getState().hasUnsavedWork()).toBe(true);
    await screen.findByText("Un-applied text is still held for this file");
    // A whole-file text under a unit has no edit state to restore INTO —
    // the merged program is read-only — so only Discard is offered.
    expect(screen.queryByRole("button", { name: "Restore" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(store.getState().sourceBuffer).toBeNull());
    expect(screen.queryByRole("button", { name: "Discard" })).toBeNull();
  });

  it("holds work no picker can reach any more, and Restore puts it back into the editor", async () => {
    const store = openStore();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: "lib/nodes.bot",
      text: "typed for the fragment",
      base: "the fragment as rendered",
      doc: null,
      session: 1,
    });
    // The fragment is deleted from the bot. The picker can no longer select
    // it, so the buffer cannot be adopted on its own — it is held, and the
    // banner's Restore moves the view back to it (#1735).
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r2",
      files: [{ rel: "main.bot" }],
    });
    mount(store);

    await screen.findByText("Un-applied text is still held for lib/nodes.bot");
    expect(store.getState().sourceBuffer).not.toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Restore" }));
    // The text is back, in edit mode, for the file it was typed for.
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
        "typed for the fragment",
      ),
    );
    expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy();
    // The picker names the file it is on, held though it is.
    expect(screen.getByTestId("source-view-file-picker").textContent).toContain("lib/nodes.bot");
    // And the buffer is the one in the store, not a fresh copy.
    expect(store.getState().sourceBuffer?.text).toBe("typed for the fragment");
    expect(store.getState().sourceBuffer?.base).toBe("the fragment as rendered");
  });

  it("does not let Edit take the held text: the banner's answer comes first", async () => {
    const store = openStore();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: "lib/nodes.bot",
      text: "typed for the fragment",
      base: "the fragment as rendered",
      doc: null,
      session: 1,
    });
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r2",
      files: [{ rel: "main.bot" }],
    });
    mount(store);

    // Entering the mode publishes a FRESH buffer over the held one — the
    // held text would be taken without an answer. Until it is answered,
    // Edit stands down.
    const edit = await screen.findByRole("button", { name: "Edit" }, { timeout: 2000 });
    expect((edit as HTMLButtonElement).disabled).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(store.getState().sourceBuffer).toBeNull());
    await waitFor(() =>
      expect((screen.getByRole("button", { name: "Edit" }) as HTMLButtonElement).disabled).toBe(
        false,
      ),
    );
  });

  it("holds a FRAGMENT's text once the tab has no unit, and the banner's Discard answers it", async () => {
    // With no unit the view shows the whole file only, so a buffer typed for
    // one file of a unit can never be adopted. It used to be dropped with a
    // toast; it is held now, with the banner as its surface — unsaved work
    // stays true until the author answers (#1735).
    const store = openStore();
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: "lib/nodes.bot",
      text: "typed for the fragment",
      base: "the fragment as rendered",
      doc: null,
      session: 1,
    });
    expect(store.getState().hasUnsavedWork()).toBe(true);
    mount(store);

    await screen.findByText("Un-applied text is still held for lib/nodes.bot");
    expect(store.getState().sourceBuffer).not.toBeNull();
    expect(store.getState().hasUnsavedWork()).toBe(true);
    // A fragment with no unit left has no edit state to restore INTO.
    expect(screen.queryByRole("button", { name: "Restore" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(store.getState().sourceBuffer).toBeNull());
    expect(store.getState().hasUnsavedWork()).toBe(false);
  });

  it("comes back on the FRAGMENT it was typed for, not on the main", async () => {
    const store = openStore();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: "lib/nodes.bot",
      text: "typed for the fragment",
      base: "the fragment as rendered",
      doc: null,
      session: 1,
    });
    // `selected` is component state and does not survive the unmount, so a
    // remount lands on the main and the fragment's buffer was never adopted.
    mount(store);

    await waitFor(() =>
      expect((screen.getByTestId("source-view-file-picker") as HTMLSelectElement).value).toBe(
        "lib/nodes.bot",
      ),
    );
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
        "typed for the fragment",
      ),
    );
  });
});

// The predicate cannot depend on WHICH kind of unsaved work it is. Closing a
// tab disposes its store, taking the document and the Source buffer alike —
// and `hasUnsavedWork`'s own comment says "a reload, a tab close or a File →
// New takes both", which was not true of the tab close.
describe("a salvage appearing under an edit of the MAIN", () => {
  it("settles read-only and keeps the text, rather than adopting it back into a view that cannot edit", async () => {
    const store = openStore();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    store.getState().setCurrentSource("the main as stored");
    mount(store);
    await waitFor(() => expect(screen.queryByRole("button", { name: "Edit" })).not.toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "a repair of the main" } });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));

    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "moved" }] });
    store.getState().setSalvaged(true);

    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
        "the main as stored",
      ),
    );
    // Still settled once another render debounce has passed.
    await new Promise((r) => setTimeout(r, 700));
    expect(screen.queryByRole("button", { name: "Apply" })).toBeNull();
    expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
      "the main as stored",
    );
    expect(store.getState().sourceBuffer?.text).toBe("a repair of the main");
  });
});

describe("the buffer across Save As", () => {
  function SaveAs({ store }: { store: ReturnType<typeof openStore> }) {
    const controller = useDocumentSaveAs();
    return (
      <>
        <button onClick={() => controller.requestSaveAs({ store })}>Open Save As</button>
        <DocumentSaveAsDialog controller={controller} />
      </>
    );
  }

  async function saveAsTo(store: ReturnType<typeof openStore>, path: string) {
    api.saveFile.mockResolvedValueOnce({ path, source: RENDERED });
    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));
    await waitFor(() => expect(store.getState().currentFilePath).toBe(path));
    // Past the view's 500 ms render debounce: a text that was NOT adopted
    // back is replaced on screen by the rendered file only once it fires.
    await new Promise((r) => setTimeout(r, 700));
  }

  it("comes back after a SECOND Save As, once the view has already adopted", async () => {
    const store = openStore();
    render(
      <DocumentStoreProvider store={store}>
        <SourceView />
        <SaveAs store={store} />
      </DocumentStoreProvider>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), {
      target: { value: "typed before Save As" },
    });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));

    await saveAsTo(store, "bots/copy.bot");
    expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
      "typed before Save As",
    );
    await saveAsTo(store, "bots/copy2.bot");

    expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
      "typed before Save As",
    );
    expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(store.getState().sourceBuffer).toMatchObject({
      path: "bots/copy2.bot",
      text: "typed before Save As",
    });
  });

  it("comes back after Save As when the pane was hidden and shown first", async () => {
    const store = openStore();
    const app = (pane: boolean) => (
      <DocumentStoreProvider store={store}>
        {pane ? <SourceView /> : null}
        <SaveAs store={store} />
      </DocumentStoreProvider>
    );
    const view = render(app(true));
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), {
      target: { value: "typed, pane hidden once" },
    });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));
    view.rerender(app(false));
    view.rerender(app(true));
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
        "typed, pane hidden once",
      ),
    );

    await saveAsTo(store, "bots/copy.bot");

    expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
      "typed, pane hidden once",
    );
    expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  });

  it("comes back under the new name, still dirty, still in edit mode", async () => {
    api.saveFile.mockResolvedValue({ path: "bots/copy.bot", source: RENDERED });
    const store = openStore();
    render(
      <DocumentStoreProvider store={store}>
        <SourceView />
        <SaveAs store={store} />
      </DocumentStoreProvider>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), {
      target: { value: "typed before Save As" },
    });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));

    fireEvent.click(screen.getByRole("button", { name: "Open Save As" }));
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));
    await waitFor(() => expect(store.getState().currentFilePath).toBe("bots/copy.bot"));

    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
        "typed before Save As",
      ),
    );
    expect(store.getState().sourceBuffer).toMatchObject({
      path: "bots/copy.bot",
      rel: null,
      text: "typed before Save As",
    });
    expect(store.getState().isSourceDirty()).toBe(true);
    expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy();
  });
});

describe("closing a tab that holds unsaved CANVAS work", () => {
  it("asks, though the Source pane holds nothing", async () => {
    const tabStore = getOrCreateDocumentStore("tab-canvas");
    tabStore.getState().setDocument(createEmptyDocument());
    tabStore.getState().setCurrentFilePath("bots/demo/main.bot");
    tabStore.getState().markSaved();
    tabStore.getState().addAgent({
      name: "a",
      model: "m",
      input: "in",
      output: "out",
      system: "s",
      user: "u",
      session: "fresh",
    });
    expect(tabStore.getState().isSourceDirty()).toBe(false);
    expect(tabStore.getState().hasUnsavedWork()).toBe(true);

    let dropped = false;
    function Probe() {
      const { guardDroppingEditorTab, dialog } = useDropEditorTab();
      return (
        <>
          <button onClick={() => void guardDroppingEditorTab(() => { dropped = true; }, "tab-canvas")}>
            drop
          </button>
          {dialog}
        </>
      );
    }
    render(<Probe />);
    fireEvent.click(screen.getByRole("button", { name: "drop" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    expect(dropped).toBe(false);
  });
});

// `applyParsedSource` (lib/salvage.ts) sets the document AND `salvaged`
// WITHOUT `setCurrentFilePath` — so a salvage can appear under an open
// per-file edit through a path that does not drop the buffer on the way in
// (an assistant proposal, the Toolbar's apply). The view turns read-only and
// is forced out of edit mode; what must NOT follow is the text going with
// it. The forced-exit effect releases only a CLEAN buffer, which is the
// chokepoint this asserts.
describe("a salvage appearing under an open per-file edit", () => {
  it("forces the mode closed and keeps the typed text visible to every discard path", async () => {
    const store = openStore();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    store.getState().setCurrentSource("the main as stored");
    mount(store);
    await screen.findByTestId("source-view-file-picker");
    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "lib/nodes.bot" },
    });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Edit" })).not.toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), {
      target: { value: "a repair the author typed" },
    });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));

    // The shape of applyParsedSource: a new document and the salvage flag,
    // and no path change.
    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "moved" }] });
    store.getState().setSalvaged(true);

    // Read-only now, showing the main as stored — which the view renders only
    // after leaving edit mode, once the effect that could release the text
    // has already run. Asserting sooner passes before that pass, whatever it
    // would do.
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(
        "the main as stored",
      ),
    );
    expect(screen.queryByRole("button", { name: "Apply" })).toBeNull();
    expect(store.getState().isSourceDirty()).toBe(true);
    expect(store.getState().hasUnsavedWork()).toBe(true);
    expect(store.getState().sourceBuffer?.text).toBe("a repair the author typed");
  });
});

// The ticket's measured case, in its cheapest form: a bot in ONE file,
// reopened on the SAME path by the watcher's reload or the assistant's
// reload-after-write. `setCurrentFilePath` clears the unit, the selection
// and the buffer even for an identical path, so `bufferKey` is
// `<path>\0!whole` before AND after — the FILE axis never flips, there is
// no transient to wait out, and Edit is offered over the text the previous
// document produced. Jo's decision (D45) keeps that window open and LABELS
// it; the Apply refusal it announces is #1227's and is unchanged.
describe("the stale-content window after a reload on the same file", () => {
  const RELOADED = "workflow w:\n  entry: a\n  a -> done\n";
  // The sentences as the operator hears them, spelled out rather than
  // imported from the view: this pins the COPY, and a test reading the
  // constant would pass any rewording of it.
  const STALE_HINT =
    "The document changed after this text was rendered from it, so applying this text is refused. An open editor keeps it until you apply or cancel; otherwise a fresh render replaces it.";
  const HELD_TEXT_HINT = "Answer the held text above first — Restore or Discard";

  function reload(store: ReturnType<typeof openStore>) {
    act(() => {
      applyOpenedFile(
        {
          source: RELOADED,
          document: { ...createEmptyDocument(), comments: [{ text: "a colleague's edit" }] },
          diagnostics: [],
          path: "bots/demo/main.bot",
        },
        store.getState(),
      );
    });
    // The key did not move: this is the content axis alone.
    expect(store.getState().currentFilePath).toBe("bots/demo/main.bot");
    expect(store.getState().unit).toBeNull();
    return store.getState().document;
  }

  it("labels it, keeps Edit offered, and still refuses the Apply", async () => {
    const store = openStore();
    api.parseSource.mockResolvedValue({
      document: createEmptyDocument(),
      diagnostics: [],
      bindable: true,
    });
    mount(store);
    // The render landed, so the text on screen IS the document's: nothing to
    // label yet. Asserting this first is what keeps the badge from being a
    // permanent decoration the next assertion would also pass with.
    await screen.findByRole("button", { name: "Edit" });
    expect(screen.queryByTestId("source-view-stale-badge")).toBeNull();

    const reloaded = reload(store);

    // Synchronous from here: the re-render that would close the window is
    // 500 ms away on a timer, and no assertion below yields to it.
    const badge = screen.getByTestId("source-view-stale-badge");
    expect(badge.textContent).toBe("Text may be stale");
    // jsdom has no layout engine and the browser harness is an advisory
    // cron, so the class is the assertion: a Badge is `whitespace-nowrap`,
    // and a nowrap child that yields narrows its box without narrowing its
    // text, which then paints under the picker's `relative` wrapper.
    expect(badge.className.split(/\s+/)).toContain("shrink-0");
    // Option B, not A: the window is named, not closed — Edit is still
    // offered, which putting the document into `bufferKey` would have taken
    // away on every canvas keystroke too.
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
    // And the HINT — the sentence, not the three-word label — reaches
    // assistive tech without touching the NAME, which seven e2e sites assert
    // is exactly "Edit": it arrives as the button's accessible DESCRIPTION.
    // Both are the real algorithms here — dom-accessibility-api computes
    // them, they are not read off attributes. `title` carries the same
    // sentence for the pointer, and nothing else: it is unreachable by
    // keyboard and by touch, so it cannot be the only channel.
    expect(
      screen.getByRole("button", { name: "Edit", description: STALE_HINT }),
    ).toBeTruthy();

    // The badge IS that refusal, read from the same `appliedFrom`: entering
    // the mode freezes this text (the render effect returns early), and the
    // Apply the badge warns about is the one `moved()` stops.
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    expect(
      screen.getByRole("button", { name: "Apply", description: STALE_HINT }),
    ).toBeTruthy();
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "typed over stale text" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));

    // The forbidden alternative, named: a badge announcing a refusal that
    // does not fire — the text written back over the reloaded document.
    expect(await screen.findByText(/Nothing was applied/)).toBeTruthy();
    expect(store.getState().document).toBe(reloaded);
    // Still labelled while the refusal stands.
    expect(screen.getByTestId("source-view-stale-badge")).toBeTruthy();
  });

  it("drops the label when the fresh render lands, so it is a window and not a verdict", async () => {
    const store = openStore();
    mount(store);
    await screen.findByRole("button", { name: "Edit" });

    api.unparse.mockResolvedValue({ source: RELOADED });
    reload(store);
    expect(screen.getByTestId("source-view-stale-badge")).toBeTruthy();

    // The re-render carries the reloaded document, so the text and its
    // provenance agree again. Asserting the TEXT too: the badge going away
    // because the window closed, not because the view stopped rendering.
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(RELOADED),
    );
    expect(screen.queryByTestId("source-view-stale-badge")).toBeNull();
    expect(
      screen.getByRole("button", { name: "Edit" }).getAttribute("aria-describedby"),
    ).toBeNull();
  });

  // Two reasons on one control. `aria-describedby` REPLACES `title` in the
  // description algorithm, so a reference that carried the window alone
  // would take the held-text reason — the reason the button is DISABLED —
  // off the only channel assistive tech has for it.
  it("says both reasons when the held text and this window overlap", async () => {
    const store = openStore();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: "lib/nodes.bot",
      text: "typed for the fragment",
      base: "the fragment as rendered",
      doc: null,
      session: 1,
    });
    // The fragment leaves the bot: its text is held, no picker can reach it,
    // and Edit stands down until the banner is answered (#1735).
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r2",
      files: [{ rel: "main.bot" }],
    });
    mount(store);

    const edit = await screen.findByRole("button", { name: "Edit" }, { timeout: 2000 });
    expect((edit as HTMLButtonElement).disabled).toBe(true);
    // The per-file render has to have LANDED, or there is no provenance to
    // move and `appliedFrom` falls back to the document as it is.
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).not.toBe(""),
    );
    expect(
      screen.getByRole("button", { name: "Edit", description: HELD_TEXT_HINT }),
    ).toBeTruthy();
    expect(screen.queryByTestId("source-view-stale-badge")).toBeNull();

    // Now the canvas moves under that text: the window opens over a button
    // that is already disabled for another reason.
    act(() => {
      store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "canvas" }] });
    });

    expect(screen.getByTestId("source-view-stale-badge")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Edit" }) as HTMLButtonElement).disabled).toBe(true);
    // Both sentences, the disabled reason first — the accname algorithm
    // concatenates the referenced nodes in the order of the idrefs.
    expect(
      screen.getByRole("button", {
        name: "Edit",
        description: `${HELD_TEXT_HINT} ${STALE_HINT}`,
      }),
    ).toBeTruthy();
    // The sighted hover says the same sentences in the same order.
    expect(screen.getByRole("button", { name: "Edit" }).getAttribute("title")).toBe(
      `${HELD_TEXT_HINT} — ${STALE_HINT}`,
    );
  });

  // The badge's two gates, each on its own. Both prevent a label over a
  // header that offers no control the label is about — the Apply refusal it
  // announces cannot even be reached there.
  it("stays silent on the merged program, which offers no Edit to warn about", async () => {
    const store = openStore();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    // The read-only entry: an explicit choice the selection effect keeps.
    store.getState().setSourceFile(MERGED);
    mount(store);

    // The merged render landed, and the group holds the note instead of the
    // controls — `editable` is false here.
    await screen.findByTestId("source-view-unit-note");
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(RENDERED),
    );
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Apply" })).toBeNull();

    // The canvas moves under the merged text: the provenance moved, so the
    // document term of the predicate holds.
    act(() => {
      store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "canvas" }] });
    });

    // Synchronous: the re-render is 500 ms away, so this reads the instant
    // the label would be wrong.
    expect(screen.queryByTestId("source-view-stale-badge")).toBeNull();
  });

  it("stays silent while the render is for another file, where there is no Edit either", async () => {
    const store = openStore();
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    mount(store);
    await screen.findByRole("button", { name: "Edit" });
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).not.toBe(""),
    );

    // One act: the canvas moves AND the picker moves to the other file. The
    // text on screen is now the PREVIOUS file's, which is the FILE axis —
    // `stale` — and it has its own answer, no Edit and no Apply.
    act(() => {
      store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "canvas" }] });
      store.getState().setSourceFile("lib/nodes.bot");
    });

    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Apply" })).toBeNull();
    expect(screen.queryByTestId("source-view-stale-badge")).toBeNull();
  });
});
