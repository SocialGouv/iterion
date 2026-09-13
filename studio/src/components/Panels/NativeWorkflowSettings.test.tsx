// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import type { IterDocument, WorkflowDecl } from "@/api/types";
import { createDocumentStore, DocumentStoreProvider, useDocumentStore } from "@/store/document";
import NativeWorkflowSettings from "./NativeWorkflowSettings";

afterEach(cleanup);

it("offers valid suppliers and preserves the named binding after an editor action", () => {
  const workflow: WorkflowDecl = { name: "main", entry: "", edges: [], runtime_semantics: "ports-v1", contract: "Root",
    graph: { nodes: [
      { name: "produce", implementation: "impl", contract: "Step" },
      { name: "consume", implementation: "impl", contract: "Step" },
    ], bindings: [{ from: "produce.text", to: "consume.item" }] },
  };
  const document = { workflows: [workflow], contracts: [
    { name: "Root", display_name: "Root", responsibility: "Supply a value", inputs: [
      { name: "optional", type: "string", required: false },
      { name: "nullable", type: "string", nullable: true },
      { name: "valid", type: "string" },
    ], outputs: [{ name: "result", type: "string" }] },
    { name: "Step", display_name: "Step", responsibility: "Transform a value",
      inputs: [{ name: "item", type: "string" }], outputs: [{ name: "text", type: "string" }] },
  ] } as IterDocument;
  const store = createDocumentStore();
  store.getState().setDocument(document);
  function Editor() {
    const active = useDocumentStore(state => state.document?.workflows?.[0]);
    return active ? <NativeWorkflowSettings workflow={active} /> : null;
  }
  render(<DocumentStoreProvider store={store}><Editor /></DocumentStoreProvider>);
  const sources = screen.getByLabelText("Source output") as HTMLSelectElement;
  expect(Array.from(sources.options, option => option.value)).toEqual(["input.valid"]);
  const exports = screen.getByLabelText("Produced by") as HTMLSelectElement;
  expect(Array.from(exports.options, option => option.value)).toEqual(["input.valid", "produce.text", "consume.text"]);
  fireEvent.click(screen.getByRole("button", { name: "Bind ports" }));
  expect(store.getState().document?.workflows?.[0]?.graph?.bindings).toEqual([
    { from: "produce.text", to: "consume.item" }, { from: "input.valid", to: "produce.item" },
  ]);
  expect((screen.getByRole("button", { name: "Bind ports" }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByText(/Already supplied by input.valid/)).toBeTruthy();
});
