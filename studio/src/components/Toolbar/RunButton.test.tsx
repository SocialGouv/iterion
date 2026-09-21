// @vitest-environment jsdom
//
// The toolbar's Run is enabled exactly when the launch view would launch.
// It used to be enabled for any non-null document — and the store always
// holds one, the scaffold — so a fresh buffer led to an empty picker, and a
// salvage or a document with errors led to a launch the server refuses
// (#1326). The gate is `launchRefusal`, read here and by the launch view.
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const setLocation = vi.fn();
vi.mock("wouter", () => ({ useLocation: () => ["/editor", setLocation] }));

import type { BackendDetectReport } from "@/api/backends";
import { createEmptyDocument } from "@/lib/defaults";
import { useBackendDetectStore } from "@/store/backendDetect";
import { DocumentStoreProvider, createDocumentStore, type DocumentStore } from "@/store/document";

import RunButton from "./RunButton";

function mount(store: DocumentStore) {
  return render(
    <DocumentStoreProvider store={store}>
      <RunButton />
    </DocumentStoreProvider>,
  );
}

const run = () => screen.getByRole("button", { name: "Run" }) as HTMLButtonElement;
const nudge = () => screen.queryByRole("button", { name: /No LLM credential detected/ });

function credential(resolved: string | null) {
  useBackendDetectStore.setState({
    report:
      resolved === null
        ? null
        : ({ resolved_default: resolved } as unknown as BackendDetectReport),
  });
}

function boundClean(): DocumentStore {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/x/main.bot");
  store.getState().markSaved();
  return store;
}

beforeEach(() => {
  setLocation.mockClear();
  credential("anthropic");
});

afterEach(cleanup);

describe("the toolbar's Run", () => {
  it("is disabled for a fresh buffer, and says to write or open a workflow first", () => {
    mount(createDocumentStore());
    expect(run().disabled).toBe(true);
    expect(run().title).toMatch(/write or open a workflow first/i);
    fireEvent.click(run());
    expect(setLocation).not.toHaveBeenCalled();
  });

  it("is disabled for the buffer File → New leaves — unbound and saved", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath(null);
    store.getState().markSaved();
    mount(store);
    expect(run().disabled).toBe(true);
    expect(run().title).toMatch(/write or open a workflow first/i);
  });

  it("launches a bound file by path", () => {
    mount(boundClean());
    expect(run().disabled).toBe(false);
    expect(run().title).toBe("Launch bots/x/main.bot");
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new?file=bots%2Fx%2Fmain.bot");
  });

  it("launches an edited unbound buffer inline", () => {
    const store = createDocumentStore();
    store.getState().setDocument(createEmptyDocument());
    mount(store);
    expect(run().disabled).toBe(false);
    expect(run().title).toBe("Launch the unsaved workflow");
    fireEvent.click(run());
    expect(setLocation).toHaveBeenCalledWith("/runs/new");
  });

  it("is disabled for a salvage, naming the way out", () => {
    const store = boundClean();
    store.getState().setSalvaged(true);
    mount(store);
    expect(run().disabled).toBe(true);
    expect(run().title).toMatch(/did not parse/i);
  });

  it("is disabled for a document with error diagnostics, counting them", () => {
    const store = boundClean();
    store.getState().setDiagnostics(["e1", "e2"], ["w1"]);
    mount(store);
    expect(run().disabled).toBe(true);
    expect(run().title).toBe("Fix the 2 errors in the Diagnostics panel before launching");
  });

  it("stays enabled on warnings alone", () => {
    const store = boundClean();
    store.getState().setDiagnostics([], ["w1"]);
    mount(store);
    expect(run().disabled).toBe(false);
  });

  it("follows the store: a document that gains an error disables it, and loses it re-enables it", () => {
    const store = boundClean();
    mount(store);
    expect(run().disabled).toBe(false);
    act(() => store.getState().setDiagnostics(["e1"]));
    expect(run().disabled).toBe(true);
    act(() => store.getState().setDiagnostics([]));
    expect(run().disabled).toBe(false);
  });

  describe("without an LLM credential", () => {
    it("is disabled and nudges to Preferences → Backends when the credential is the only blocker", () => {
      credential("");
      mount(boundClean());
      expect(run().disabled).toBe(true);
      expect(run().title).toMatch(/No LLM credentials detected/);
      expect(nudge()).not.toBeNull();
    });

    it("names the document's own reason first, and shows no nudge for it", () => {
      credential("");
      mount(createDocumentStore());
      expect(run().disabled).toBe(true);
      expect(run().title).toMatch(/write or open a workflow first/i);
      expect(nudge()).toBeNull();
    });

    it("shows no nudge before the probe has answered", () => {
      credential(null);
      mount(boundClean());
      expect(run().disabled).toBe(true);
      expect(nudge()).toBeNull();
    });
  });
});
