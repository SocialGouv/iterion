// @vitest-environment jsdom
//
// The inline launch is the sixth site that hands the document out as the
// program, and the harshest: it loses no bytes, it RUNS a workflow the
// author never wrote — the program minus the region the parser could not
// read — at real cost, with nothing in the run's report to say so.
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  unparse: vi.fn(),
  openFile: vi.fn(),
  getWorktreeConfig: vi.fn(),
}));
vi.mock("@/api/client", () => api);

import { createEmptyDocument } from "@/lib/defaults";
import { DocumentStoreProvider, createDocumentStore } from "@/store/document";

import { useLaunchDoc } from "./useLaunchDoc";

function wrapperFor(store: ReturnType<typeof createDocumentStore>) {
  return function Wrapper({ children }: { children: React.ReactNode }) {
    return <DocumentStoreProvider store={store}>{children}</DocumentStoreProvider>;
  };
}

// A buffer that counts as a launch candidate: edited past the saved mark,
// with no ?file= path — the inline-source launch, and the only one cloud has.
function launchableStore(salvaged: boolean) {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setSalvaged(salvaged);
  return store;
}

beforeEach(() => {
  vi.resetAllMocks();
  api.unparse.mockResolvedValue("workflow x:\n  entry: done\n");
});

afterEach(cleanup);

describe("the inline launch of an editor buffer", () => {
  it("refuses to run a document the parser only salvaged", async () => {
    const onError = vi.fn();
    const store = launchableStore(true);

    renderHook(() => useLaunchDoc("", onError), { wrapper: wrapperFor(store) });

    await waitFor(() => expect(onError).toHaveBeenCalled());
    expect(String(onError.mock.calls[0]?.[0])).toMatch(/did not parse/i);
    // Never even rendered: the source the launch would carry is not built.
    expect(api.unparse).not.toHaveBeenCalled();
  });

  it("runs one that is not a salvage", async () => {
    const onError = vi.fn();
    const store = launchableStore(false);

    renderHook(() => useLaunchDoc("", onError), { wrapper: wrapperFor(store) });

    await waitFor(() => expect(api.unparse).toHaveBeenCalled());
    expect(onError).not.toHaveBeenCalled();
  });

  // The same rule as the toolbar's Run button, read from the same predicate:
  // a document the validator refused would fail at the server with the same
  // errors, after the form was filled in.
  it("refuses a document with error diagnostics, naming their count", async () => {
    const onError = vi.fn();
    const store = launchableStore(false);
    store.getState().setDiagnostics(["e1", "e2"], []);

    renderHook(() => useLaunchDoc("", onError), { wrapper: wrapperFor(store) });

    await waitFor(() => expect(onError).toHaveBeenCalled());
    expect(String(onError.mock.calls[0]?.[0])).toMatch(/fix the 2 errors/i);
    expect(api.unparse).not.toHaveBeenCalled();
  });

  // A buffer nothing was put in — a fresh tab, File → New, Start blank — is
  // not a launch candidate: the view shows the picker's empty state instead
  // of offering the scaffold as an "unsaved workflow".
  it("reports no source for a pristine buffer, and neither errors nor unparses", async () => {
    const onError = vi.fn();
    const store = createDocumentStore();

    const { result } = renderHook(() => useLaunchDoc("", onError), { wrapper: wrapperFor(store) });

    expect(result.current.noSource).toBe(true);
    await new Promise((r) => setTimeout(r, 10));
    expect(onError).not.toHaveBeenCalled();
    expect(api.unparse).not.toHaveBeenCalled();
  });

  // The store keeps living while the form is up (the launch route reads the
  // active tab's store): a refusal arriving after the form mounted must
  // clear it — a document the gate refuses must not stay launchable under
  // the error banner.
  it("clears a mounted form when the buffer gains a refusal", async () => {
    const onError = vi.fn();
    const store = launchableStore(false);

    const { result } = renderHook(() => useLaunchDoc("", onError), { wrapper: wrapperFor(store) });

    await waitFor(() => expect(result.current.doc).not.toBeNull());
    expect(onError).not.toHaveBeenCalled();

    store.getState().setDiagnostics(["e1"]);
    await waitFor(() => expect(onError).toHaveBeenCalled());
    expect(String(onError.mock.calls[0]?.[0])).toMatch(/fix the 1 error/i);
    expect(result.current.doc).toBeNull();
  });
});
