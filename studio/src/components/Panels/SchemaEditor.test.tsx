// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { IterDocument, SchemaDecl } from "@/api/types";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";
import {
  DocumentStoreProvider,
  createDocumentStore,
  type DocumentStore,
} from "@/store/document";
import SchemaEditor from "./SchemaEditor";

// One "Delete" per schema card said only that: N sibling cards announced N
// identical buttons and none named the schema it would remove (RGAA 11.1,
// repeated controls). The field-row "x" buttons were already qualified
// (`Remove field ${name || i + 1}`) — the cards now follow them, keeping the
// visible "Delete" text and carrying the schema name in the aria-label.

setupMatchMedia();

afterEach(cleanup);

function seed(schemas: SchemaDecl[]): IterDocument {
  return {
    prompts: [],
    schemas,
    agents: [],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [],
    comments: [],
  };
}

function mountSchemas(schemas: SchemaDecl[]) {
  const store: DocumentStore = createDocumentStore();
  store.getState().setDocument(seed(schemas));
  const { container } = render(
    <DocumentStoreProvider store={store}>
      <SchemaEditor />
    </DocumentStoreProvider>,
  );
  return container;
}

describe("SchemaEditor card actions", () => {
  it("names each card's Delete button after the schema it deletes (RGAA 11.1)", () => {
    mountSchemas([
      { name: "s_one", fields: [{ name: "alpha", type: "string" }] },
      { name: "s_two", fields: [{ name: "beta", type: "int" }] },
    ]);
    for (const name of ["s_one", "s_two"]) {
      const btn = screen.getByRole("button", { name: `Delete schema ${name}` });
      // The accessible name qualifies; the visible text is untouched.
      expect(btn.textContent).toBe("Delete");
    }
    // The per-row removal keeps its own qualification, spelled after the
    // same pattern the card delete now follows.
    expect(screen.getByRole("button", { name: "Remove field alpha" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove field beta" })).toBeTruthy();
  });
});

describe("SchemaEditor axe", () => {
  it("Schemas · SchemaEditor — card Delete names, axe smoke", async () => {
    const container = mountSchemas([
      { name: "s_one", fields: [{ name: "alpha", type: "string" }] },
      { name: "s_two", fields: [{ name: "beta", type: "int" }] },
    ]);
    await expectNoViolationsStrict(container, "SchemaEditor");
  });
});
