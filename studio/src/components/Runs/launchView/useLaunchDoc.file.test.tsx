// @vitest-environment jsdom
//
// /runs/new?file=X reads the ACTIVE editor tab's store, which may hold
// another file. The launch is about X: X's source goes to the launch, and the
// tab keeps its own text — what its salvage view shows as the file.
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  openFile: vi.fn(async (path: string) => ({
    source: `source of ${path}\n`,
    document: { workflows: [], comments: [] },
    diagnostics: [],
    path,
  })),
  unparse: vi.fn(),
}));

import { createEmptyDocument } from "@/lib/defaults";
import { getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";
import LaunchDocStoreProvider from "./LaunchDocStoreProvider";
import { useLaunchDoc } from "./useLaunchDoc";

function Launch({ file }: { file: string }) {
  const { currentSource } = useLaunchDoc(file, () => {});
  return <output aria-label="launched">{currentSource ?? ""}</output>;
}
const launched = () => screen.getByLabelText("launched").textContent;

afterEach(cleanup);

describe("a launch of ?file=X while the active tab holds Y", () => {
  it("launches X's source and leaves Y's alone", async () => {
    const id = useTabsStore.getState().openTab("editor", { file: "bots/y.bot" }, "y");
    useTabsStore.getState().setActive(id);
    const store = getOrCreateDocumentStore(id);
    store.getState().setDocument(createEmptyDocument());
    store.getState().setCurrentFilePath("bots/y.bot");
    store.getState().setCurrentSource("source of bots/y.bot\n");
    store.getState().markSaved();
    render(
      <LaunchDocStoreProvider>
        <Launch file="bots/x.bot" />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(launched()).toBe("source of bots/x.bot\n"));
    expect(store.getState().currentFilePath).toBe("bots/y.bot");
    expect(store.getState().currentSource).toBe("source of bots/y.bot\n");
  });
});
