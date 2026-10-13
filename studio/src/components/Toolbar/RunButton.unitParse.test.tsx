// @vitest-environment jsdom
//
// The probe of #1532, against the REAL auto-validation hook and the REAL
// gate: a bot in several files whose fragment does not parse opens
// `bindable: true` — the MAIN parsed, so no salvage refuses it — with the
// fragment's parse errors as diagnostics. Auto-validation then validates
// the MERGED document, built from what parsed, which the server answers
// clean, and REWRITES those diagnostics. The gate's fact has to be one the
// rewrite cannot reach: the unit's per-file parse verdict.
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const setLocation = vi.fn();
vi.mock("wouter", () => ({ useLocation: () => ["/editor", setLocation] }));
vi.mock("@/api/client", () => ({ validate: vi.fn() }));

import * as client from "@/api/client";
import type { BackendDetectReport } from "@/api/backends";
import { useAutoValidation } from "@/hooks/useAutoValidation";
import { createEmptyDocument } from "@/lib/defaults";
import { applyOpenedFile } from "@/lib/openedFile";
import { useBackendDetectStore } from "@/store/backendDetect";
import { DocumentStoreProvider, createDocumentStore, type DocumentStore } from "@/store/document";

import RunButton from "./RunButton";

// The shape POST /api/files/open answers for `bots/feature-dev/main.bot`
// with a `model` line missing its colon in `lib/nodes.bot`: the main
// parses, the fragment does not, and the unit says which file and how
// many errors.
const firstError = "lib/nodes.bot:202:9: error [E002]: expected :, got String";

function openedUnitWithBrokenFragment() {
  return {
    source: 'import "lib/nodes.bot"\n',
    document: createEmptyDocument(),
    diagnostics: [firstError, "lib/nodes.bot:202:54: error [E002]: expected string literal, got Newline"],
    path: "bots/feature-dev/main.bot",
    unit: {
      root: "bots/feature-dev",
      main: "main.bot",
      revision: "r1",
      files: [
        { rel: "main.bot" },
        { rel: "lib/nodes.bot", parse_errors: 2, parse_error: firstError },
      ],
    },
    bindable: true,
  };
}

function Editor() {
  useAutoValidation();
  return <RunButton />;
}

function mount(store: DocumentStore) {
  return render(
    <DocumentStoreProvider store={store}>
      <Editor />
    </DocumentStoreProvider>,
  );
}

const run = () => screen.getByRole("button", { name: "Run" }) as HTMLButtonElement;

beforeEach(() => {
  vi.useFakeTimers();
  setLocation.mockClear();
  vi.mocked(client.validate).mockReset();
  useBackendDetectStore.setState({
    report: { resolved_default: "anthropic" } as unknown as BackendDetectReport,
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("the Run gate on a bot in several files", () => {
  it("stays closed when auto-validation answers clean on the merged document, and names the file", async () => {
    const store = createDocumentStore();
    let answer!: (v: unknown) => void;
    vi.mocked(client.validate).mockReturnValue(new Promise((r) => (answer = r)) as never);
    applyOpenedFile(openedUnitWithBrokenFragment(), store.getState());
    mount(store);

    // Where the gate starts: the open answered the fragment's parse errors
    // both as diagnostics and as the unit's per-file verdict.
    expect(run().disabled).toBe(true);
    expect(store.getState().diagnostics).toHaveLength(2);

    // The rewrite, through the real hook: the merged document validates
    // clean, warnings only.
    await act(async () => vi.advanceTimersByTime(1500));
    expect(client.validate).toHaveBeenCalledTimes(1);
    await act(async () => answer({ diagnostics: [], warnings: ["C101 something"], issues: [] }));
    expect(store.getState().diagnostics).toEqual([]);

    // The gate holds, on the unit's own verdict, and says which file.
    expect(run().disabled).toBe(true);
    expect(run().title).toMatch(/lib\/nodes\.bot/);
    expect(run().title).toContain(firstError);
  });

  it("opens once every file of the unit parses", async () => {
    const store = createDocumentStore();
    let answer!: (v: unknown) => void;
    vi.mocked(client.validate).mockReturnValue(new Promise((r) => (answer = r)) as never);
    const opened = openedUnitWithBrokenFragment();
    opened.diagnostics = [];
    opened.unit.files = [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }];
    applyOpenedFile(opened, store.getState());
    mount(store);

    await act(async () => vi.advanceTimersByTime(1500));
    await act(async () => answer({ diagnostics: [], warnings: [], issues: [] }));
    expect(run().disabled).toBe(false);
    expect(run().title).toBe("Launch bots/feature-dev/main.bot");
  });
});
