// @vitest-environment jsdom
//
// The two measured residuals of #1662, from the outside: the Source view's
// buffer was component-local, so (1) no discard prompt anywhere could see an
// un-applied edit, and (2) leaving edit mode destroyed the typed text with no
// prompt and no undo — measured on the documented way out of a salvage, where
// the watcher's reload discovers the bot is in several files.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ parseSource: vi.fn(), unparse: vi.fn() }));
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
import {
  DocumentStoreProvider,
  createDocumentStore,
  getOrCreateDocumentStore,
} from "@/store/document";

import SourceView from "./SourceView";
import { useDropEditorTab } from "@/hooks/useDropEditorTab";
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

    // A buffer whose path is not this tab's file is another tab's.
    store.setState({
      sourceBuffer: { ...store.getState().sourceBuffer!, path: "bots/other/main.bot" },
    });
    mount(store);
    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(RENDERED),
    );
  });
});

// Round 4's own findings: keeping the buffer across an unmount created a
// second class — a buffer that survives but is never reached, and an exit
// from edit mode that deleted it under the author's eyes.
describe("the buffer when the view turns read-only under it", () => {
  it("survives being forced out of edit mode", async () => {
    const store = openStore();
    const buffer = await startEditing(store);
    fireEvent.change(buffer, { target: { value: "a repair the author typed" } });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));

    // A unit arrives under the open edit — the watcher's reload discovering
    // the bot is in several files, which is the documented way out of a
    // salvage. The view turns read-only; the work must not go with it.
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });

    await waitFor(() => expect(screen.queryByRole("button", { name: "Apply" })).toBeNull());
    expect(store.getState().isSourceDirty()).toBe(true);
    expect(store.getState().hasUnsavedWork()).toBe(true);
  });

  it("lets go of work no picker can reach again, and says so", async () => {
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
    });
    // The fragment is deleted from the bot. Nothing can select it again, so
    // the buffer could never be adopted — and holding it would stop this tab
    // auto-reloading for ever and light `beforeunload` with nothing to show.
    store.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r2",
      files: [{ rel: "main.bot" }],
    });
    mount(store);

    await waitFor(() => expect(store.getState().sourceBuffer).toBeNull());
    expect(useUIStore.getState().toasts.map((t) => t.message).join(" ")).toContain(
      "lib/nodes.bot is no longer one of this bot's files",
    );
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
