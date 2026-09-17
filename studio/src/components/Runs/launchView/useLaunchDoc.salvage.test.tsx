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
});
