// @vitest-environment jsdom
//
// The launch route reads the ACTIVE EDITOR TAB's document store — the one
// place the buffer Run was clicked on lives. Mounted bare (as the route was,
// before the bridge), useLaunchDoc always read the module-default store,
// which no authoring path ever writes: the Run button enabled on the tab's
// buffer, the view answering "no workflow to launch" (#1326). The
// falsification of this file is the same assertions rendered WITHOUT the
// provider — every one of them goes red (verified during the adversarial
// round before the bridge existed).
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  unparse: vi.fn(),
  openFile: vi.fn(),
  loadExample: vi.fn(),
  parseSource: vi.fn(),
  parseBotSourceEditorPath: () => null,
}));
vi.mock("@/api/client", () => api);
vi.mock("wouter", () => ({ useLocation: () => ["/", vi.fn()] }));

import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";

import LaunchDocStoreProvider from "./LaunchDocStoreProvider";
import { useLaunchDoc } from "./useLaunchDoc";

function Probe() {
  // Inline on purpose: the load effect writes the store, so an onError
  // whose identity changes per render would re-run it per render — an
  // unparse-per-render loop. The hook must be immune to the caller's
  // callback identity.
  const { noSource, doc } = useLaunchDoc("", () => {});
  return <div>{noSource ? "NOSOURCE" : doc ? "DOC" : "LOADING"}</div>;
}

function mount() {
  return render(
    <LaunchDocStoreProvider>
      <Probe />
    </LaunchDocStoreProvider>,
  );
}

beforeEach(() => {
  api.unparse.mockReset().mockResolvedValue("PARSED SOURCE");
  useTabsStore.setState({ tabs: [], activeEditorTabId: null, activeRunTabId: null });
});

afterEach(cleanup);

describe("the launch route's document store", () => {
  it("is the active tab's: an edited unbound buffer in it is a launch candidate", async () => {
    const tabId = useTabsStore.getState().openTab("editor", {}, "draft");
    useTabsStore.getState().setActive(tabId);
    getOrCreateDocumentStore(tabId).getState().setDocument(createEmptyDocument());

    mount();

    await waitFor(() => expect(screen.getByText("DOC")).toBeTruthy());
    // Settles: exactly one load, not an unparse-per-render loop.
    await new Promise((r) => setTimeout(r, 30));
    expect(api.unparse).toHaveBeenCalledTimes(1);
  });

  it("is the active tab's: its pristine scaffold still reads as no source", async () => {
    const tabId = useTabsStore.getState().openTab("editor", {}, "empty");
    useTabsStore.getState().setActive(tabId);

    mount();

    await waitFor(() => expect(screen.getByText("NOSOURCE")).toBeTruthy());
    expect(api.unparse).not.toHaveBeenCalled();
  });

  it("falls back honest when no tab is active: a bare deep link has no buffer", async () => {
    mount();

    await waitFor(() => expect(screen.getByText("NOSOURCE")).toBeTruthy());
    expect(api.unparse).not.toHaveBeenCalled();
  });
});
