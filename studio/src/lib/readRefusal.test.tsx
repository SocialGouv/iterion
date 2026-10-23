// @vitest-environment jsdom
//
// A bot the SERVER's read budget cut short — its main parses — is no file to
// repair: every surface that refuses it names the budget and the variable
// that moves it, the Diagnostics panel keeps the refusal with that remedy,
// and auto-validation does not replace it with the salvage's "not found"
// errors. Fed the server's own answers (__fixtures__: the open of such a
// bot, /api/parse of a text past the budget, /api/validate of the salvage),
// dumped from pkg/server's fixture of TestEveryOpenSaysWhyItIsASalvage.
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  validate: vi.fn(),
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
  default: ({ value, options }: { value?: string; options?: { readOnly?: boolean } }) => (
    <textarea aria-label="source" data-readonly={String(!!options?.readOnly)} value={value ?? ""} readOnly />
  ),
}));
vi.mock("@/lib/iterLanguage", () => ({ ITER_LANGUAGE_ID: "iter", iterLanguageConfig: {}, iterTokensProvider: {} }));
vi.mock("@/lib/iterMonacoCompletion", () => ({ registerIterCompletionProvider: vi.fn() }));

import opened from "./__fixtures__/read-refusal-open.json";
import parsedPastTheBudget from "./__fixtures__/read-refusal-parse.json";
import validated from "./__fixtures__/read-refusal-validate.json";
import type { IterDocument, ReadRefusal, UnitInfo } from "@/api/types";
import { applyOpenedFile } from "@/lib/openedFile";
import { applyParsedSource, ReadRefusalError, salvageRefusal } from "@/lib/salvage";
import { launchRefusal } from "@/lib/launch";
import { useAutoValidation } from "@/hooks/useAutoValidation";
import DiagnosticsPanel from "@/components/Diagnostics/DiagnosticsPanel";
import SourceView from "@/components/SourceView/SourceView";
import { DocumentStoreProvider, createDocumentStore } from "@/store/document";

const ENV = "ITERION_DSL_READ_BUDGET";

function openedStore() {
  const store = createDocumentStore();
  applyOpenedFile(
    {
      ...opened,
      document: opened.document as unknown as IterDocument,
      unit: opened.unit as unknown as UnitInfo,
      read_refusal: opened.read_refusal as ReadRefusal,
    },
    store.getState(),
  );
  return store;
}

function Validator() {
  useAutoValidation();
  return null;
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.resetAllMocks();
});

describe("a bot the server's read budget cut short", () => {
  it("is refused everywhere for the budget, with the variable that moves it", () => {
    const s = openedStore().getState();
    expect(s.salvaged).toBe(true);
    expect(s.readRefusal?.hint).toContain(ENV);
    for (const said of [salvageRefusal(s), launchRefusal(s)]) {
      expect(said).toContain("past the read budget of 60");
      expect(said).toContain(ENV);
      expect(said).not.toContain("did not parse");
    }
  });

  it("is shown read-only in the Source view, the budget and its remedy named", async () => {
    const store = openedStore();
    api.unparseUnitFile.mockResolvedValue({ source: "" });
    render(
      <DocumentStoreProvider store={store}>
        <SourceView />
      </DocumentStoreProvider>,
    );
    const note = await screen.findByTestId("source-view-read-refusal-note");
    expect(note.textContent).toContain(ENV);
    expect(note.textContent).not.toContain("did not parse");
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  });

  it("keeps the refusal in the panel, with its remedy, past auto-validation", async () => {
    vi.useFakeTimers();
    api.validate.mockResolvedValue({ diagnostics: validated.diagnostics, warnings: [], issues: validated.issues });
    const store = openedStore();
    const atOpen = [...store.getState().diagnostics];
    render(
      <DocumentStoreProvider store={store}>
        <Validator />
        <DiagnosticsPanel />
      </DocumentStoreProvider>,
    );
    await act(async () => vi.advanceTimersByTime(1600));
    await act(async () => {});
    expect(api.validate).not.toHaveBeenCalled();
    expect(store.getState().diagnostics).toEqual(atOpen);
    expect(store.getState().diagnostics.join("\n")).toContain("past the read budget of 60");
    expect(document.body.textContent).toContain(ENV);
    expect(document.body.textContent).not.toContain("not found");
  });

  it("is read-only in the Source view when it is one file past the budget alone", async () => {
    const store = createDocumentStore();
    applyOpenedFile(
      {
        source: "prompt big:\n  x\n",
        document: parsedPastTheBudget.document as unknown as IterDocument,
        diagnostics: parsedPastTheBudget.diagnostics,
        path: "bots/big.bot",
        bindable: parsedPastTheBudget.bindable,
        read_refusal: parsedPastTheBudget.read_refusal as ReadRefusal,
      },
      store.getState(),
    );
    api.unparse.mockResolvedValue({ source: "" });
    render(
      <DocumentStoreProvider store={store}>
        <SourceView />
      </DocumentStoreProvider>,
    );
    const note = await screen.findByTestId("source-view-read-refusal-note");
    expect(note.textContent).toContain(ENV);
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  });

  it("is never applied over the document a tab holds", () => {
    const store = createDocumentStore();
    const before = store.getState().document;
    expect(() =>
      applyParsedSource(
        {
          ...parsedPastTheBudget,
          document: parsedPastTheBudget.document as unknown as IterDocument,
          read_refusal: parsedPastTheBudget.read_refusal as ReadRefusal,
        },
        store.getState(),
      ),
    ).toThrow(ReadRefusalError);
    expect(store.getState().document).toBe(before);
    expect(store.getState().salvaged).toBe(false);
  });
});
