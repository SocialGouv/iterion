// @vitest-environment jsdom
//
// Apply sends the text of ONE edit session and awaits the parse; the editor
// stays writable meanwhile. What its answer may settle is that session and
// that text — nothing typed since, and nothing of a session that replaced
// it (#1770's rule, applied to the Source view's own answer).
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  parseSource: vi.fn(),
  unparse: vi.fn(),
  unparseUnitFile: vi.fn(),
  parseUnitFile: vi.fn(),
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
import { useUIStore } from "@/store/ui";
import SourceView from "./SourceView";

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

const parsed = (marker: string) => ({
  document: { ...createEmptyDocument(), comments: [{ text: marker }] },
  diagnostics: [],
});

function store(unit = false) {
  const s = createDocumentStore();
  s.getState().setDocument(createEmptyDocument());
  s.getState().setCurrentFilePath("bots/demo/main.bot");
  if (unit) {
    s.getState().setUnit({
      root: "bots/demo",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
  }
  s.getState().markSaved();
  return s;
}

function mount(s: ReturnType<typeof store>) {
  return render(
    <DocumentStoreProvider store={s}>
      <SourceView />
    </DocumentStoreProvider>,
  );
}

const textarea = () => screen.getByLabelText("source") as HTMLTextAreaElement;
const type = (text: string) => fireEvent.change(textarea(), { target: { value: text } });

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

describe("Apply, when the author types on while it parses", () => {
  it("keeps the newer text in edit mode, dirty against what was applied (whole file)", async () => {
    const s = store();
    mount(s);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    type("first");
    const parse = deferred<ReturnType<typeof parsed>>();
    api.parseSource.mockReturnValue(parse.promise);
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(api.parseSource).toHaveBeenCalledWith("first"));
    type("first and more");

    await act(async () => parse.resolve(parsed("applied")));
    expect(s.getState().document?.comments?.[0]?.text).toBe("applied");
    expect(textarea().value).toBe("first and more");
    expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy();
    const held = s.getState().sourceBuffer;
    expect(held).toMatchObject({ text: "first and more", base: "first" });
    expect(held?.doc).toBe(s.getState().document);
  });

  it("keeps the newer text in edit mode, dirty against what was applied (one file of a unit)", async () => {
    const s = store(true);
    mount(s);
    await screen.findByTestId("source-view-file-picker");
    fireEvent.change(screen.getByTestId("source-view-file-picker"), { target: { value: "lib/nodes.bot" } });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Edit" })).not.toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    type("nodes v1");
    const parse = deferred<ReturnType<typeof parsed>>();
    api.parseUnitFile.mockReturnValue(parse.promise);
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalledTimes(1));
    type("nodes v1 and more");

    await act(async () => parse.resolve(parsed("nodes applied")));
    expect(s.getState().document?.comments?.[0]?.text).toBe("nodes applied");
    expect(textarea().value).toBe("nodes v1 and more");
    expect(s.getState().sourceBuffer).toMatchObject({ text: "nodes v1 and more", base: "nodes v1", rel: "lib/nodes.bot" });
  });
});

describe("Apply, when its session was closed while it parsed", () => {
  it("applies nothing and leaves the session that replaced it alone", async () => {
    const s = store(true);
    mount(s);
    await screen.findByTestId("source-view-file-picker");
    fireEvent.change(screen.getByTestId("source-view-file-picker"), { target: { value: "lib/nodes.bot" } });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Edit" })).not.toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    type("for nodes");
    const parse = deferred<ReturnType<typeof parsed>>();
    api.parseUnitFile.mockReturnValue(parse.promise);
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(api.parseUnitFile).toHaveBeenCalledTimes(1));

    // Cancel (and discard), then edit the main.
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard" }));
    await waitFor(() => expect(s.getState().sourceBuffer).toBeNull());
    fireEvent.change(screen.getByTestId("source-view-file-picker"), { target: { value: "main.bot" } });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Edit" })).not.toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    type("for main");
    const mainSession = s.getState().sourceBuffer?.session;

    await act(async () => parse.resolve(parsed("nodes applied")));
    expect(s.getState().document?.comments?.[0]?.text).toBeUndefined();
    expect(s.getState().sourceBuffer).toMatchObject({ text: "for main", rel: "main.bot", session: mainSession });
    expect(screen.getByText("This edit was closed while it was applying. Nothing was applied.")).toBeTruthy();
  });
});

describe("Apply, when the pane was hidden while it parsed", () => {
  it("comes back on the applied base, and applies again without being refused as stale", async () => {
    const s = store();
    const view = mount(s);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    type("v1");
    const first = deferred<ReturnType<typeof parsed>>();
    api.parseSource.mockReturnValueOnce(first.promise);
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(api.parseSource).toHaveBeenCalledTimes(1));
    type("v1 and more");
    view.unmount();

    await act(async () => first.resolve(parsed("v1 applied")));
    expect(s.getState().sourceBuffer).toMatchObject({ text: "v1 and more", base: "v1" });

    mount(s);
    await waitFor(() => expect(textarea().value).toBe("v1 and more"));
    api.parseSource.mockResolvedValueOnce(parsed("v2 applied"));
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(s.getState().document?.comments?.[0]?.text).toBe("v2 applied"));
    expect(screen.queryByText(/The editor changed while this was applying/)).toBeNull();
  });
});

describe("Apply, when the pane was hidden and shown again WHILE it parsed", () => {
  // Two instances of the view share one stored edit: the one that asked
  // settles the STORE, and the one on screen has to follow it.
  async function applyThenReshow(s: ReturnType<typeof store>) {
    const first = mount(s);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    type("v1");
    const parse = deferred<ReturnType<typeof parsed>>();
    api.parseSource.mockReturnValueOnce(parse.promise);
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(api.parseSource).toHaveBeenCalledTimes(1));
    first.unmount();
    mount(s);
    await waitFor(() => expect(textarea().value).toBe("v1"));
    return parse;
  }

  it("follows the stored edit out of edit mode, and the next Apply is accepted", async () => {
    const s = store();
    const parse = await applyThenReshow(s);
    await act(async () => parse.resolve(parsed("v1 applied")));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Apply" })).toBeNull());
    expect(s.getState().sourceBuffer).toBeNull();

    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    type("v1 then v2");
    api.parseSource.mockResolvedValueOnce(parsed("v2 applied"));
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(s.getState().document?.comments?.[0]?.text).toBe("v2 applied"));
    expect(screen.queryByText(/The editor changed while this was applying/)).toBeNull();
  });

  it("takes the applied base and provenance when typing went on, and the next Apply is accepted", async () => {
    const s = store();
    const parse = await applyThenReshow(s);
    type("v1 and more");
    await act(async () => parse.resolve(parsed("v1 applied")));
    expect(s.getState().sourceBuffer).toMatchObject({ text: "v1 and more", base: "v1" });
    expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy();

    api.parseSource.mockResolvedValueOnce(parsed("v2 applied"));
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(s.getState().document?.comments?.[0]?.text).toBe("v2 applied"));
    expect(screen.queryByText(/The editor changed while this was applying/)).toBeNull();
  });
});
