// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { Edge, IterDocument } from "@/api/types";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";
import {
  DocumentStoreProvider,
  createDocumentStore,
  type DocumentStore,
} from "@/store/document";
import EdgeForm from "./EdgeForm";

// The edge form rendered five buttons named by bare "+ Add" / "Remove"
// across its three sections: a screen-reader user heard "plus, button"
// three times and could not tell the when-clause Add from the loop Add or
// the data-mapping Add (RGAA 11.1, repeated controls). Each now carries its
// section in the accessible name; the mapping row's cross names the key it
// removes. Visible text is untouched.

setupMatchMedia();

afterEach(cleanup);

function seed(edge: Edge): IterDocument {
  return {
    prompts: [],
    schemas: [],
    agents: [],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [{ name: "main", entry: "a", edges: [edge] }],
    comments: [],
  };
}

function mountEdge(edge: Edge) {
  const store: DocumentStore = createDocumentStore();
  store.getState().setDocument(seed(edge));
  const { container } = render(
    <DocumentStoreProvider store={store}>
      <EdgeForm edge={edge} edgeIndex={0} workflowName="main" />
    </DocumentStoreProvider>,
  );
  return container;
}

const PLAIN: Edge = { from: "a", to: "b" };

const POPULATED: Edge = {
  from: "a",
  to: "b",
  when: { condition: "", negated: false },
  loop: { name: "", max_iterations: 3 },
  with: [{ key: "row_key", value: "" }],
};

describe("EdgeForm section actions", () => {
  it("names each section's Add button after the clause it creates (RGAA 11.1)", () => {
    // With no when and no loop the two clause sections render their Add;
    // the mapping section always does.
    mountEdge(PLAIN);
    expect(screen.getByRole("button", { name: "Add when condition" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add loop" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add with entry" })).toBeTruthy();
  });

  it("names each clause Remove button after the clause it clears (RGAA 11.1)", () => {
    mountEdge(POPULATED);
    expect(screen.getByRole("button", { name: "Remove when clause" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove loop" })).toBeTruthy();
  });

  it("names a mapping row's cross after the key it removes (RGAA 11.1)", () => {
    mountEdge(POPULATED);
    const btn = screen.getByRole("button", { name: "Remove mapping row_key" });
    // An unnamed key would strand the announced action between sibling
    // rows; the key is what the row is.
    expect(btn.getAttribute("aria-label")).toBe("Remove mapping row_key");
  });
});

describe("EdgeForm axe", () => {
  it("Edge · EdgeForm — section and per-row names, axe smoke", async () => {
    // Populated: both clauses open (their Remove visible) and one mapping
    // row — the form's full button set.
    const container = mountEdge(POPULATED);
    await expectNoViolationsStrict(container, "EdgeForm");
  });
});
