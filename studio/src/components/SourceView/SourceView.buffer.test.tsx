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
import { DocumentStoreProvider, createDocumentStore } from "@/store/document";

import SourceView from "./SourceView";

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

afterEach(cleanup);

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

  it("holds nothing once the view is gone", async () => {
    const store = openStore();
    const buffer = await startEditing(store);
    fireEvent.change(buffer, { target: { value: "typed" } });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));
    cleanup();
    expect(store.getState().sourceBuffer).toBeNull();
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
