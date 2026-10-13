// @vitest-environment jsdom
//
// The repair of a bot in several files that does not LOAD, against the REAL
// watcher and the REAL gate. The gate refuses on the unit's load verdict
// (#1532), and the repair of the commonest load failure — an `import` naming
// a fragment that is not on disk — is to write that fragment. It is a file of
// NO list: the loader stops at the failed read, so the unit holds every file
// it reached and nothing of the one it did not. The watcher recognises it by
// the `unread` list the answer carries beside the verdict, or the refusal
// stands over a fragment that now exists and only reopening the tab clears it.
//
// The same road backwards: a fragment DELETED from under the unit leaves the
// gate open on a launch the server refuses, so the deletion is read again too.
import { cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ openFile: vi.fn() }));
vi.mock("@/api/client", () => api);

// A set of handlers, as the real client keeps: every mounted tab listens.
const ws = vi.hoisted(() => {
  const handlers = new Set<(e: unknown) => void>();
  return {
    fileWatcher: {
      connect: vi.fn(),
      disconnect: vi.fn(),
      subscribe: vi.fn((fn: (e: unknown) => void) => {
        handlers.add(fn);
        return () => {
          handlers.delete(fn);
        };
      }),
    },
    emit: (e: unknown) => {
      for (const h of [...handlers]) h(e);
    },
  };
});
vi.mock("@/api/ws", () => ({ fileWatcher: ws.fileWatcher }));

import { createEmptyDocument } from "@/lib/defaults";
import { launchRefusal } from "@/lib/launch";
import { applyOpenedFile, type OpenedFile } from "@/lib/openedFile";
import { DocumentStoreProvider, createDocumentStore, type DocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";

import { useFileWatcher } from "./useFileWatcher";

const loadError = 'main.bot:1:1: error [E046]: import "lib/missing.bot": no such fragment (lib/missing.bot)';
const deletedError = 'main.bot:2:1: error [E046]: import "lib/nodes.bot": no such fragment (lib/nodes.bot)';

// What `/api/files/open` answers for a main importing a fragment that is not
// on disk: the main parses (`bindable`), every file the load REACHED parses,
// and the load verdict is the only fact left — naming a file the list does
// not hold, which `unread` names instead
// (pkg/server TestAUnitsRootAndMainComposeIntoThePathThatWasOpened).
function openedUnitThatDoesNotLoad() {
  return {
    source: 'import "lib/missing.bot"\nimport "lib/nodes.bot"\n',
    document: createEmptyDocument(),
    diagnostics: [loadError],
    path: "bots/demo/main.bot",
    unit: {
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot", imports: ["lib/missing.bot", "lib/nodes.bot"] }, { rel: "lib/nodes.bot" }],
      unread: ["lib/missing.bot"],
      load_errors: 1,
      load_error: loadError,
    },
    bindable: true,
  };
}

// The same bot once the fragment is there: it loads, and nothing refuses.
function openedUnitThatLoads() {
  return {
    source: 'import "lib/missing.bot"\nimport "lib/nodes.bot"\n',
    document: createEmptyDocument(),
    diagnostics: [],
    path: "bots/demo/main.bot",
    unit: {
      root: "bots/demo",
      main: "main.bot",
      revision: "r2",
      files: [
        { rel: "main.bot", imports: ["lib/missing.bot", "lib/nodes.bot"] },
        { rel: "lib/missing.bot" },
        { rel: "lib/nodes.bot" },
      ],
    },
    bindable: true,
  };
}

// And once a fragment it held is deleted: the import names a fragment that is
// no longer there, so the load refuses what the launch refuses.
function openedUnitWithTheFragmentGone() {
  return {
    source: 'import "lib/nodes.bot"\n',
    document: createEmptyDocument(),
    diagnostics: [deletedError],
    path: "bots/demo/main.bot",
    unit: {
      root: "bots/demo",
      main: "main.bot",
      revision: "r3",
      files: [{ rel: "main.bot", imports: ["lib/nodes.bot"] }],
      unread: ["lib/nodes.bot"],
      load_errors: 1,
      load_error: deletedError,
    },
    bindable: true,
  };
}

function Watcher() {
  useFileWatcher();
  return null;
}

function openOn(opened: OpenedFile): DocumentStore {
  const store = createDocumentStore();
  applyOpenedFile(opened, store.getState());
  render(
    <DocumentStoreProvider store={store}>
      <Watcher />
    </DocumentStoreProvider>,
  );
  return store;
}

const refusal = (store: DocumentStore) => launchRefusal(store.getState());

// Longer than the watcher's 500 ms debounce.
const RELOAD_WINDOW_MS = 900;

beforeEach(() => {
  vi.resetAllMocks();
  useUIStore.setState({ toasts: [] });
});

afterEach(cleanup);

describe("the watcher against a unit that does not load", () => {
  it("reads the main again when the fragment the refusal names is created, and the gate opens", async () => {
    const store = openOn(openedUnitThatDoesNotLoad());
    expect(refusal(store)).toContain("lib/missing.bot");

    api.openFile.mockResolvedValue(openedUnitThatLoads());
    // The author writes the fragment: a creation, which is what the server
    // broadcasts when the write lands as a rename (an editor's atomic save,
    // `git checkout`, `touch`).
    ws.emit({ type: "file_created", path: "bots/demo/lib/missing.bot" });

    await waitFor(() => expect(api.openFile).toHaveBeenCalledWith("bots/demo/main.bot"), { timeout: 2000 });
    await waitFor(() => expect(refusal(store)).toBeNull(), { timeout: 2000 });
  });

  it("reads it again when the same write arrives as a modification", async () => {
    const store = openOn(openedUnitThatDoesNotLoad());
    api.openFile.mockResolvedValue(openedUnitThatLoads());
    // The other flavour of the same write: the server's watcher keeps the
    // LAST event type of its 200 ms debounce, so a create-then-write lands
    // as a modification.
    ws.emit({ type: "file_modified", path: "bots/demo/lib/missing.bot" });

    await waitFor(() => expect(api.openFile).toHaveBeenCalledWith("bots/demo/main.bot"), { timeout: 2000 });
    await waitFor(() => expect(refusal(store)).toBeNull(), { timeout: 2000 });
  });

  it("leaves a file no import of the unit names alone, under its root or elsewhere", async () => {
    const store = openOn(openedUnitThatDoesNotLoad());
    api.openFile.mockResolvedValue(openedUnitThatLoads());
    // Under the unit's own root, and named by nothing: writing it repairs
    // nothing, and reading the main again on it would reload the tab on
    // every file written beside the bot.
    ws.emit({ type: "file_created", path: "bots/demo/lib/unused.bot" });
    // Another bot's fragment, of the same name as the one the refusal wants.
    ws.emit({ type: "file_created", path: "bots/other/lib/missing.bot" });
    // A deeper path the import's name is a prefix of.
    ws.emit({ type: "file_modified", path: "bots/demo/lib/missing.bot.bak" });

    await new Promise((r) => setTimeout(r, RELOAD_WINDOW_MS));
    expect(api.openFile).not.toHaveBeenCalled();
    expect(refusal(store)).toContain("lib/missing.bot");
  });

  it("reads the main again when a fragment it HOLDS reappears", async () => {
    const store = openOn(openedUnitThatDoesNotLoad());
    api.openFile.mockResolvedValue(openedUnitThatLoads());
    // A fragment replaced on disk whole — `git checkout` of the bot's
    // directory — reaches the tab as a creation, and what the tab holds of
    // it is then not what the launch would read.
    ws.emit({ type: "file_created", path: "bots/demo/lib/nodes.bot" });

    await waitFor(() => expect(api.openFile).toHaveBeenCalledWith("bots/demo/main.bot"), { timeout: 2000 });
    expect(refusal(store)).toBeNull();
  });
});

describe("the watcher after a per-file apply that dropped the broken import", () => {
  it("offers the write of the fragment the refusal names, naming that file", async () => {
    const store = openOn(openedUnitThatDoesNotLoad());
    // The apply's answer installed the way SourceView does, and the tab
    // left as the apply leaves it: the merged document REPLACED, so the tab
    // holds work of the author's — the apply stored nothing, and no save
    // can store it while the stored files do not load; the files of the
    // STAGED unit, whose header no longer imports the fragment, over the
    // verdict of the STORED one, which still refuses on it
    // (pkg/server TestApplyingOneFileAnswersTheFragmentsTheStoredLoadAwaits);
    // and the staged unit's own diagnostics, which are clean.
    const held = store.getState().unit!;
    store.getState().setDocument(createEmptyDocument());
    store.getState().setUnit({
      ...held,
      files: [{ rel: "main.bot", imports: ["lib/nodes.bot"] }, { rel: "lib/nodes.bot" }],
      unread: ["lib/missing.bot"],
      load_errors: 1,
      load_error: loadError,
    });
    store.getState().setDiagnostics([]);
    expect(store.getState().isDirty()).toBe(true);
    // The load verdict is then the ONLY thing left refusing the launch.
    expect(refusal(store)).toContain("lib/missing.bot");

    // The author writes it on disk, which is where the refusal sends them:
    // a save of a bot whose stored files do not load is refused.
    api.openFile.mockResolvedValue(openedUnitThatLoads());
    ws.emit({ type: "file_created", path: "bots/demo/lib/missing.bot" });

    // Their work is not taken from them: the files on disk are not the ones
    // the tab read any more, and the offer says so — naming the file that
    // CHANGED, not the one it would reload — while the refusal stands until
    // they take it.
    expect(useUIStore.getState().toasts.map((t) => `${t.message} [${t.action?.label ?? "-"}]`)).toEqual([
      "bots/demo/lib/missing.bot changed externally — it is one of this bot's files [Reload and discard]",
    ]);
    await new Promise((r) => setTimeout(r, RELOAD_WINDOW_MS));
    expect(api.openFile).not.toHaveBeenCalled();
    expect(refusal(store)).toContain("lib/missing.bot");
  });

  it("awaits nothing once the apply answers a unit that loads", async () => {
    const store = openOn(openedUnitThatDoesNotLoad());
    // The tab is left SAVED here, where the case above leaves it dirty: an
    // auto-reload would then fire, so `openFile` never being called is this
    // unit awaiting nothing and not the gate on the author's work.
    const held = store.getState().unit!;
    // An apply answered after the fragment was written: the stored unit
    // loads, so it awaits nothing — and `unread` is dropped with the
    // verdict, not kept from the answer before it.
    store.getState().setUnit({
      ...held,
      files: openedUnitThatLoads().unit.files,
      unread: undefined,
      load_errors: undefined,
      load_error: undefined,
    });
    // The apply writes the diagnostics of its own answer too.
    store.getState().setDiagnostics([]);
    expect(refusal(store)).toBeNull();

    api.openFile.mockResolvedValue(openedUnitThatLoads());
    ws.emit({ type: "file_created", path: "bots/demo/lib/other.bot" });

    await new Promise((r) => setTimeout(r, RELOAD_WINDOW_MS));
    expect(api.openFile).not.toHaveBeenCalled();
  });
});

// The load failure the loader refuses by PATH: an `import` resolving outside
// the bot's `lib/` directory. The file it names is in no list either, and the
// main's `imports` names it — but the loader never read it, so no write there
// lifts the refusal, and such a directory is exactly where another bot's main
// lives (pkg/server TestAnImportTheLoaderRefusesByPathIsAwaitedByNothing).
describe("the watcher against an import the loader refuses by path", () => {
  const outsideLib =
    "main.bot:1:1: error [E045]: import \"other.bot\" resolves to other.bot, outside the bot's `lib/` directory where every fragment lives";
  const openedUnitImportingOutsideLib = () => ({
    source: 'import "other.bot"\n',
    document: createEmptyDocument(),
    diagnostics: [outsideLib],
    path: "bots/demo/main.bot",
    unit: {
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot", imports: ["other.bot"] }],
      load_errors: 1,
      load_error: outsideLib,
    },
    bindable: true,
  });

  it("leaves the neighbour it names alone, written or deleted", async () => {
    const store = openOn(openedUnitImportingOutsideLib() as OpenedFile);
    expect(refusal(store)).toContain("outside the bot's `lib/` directory");

    api.openFile.mockResolvedValue(openedUnitImportingOutsideLib());
    ws.emit({ type: "file_modified", path: "bots/demo/other.bot" });
    ws.emit({ type: "file_deleted", path: "bots/demo/other.bot" });

    await new Promise((r) => setTimeout(r, RELOAD_WINDOW_MS));
    // Reading the main again would reload this tab on every save of that
    // neighbour, forever: the refusal is about the path.
    expect(api.openFile).not.toHaveBeenCalled();
    // And its deletion is not this bot losing a file.
    expect(useUIStore.getState().toasts.map((t) => t.message)).toEqual([]);
    expect(refusal(store)).toContain("outside the bot's `lib/` directory");
  });
});

describe("the watcher against a fragment deleted from under the unit", () => {
  it("reads the main again, and the gate closes on the load the launch refuses", async () => {
    const store = openOn(openedUnitThatLoads());
    expect(refusal(store)).toBeNull();

    api.openFile.mockResolvedValue(openedUnitWithTheFragmentGone());
    ws.emit({ type: "file_deleted", path: "bots/demo/lib/nodes.bot" });

    await waitFor(() => expect(api.openFile).toHaveBeenCalledWith("bots/demo/main.bot"), { timeout: 2000 });
    await waitFor(() => expect(refusal(store)).toContain("lib/nodes.bot"), { timeout: 2000 });
    // What the author is shown, whole: the warning that named the file is
    // still there — the reload says what the files are now, the warning says
    // what happened to them — and the reload says it took them.
    expect(useUIStore.getState().toasts.map((t) => t.message)).toEqual([
      "bots/demo/lib/nodes.bot was deleted externally — it is one of this bot's files",
      "File reloaded",
    ]);
  });

  it("does not read the open file again when it is the one deleted", async () => {
    openOn(openedUnitThatLoads());
    ws.emit({ type: "file_deleted", path: "bots/demo/main.bot" });

    await new Promise((r) => setTimeout(r, RELOAD_WINDOW_MS));
    // There is nothing to read: the request would 404 and the error toast
    // would say "failed to reload" over a file the author deleted.
    expect(api.openFile).not.toHaveBeenCalled();
    expect(useUIStore.getState().toasts.map((t) => t.message)).toContain("bots/demo/main.bot was deleted externally");
  });

  it("says the reload failed, and keeps the warning, when the whole bot went away", async () => {
    openOn(openedUnitThatLoads());
    // `rm -rf` of the bot's directory: the main's own deletion is one
    // event, a fragment's another, and the read the fragment's asks for
    // finds nothing. The failure is shown rather than swallowed — the
    // author is told twice over which files went, which beats a reload
    // that silently did not happen.
    api.openFile.mockRejectedValue(new Error("file not found"));
    ws.emit({ type: "file_deleted", path: "bots/demo/main.bot" });
    ws.emit({ type: "file_deleted", path: "bots/demo/lib/nodes.bot" });

    await waitFor(
      () =>
        expect(useUIStore.getState().toasts.map((t) => t.message)).toEqual([
          "bots/demo/main.bot was deleted externally",
          "bots/demo/lib/nodes.bot was deleted externally — it is one of this bot's files",
          "Failed to reload main.bot: file not found",
        ]),
      { timeout: 2000 },
    );
  });

  it("keeps an open edit, and offers the reload instead of taking it", async () => {
    const store = openOn(openedUnitThatLoads());
    store.getState().setSourceBuffer({
      path: "bots/demo/main.bot",
      rel: null,
      text: "the author's text",
      base: "the author's text",
      doc: null,
      session: 1,
    });
    ws.emit({ type: "file_deleted", path: "bots/demo/lib/nodes.bot" });

    await new Promise((r) => setTimeout(r, RELOAD_WINDOW_MS));
    expect(api.openFile).not.toHaveBeenCalled();
    expect(useUIStore.getState().toasts.map((t) => t.message)).toContain(
      "bots/demo/lib/nodes.bot was deleted externally — it is one of this bot's files",
    );
  });
});

// What the offer SAYS, over a tab holding work of the author's. It reloads
// the OPEN file — a fragment's text and the revision ride the merged
// document, so that is the read the event asks for — and the sentence is
// about the file that CHANGED: "the open file changed externally" over a
// write to a fragment asks the author to discard their work on a statement
// about a file nobody touched.
describe("the offer raised for a write to one of the bot's other files", () => {
  it("names the file that changed, and the open file when that is the one", async () => {
    const store = openOn(openedUnitThatLoads());
    // Work of the author's, as an edit of the canvas leaves it.
    store.getState().setDocument(createEmptyDocument());
    expect(store.getState().isDirty()).toBe(true);

    ws.emit({ type: "file_modified", path: "bots/demo/lib/nodes.bot" });
    expect(useUIStore.getState().toasts.map((t) => `${t.message} [${t.action?.label ?? "-"}]`)).toEqual([
      "bots/demo/lib/nodes.bot changed externally — it is one of this bot's files [Reload and discard]",
    ]);
    expect(api.openFile).not.toHaveBeenCalled();

    // And the open file's own write says just that, as it always has.
    useUIStore.setState({ toasts: [] });
    ws.emit({ type: "file_modified", path: "bots/demo/main.bot" });
    expect(useUIStore.getState().toasts.map((t) => t.message)).toEqual(["bots/demo/main.bot changed externally"]);
  });
});
