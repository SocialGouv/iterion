// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { ComputeDecl, IterDocument } from "@/api/types";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";
import {
  DocumentStoreProvider,
  createDocumentStore,
  type DocumentStore,
} from "@/store/document";
import ComputeForm from "./ComputeForm";

// Every expression row rendered its own bare "Remove": with two entries the
// form announced two identical buttons and neither named the output field
// whose expression it deletes (RGAA 11.1, repeated controls). The visible
// text stays "Remove"; the entry key travels in the aria-label, falling
// back to the 1-based row number while the key is still empty.

setupMatchMedia();

afterEach(cleanup);

function seed(computes: ComputeDecl[]): IterDocument {
  return {
    prompts: [],
    schemas: [],
    agents: [],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes,
    workflows: [],
    comments: [],
  };
}

function mountCompute(decl: ComputeDecl) {
  const store: DocumentStore = createDocumentStore();
  store.getState().setDocument(seed([decl]));
  const { container } = render(
    <DocumentStoreProvider store={store}>
      <ComputeForm decl={decl} />
    </DocumentStoreProvider>,
  );
  return container;
}

const DECL: ComputeDecl = {
  name: "calc",
  output: "out",
  expr: [
    { key: "streak", expr: "input.n >= 1" },
    { key: "total", expr: "input.a + input.b" },
  ],
};

describe("ComputeForm expression actions", () => {
  it("names each entry's Remove button after the key it deletes (RGAA 11.1)", () => {
    mountCompute(DECL);
    for (const key of ["streak", "total"]) {
      const btn = screen.getByRole("button", { name: `Remove entry ${key}` });
      // The accessible name qualifies; the visible text is untouched.
      expect(btn.textContent).toBe("Remove");
    }
  });
});

describe("ComputeForm axe", () => {
  it("Compute · ComputeForm — per-entry Remove names, axe smoke", async () => {
    const container = mountCompute(DECL);
    await expectNoViolationsStrict(container, "ComputeForm");
  });
});
