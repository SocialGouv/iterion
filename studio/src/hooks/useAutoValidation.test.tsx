// @vitest-environment jsdom
//
// Automatic validation answers after a 1.5 s debounce and a round trip. Its
// diagnostics describe the document it sent, of the file it sent it for:
// they are published only while both still hold, and never once the editor
// that asked has gone away.
import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/api/client", () => ({ validate: vi.fn() }));

import * as client from "@/api/client";
import { createEmptyDocument } from "@/lib/defaults";
import { DocumentStoreProvider, createDocumentStore } from "@/store/document";
import { useAutoValidation } from "./useAutoValidation";

function Validator() {
  useAutoValidation();
  return null;
}

function setup() {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/a.bot");
  store.getState().markSaved();
  let answer!: (v: unknown) => void;
  vi.mocked(client.validate).mockReturnValue(new Promise((r) => (answer = r)) as never);
  const view = render(
    <DocumentStoreProvider store={store}>
      <Validator />
    </DocumentStoreProvider>,
  );
  return { store, view, answer: (diagnostics: string[]) => answer({ diagnostics, warnings: [], issues: [] }) };
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.mocked(client.validate).mockReset();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("automatic validation's answer", () => {
  it("is published for the document it validated (the control)", async () => {
    const { store, answer } = setup();
    await act(async () => vi.advanceTimersByTime(1500));
    expect(client.validate).toHaveBeenCalledTimes(1);
    await act(async () => answer(["C001 real"]));
    expect(store.getState().diagnostics).toEqual(["C001 real"]);
  });

  it("is not published once the document moved, even before the effect re-runs", async () => {
    const { store, answer } = setup();
    await act(async () => vi.advanceTimersByTime(1500));
    // The gap: the store moved and the component has not re-run its effect
    // (nothing it subscribes to changed), so no abort happened.
    store.setState({ _generation: store.getState()._generation + 1 });
    await act(async () => answer(["C001 stale"]));
    expect(store.getState().diagnostics).toEqual([]);
  });

  it("is not published once the editor that asked has unmounted", async () => {
    const { store, view, answer } = setup();
    await act(async () => vi.advanceTimersByTime(1500));
    view.unmount();
    await act(async () => answer(["C001 orphaned"]));
    expect(store.getState().diagnostics).toEqual([]);
  });
});
