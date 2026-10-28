// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { IterDocument, PromptDecl } from "@/api/types";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";
import {
  DocumentStoreProvider,
  createDocumentStore,
  type DocumentStore,
} from "@/store/document";
import PromptEditor from "./PromptEditor";

// In the non-compact (modal) branch the visible "Prompt body" caption was a
// bare sibling <label>: the PromptBodyEditor textarea below it had only its
// placeholder for a name (axe 4.13 passes placeholder-only, the triage does
// not — RGAA 11.1). The caption now labels that textarea, so getByLabelText
// resolves to it. And N sibling cards repeated the same visible "Delete"
// without naming the prompt it removes; each card's button now carries the
// prompt name, with the 1-based position as fallback after `Remove
// variable`'s pattern.

setupMatchMedia();

afterEach(cleanup);

function mountPrompts(prompts: PromptDecl[], filterName?: string) {
  const doc: IterDocument = {
    prompts,
    schemas: [],
    agents: [],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [],
    comments: [],
  };
  const store: DocumentStore = createDocumentStore();
  store.getState().setDocument(doc);
  const { container } = render(
    <main>
      <DocumentStoreProvider store={store}>
        <PromptEditor filterName={filterName} compact={false} />
      </DocumentStoreProvider>
    </main>,
  );
  return container;
}

describe("PromptEditor card controls", () => {
  it("labels the non-compact body textarea with its Prompt body caption (RGAA 11.1)", () => {
    mountPrompts([{ name: "greet", body: "Hello {{input.name}}" }], "greet");
    const body = screen.getByLabelText("Prompt body") as HTMLTextAreaElement;
    expect(body.tagName).toBe("TEXTAREA");
    // It is THE body editor of this card, not any other control.
    expect(body.value).toBe("Hello {{input.name}}");
  });

  it("names each card's Delete button after the prompt it deletes (RGAA 11.1)", () => {
    mountPrompts([
      { name: "greet", body: "hi" },
      { name: "farewell", body: "bye" },
    ]);
    for (const name of ["greet", "farewell"]) {
      const del = screen.getByRole("button", { name: `Delete prompt ${name}` });
      // The accessible name qualifies; the visible text is untouched.
      expect(del.textContent).toBe("Delete");
    }
    // A nameless prompt falls back to its 1-based position.
    mountPrompts([{ name: "greet", body: "hi" }, { name: "", body: "" }]);
    expect(screen.getByRole("button", { name: "Delete prompt 2" })).toBeTruthy();
  });
});

describe("PromptEditor axe", () => {
  it("Prompts · PromptEditor — body caption and card Delete names, axe smoke", async () => {
    // The bare single-card mount (filterName): the confirm dialog is closed,
    // so no Radix portal is mounted and the render container suffices.
    const container = mountPrompts([{ name: "greet", body: "Hello {{input.name}}" }], "greet");
    await expectNoViolationsStrict(container, "PromptEditor");
  });
});
