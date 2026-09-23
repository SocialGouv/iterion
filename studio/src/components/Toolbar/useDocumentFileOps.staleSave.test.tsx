// @vitest-environment jsdom
//
// A canvas Save's answer arrives after an await: whatever the tab did in the
// meantime — opened another file, changed its unit's files, took more edits —
// the answer is about the file it wrote, and it may only settle THAT.
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  parseSource: vi.fn(),
  unparse: vi.fn(),
  unparseUnitFile: vi.fn(),
  parseUnitFile: vi.fn(),
  saveFile: vi.fn(),
  openFile: vi.fn(),
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
import { DocumentStoreProvider, createDocumentStore } from "@/store/document";
import SourceView from "@/components/SourceView/SourceView";
import { useUIStore } from "@/store/ui";
import { useDocumentFileOps } from "./useDocumentFileOps";

function Toolbarish() {
  const ops = useDocumentFileOps({ confirm: async () => true });
  return (
    <>
      <button onClick={() => void ops.handleSave()}>Canvas Save</button>
      <button onClick={() => void ops.handlePickFile("file", "bots/b.bot")}>Open B</button>
    </>
  );
}

function unitStore(files: string[]) {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/u/main.bot");
  store.getState().setUnit({
    root: "bots/u",
    main: "main.bot",
    revision: "r1",
    files: files.map((rel) => ({ rel })),
  });
  store.getState().markSaved();
  return store;
}

function mount(store: ReturnType<typeof unitStore>) {
  render(
    <DocumentStoreProvider store={store}>
      <SourceView />
      <Toolbarish />
    </DocumentStoreProvider>,
  );
}

async function startSlowSave() {
  let resolve!: (v: unknown) => void;
  api.saveFile.mockReturnValueOnce(new Promise((r) => (resolve = r)));
  fireEvent.click(screen.getByRole("button", { name: "Canvas Save" }));
  await waitFor(() => expect(api.saveFile).toHaveBeenCalledTimes(1));
  return resolve;
}

const toasts = () => useUIStore.getState().toasts.map((t) => t.message);

beforeEach(() => {
  vi.resetAllMocks();
  api.unparse.mockResolvedValue({ source: "rendered\n" });
  api.unparseUnitFile.mockImplementation(async (_d: unknown, _p: string, rel: string) => ({
    source: `rendered ${rel}\n`,
  }));
});

afterEach(() => {
  cleanup();
  useUIStore.setState({ toasts: [] });
});

describe("a canvas Save that answers after the tab moved on", () => {
  it("lands nothing on another file the tab opened meanwhile", async () => {
    const store = unitStore(["main.bot", "lib/a.bot"]);
    mount(store);
    const landSave = await startSlowSave();

    api.openFile.mockResolvedValue({
      source: "B source\n",
      document: createEmptyDocument(),
      diagnostics: [],
      path: "bots/b.bot",
    });
    fireEvent.click(screen.getByRole("button", { name: "Open B" }));
    await waitFor(() => expect(store.getState().currentFilePath).toBe("bots/b.bot"));
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }, { timeout: 3000 }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "typed in B" } });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));

    await act(async () => landSave({ path: "bots/u/main.bot", source: "U source\n", revision: "r2" }));

    const s = store.getState();
    expect(s.currentFilePath).toBe("bots/b.bot");
    expect(s.unit).toBeNull();
    expect(s.currentSource).toBe("B source\n");
    expect(s.sourceBuffer?.text).toBe("typed in B");
    expect(toasts().join(" ")).not.toContain("discarded");
  });

  it("keeps a file a per-file Apply added to the unit while it was in flight", async () => {
    const store = unitStore(["main.bot", "lib/a.bot"]);
    mount(store);
    await screen.findByTestId("source-view-file-picker");
    const landSave = await startSlowSave();

    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: 'import "lib/c.bot"\n' } });
    api.parseUnitFile.mockResolvedValue({
      document: { ...createEmptyDocument(), comments: [{ text: "with c" }] },
      diagnostics: [],
      unit: {
        root: "bots/u",
        main: "main.bot",
        revision: "",
        files: [{ rel: "main.bot" }, { rel: "lib/a.bot" }, { rel: "lib/c.bot" }],
      },
    });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(store.getState().unit?.files.map((f) => f.rel)).toContain("lib/c.bot"),
    );
    fireEvent.change(screen.getByTestId("source-view-file-picker"), {
      target: { value: "lib/c.bot" },
    });
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }, { timeout: 3000 }));
    fireEvent.change(screen.getByLabelText("source"), { target: { value: "typed for lib/c.bot" } });
    await waitFor(() => expect(store.getState().isSourceDirty()).toBe(true));

    await act(async () => landSave({ path: "bots/u/main.bot", source: "x", revision: "r2" }));

    const s = store.getState();
    expect(s.unit?.files.map((f) => f.rel)).toEqual(["main.bot", "lib/a.bot", "lib/c.bot"]);
    expect(s.unit?.revision).toBe("r2");
    expect(s.sourceBuffer?.text).toBe("typed for lib/c.bot");
    expect(toasts().join(" ")).not.toContain("discarded");
  });

  it("does not mark saved the edits made while it was in flight", async () => {
    const store = unitStore(["main.bot", "lib/a.bot"]);
    mount(store);
    const landSave = await startSlowSave();

    act(() => {
      store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "edited during the save" }] });
    });
    await act(async () => landSave({ path: "bots/u/main.bot", source: "U source\n", revision: "r2" }));

    expect(store.getState().isDirty()).toBe(true);
    expect(toasts()).toContain("Saved, but newer editor changes remain unsaved");
  });
});
