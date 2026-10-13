// @vitest-environment jsdom
//
// /runs/new?file=X reads the ACTIVE editor tab's store, which may hold
// another file. The launch is about X: X's source goes to the launch, and the
// tab keeps its own text — what its salvage view shows as the file.
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { useEffect } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  openFile: vi.fn(async (path: string) => ({
    source: `source of ${path}\n`,
    // One var named after the file, so a form field betrays which document
    // the hook is answering with.
    document: {
      workflows: [],
      comments: [],
      vars: { fields: [{ name: `var_of_${path}`, type: "string" as const }] },
    },
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

// Every (file, source, vars) triple the hook handed its caller, render by
// render: the launch reads the source it is given as the program and the
// document's vars as the form, so a triple naming one file with another's
// text is a run of the wrong workflow, and one naming it with another's
// vars sends that workflow the wrong inputs.
const seen: { file: string; source: string | null; vars: string[] }[] = [];

function Launch({ file }: { file: string }) {
  const { currentSource, doc, fields } = useLaunchDoc(file, () => {});
  const vars = fields.map((f) => f.name).join(",");
  useEffect(() => {
    seen.push({ file, source: currentSource, vars: vars ? vars.split(",") : [] });
  }, [file, currentSource, vars]);
  return (
    <>
      <output aria-label="launched">{currentSource ?? ""}</output>
      <output aria-label="form">{doc ? vars : "NO DOC"}</output>
    </>
  );
}
const form = () => screen.getByLabelText("form").textContent;
const launched = () => screen.getByLabelText("launched").textContent;

beforeEach(() => {
  seen.length = 0;
});

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

// This hook outlives the launch it was mounted for: /runs/new keeps it
// mounted while the URL moves from one workflow to another.
describe("a launch the view has moved on from", () => {
  it("never hands its source to the launch that replaced it", async () => {
    const { rerender } = render(
      <LaunchDocStoreProvider>
        <Launch file="bots/x.bot" />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(launched()).toBe("source of bots/x.bot\n"));

    rerender(
      <LaunchDocStoreProvider>
        <Launch file="bots/z.bot" />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(launched()).toBe("source of bots/z.bot\n"));

    expect(seen).not.toContainEqual({
      file: "bots/z.bot",
      source: "source of bots/x.bot\n",
      vars: ["var_of_bots/x.bot"],
    });
    expect(seen).toContainEqual({
      file: "bots/z.bot",
      source: "source of bots/z.bot\n",
      vars: ["var_of_bots/z.bot"],
    });
  });

  // The form is the other half of what a launch sends: the var names come
  // from the document, the values are keyed by them, and the required-var
  // gate decides whether Launch may be pressed at all. The document the
  // view has moved on from must no more be the form than its text is the
  // program — the hook answers with nothing until this file's own arrives.
  it("never describes the launch that replaced it with the previous one's form", async () => {
    const { rerender } = render(
      <LaunchDocStoreProvider>
        <Launch file="bots/x.bot" />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(form()).toBe("var_of_bots/x.bot"));

    rerender(
      <LaunchDocStoreProvider>
        <Launch file="bots/z.bot" />
      </LaunchDocStoreProvider>,
    );
    expect(form()).toBe("NO DOC");
    await waitFor(() => expect(form()).toBe("var_of_bots/z.bot"));

    expect(seen.filter((r) => r.file === "bots/z.bot")).not.toContainEqual(
      expect.objectContaining({ vars: ["var_of_bots/x.bot"] }),
    );
  });
});
