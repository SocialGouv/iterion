// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { IterDocument, WorkflowDecl } from "@/api/types";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";
import {
  DocumentStoreProvider,
  createDocumentStore,
  type DocumentStore,
} from "@/store/document";
import WorkflowSettingsForm from "./WorkflowSettingsForm";

// Every resource row rendered a "Remove resource" cross: the label existed
// but was identical on each row, so a screen-reader user with two resources
// heard the same announcement twice and neither copy named the resource it
// deletes (RGAA 11.1, repeated controls). The row's resource name now rides
// in the aria-label; the visible ✕ is untouched. The permission suite pins
// the mechanism (IconButton chokepoint's aria-label) for one row; this file
// pins the qualification across rows.

setupMatchMedia();

afterEach(cleanup);

function seed(over: Partial<WorkflowDecl> = {}): IterDocument {
  return {
    prompts: [],
    schemas: [],
    agents: [],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [{ name: "main", entry: "start", edges: [], ...over }],
    comments: [],
  };
}

function mountSettings(over: Partial<WorkflowDecl> = {}) {
  const store: DocumentStore = createDocumentStore();
  store.getState().setDocument(seed(over));
  const { container } = render(
    <DocumentStoreProvider store={store}>
      <WorkflowSettingsForm />
    </DocumentStoreProvider>,
  );
  return container;
}

describe("WorkflowSettingsForm resource actions", () => {
  it("names each resource row's remove button after the resource it deletes (RGAA 11.1)", () => {
    mountSettings({ resources: { db: 2, cache: 3 } });
    for (const name of ["db", "cache"]) {
      const btn = screen.getByRole("button", { name: `Remove resource ${name}` });
      expect(btn.textContent).toContain("✕");
    }
  });
});

describe("WorkflowSettingsForm axe", () => {
  it("Workflow settings · WorkflowSettingsForm — resource remove names, axe smoke", async () => {
    const container = mountSettings({ resources: { db: 2, cache: 3 } });
    await expectNoViolationsStrict(container, "WorkflowSettingsForm");
  });
});
