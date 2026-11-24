// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { IterDocument } from "@/api/types";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";
import {
  DocumentStoreProvider,
  createDocumentStore,
  type DocumentStore,
} from "@/store/document";
import VarsEditor from "./VarsEditor";

// The bool row's visible "Default" caption is a bare sibling label: the
// select it names had NO accessible name at all (RGAA 11.1). The aria-label
// carries the variable so sibling rows — and the same caption on the
// workflow section's rows — don't collide; a nameless row falls back to its
// 1-based position, after the row's own `Remove variable` pattern. The two
// section "+ Add" buttons were equally indistinguishable; each now names its
// section.

setupMatchMedia();

afterEach(cleanup);

function seedVars(): IterDocument {
  return {
    vars: {
      fields: [{ name: "verbose", type: "bool" }, { name: "", type: "bool" }],
    },
    workflows: [
      {
        name: "main",
        entry: "agent_1",
        edges: [],
        vars: { fields: [{ name: "dry_run", type: "bool" }] },
      },
    ],
    prompts: [],
    schemas: [],
    agents: [],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    comments: [],
  };
}

function mountVars() {
  const store: DocumentStore = createDocumentStore();
  store.getState().setDocument(seedVars());
  const { container } = render(
    <main>
      <DocumentStoreProvider store={store}>
        <VarsEditor />
      </DocumentStoreProvider>
    </main>,
  );
  return container;
}

describe("VarsEditor control names", () => {
  it("names the bool Default select after its variable (RGAA 11.1)", () => {
    mountVars();
    const def = screen.getByLabelText("Default for variable verbose") as HTMLSelectElement;
    expect(def.tagName).toBe("SELECT");
    // The same caption on a workflow-section row gets its own name — not one
    // control found twice.
    const workflowDef = screen.getByLabelText("Default for variable dry_run") as HTMLSelectElement;
    expect(workflowDef.tagName).toBe("SELECT");
    expect(workflowDef).not.toBe(def);
    // A nameless row falls back to its 1-based position.
    expect(screen.getByLabelText("Default for variable 2")).toBeTruthy();
  });

  it("names each section's Add button after that section (RGAA 11.1)", () => {
    mountVars();
    // The accessible name qualifies the section; the visible "+ Add" is untouched.
    expect(screen.getByRole("button", { name: "Add variable" }).textContent).toBe("+ Add");
    expect(screen.getByRole("button", { name: "Add workflow variable" }).textContent).toBe("+ Add");
  });
});

describe("VarsEditor axe", () => {
  it("Variables · VarsEditor — Default select and section Add names, axe smoke", async () => {
    const container = mountVars();
    await expectNoViolationsStrict(container, "VarsEditor");
  });
});
