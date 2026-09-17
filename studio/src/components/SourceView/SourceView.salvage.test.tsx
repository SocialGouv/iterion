// @vitest-environment jsdom
//
// The Source view is the way OUT of a salvage: the refusal on the three
// write sites points here, so what it shows and what Apply does decide
// whether an author can recover a file the parser could not read whole.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ parseSource: vi.fn(), unparse: vi.fn() }));
vi.mock("@/api/client", () => api);
// Monaco does not run under jsdom; the editor is a textarea here, which is
// enough to read what the view SHOWS and to drive Apply.
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

const FILE_TEXT = "workflow y:\n  entry: done\n\nagent broken\n  the line the author must fix\n";

function salvagedStore() {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/y/main.bot");
  store.getState().setCurrentSource(FILE_TEXT);
  store.getState().setSalvaged(true);
  store.getState().markSaved();
  return store;
}

beforeEach(() => {
  vi.resetAllMocks();
  api.unparse.mockResolvedValue("workflow y:\n  entry: done\n");
});

afterEach(cleanup);

describe("the Source view of a salvaged file", () => {
  // Rendering the document back would show a text the author's own file does
  // not contain, with the broken lines silently gone — they would "repair"
  // something already amputated.
  it("shows the file's own text, not the document rendered back", async () => {
    const store = salvagedStore();
    render(
      <DocumentStoreProvider store={store}>
        <SourceView />
      </DocumentStoreProvider>,
    );

    await waitFor(() =>
      expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(FILE_TEXT),
    );
    // Never rendered back from the document: that is the text the author's
    // file does not contain.
    expect(api.unparse).not.toHaveBeenCalled();
  });

  // And the exit: text that parses whole makes the document the program
  // again, so the write sites stop refusing.
  it("ends the salvage when the repaired text parses whole", async () => {
    const store = salvagedStore();
    api.parseSource.mockResolvedValue({
      document: createEmptyDocument(),
      diagnostics: [],
      bindable: true,
    });
    render(
      <DocumentStoreProvider store={store}>
        <SourceView />
      </DocumentStoreProvider>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), {
      target: { value: "workflow y:\n  entry: done\n" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));

    await waitFor(() => expect(store.getState().salvaged).toBe(false));
  });

  // A repair that still does not parse leaves the refusal in place — and
  // must leave the author's OWN text on screen. The buffer is still a
  // salvage, so the sync re-runs; without the applied text becoming the
  // buffer's own, it puts the text this view opened with back over what was
  // just typed: the loss, inside the way out of it.
  it("keeps the salvage but not the stale text when a repair does not parse yet", async () => {
    const store = salvagedStore();
    const HALF_REPAIRED = "workflow y:\n  entry: done\n\nagent broken\n  half fixed\n";
    api.parseSource.mockResolvedValue({
      document: createEmptyDocument(),
      diagnostics: ["y.bot:4:1: error [E012]: unknown property"],
      bindable: false,
    });
    render(
      <DocumentStoreProvider store={store}>
        <SourceView />
      </DocumentStoreProvider>,
    );

    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: HALF_REPAIRED } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));

    await waitFor(() => expect(store.getState().salvaged).toBe(true));
    // The applied text is the buffer's own now — this is what the sync reads
    // back, so asserting it is what makes the check bite.
    await waitFor(() => expect(store.getState().currentSource).toBe(HALF_REPAIRED));
    // And past the sync's debounce, the editor still shows it. Asserting the
    // textarea alone would pass before the sync ever ran.
    await new Promise((r) => setTimeout(r, 700));
    expect((screen.getByLabelText("source") as HTMLTextAreaElement).value).toBe(HALF_REPAIRED);
  });
});
