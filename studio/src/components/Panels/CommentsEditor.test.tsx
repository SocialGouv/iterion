// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { Comment, IterDocument } from "@/api/types";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";
import {
  DocumentStoreProvider,
  createDocumentStore,
  type DocumentStore,
} from "@/store/document";
import CommentsEditor from "./CommentsEditor";

// One placeholder-only textarea per comment card: the placeholder is not an
// accessible name (axe 4.13 passes it outright, the triage does not), and N
// sibling cards repeated the same visible "Delete" without saying which
// comment it removes (RGAA 11.1, repeated controls). The 1-based position is
// the qualifier the card header already shows visually ("## Comment N").

setupMatchMedia();

afterEach(cleanup);

function mountComments(comments: Comment[]) {
  const doc: IterDocument = {
    prompts: [],
    schemas: [],
    agents: [],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [],
    comments,
  };
  const store: DocumentStore = createDocumentStore();
  store.getState().setDocument(doc);
  const { container } = render(
    <main>
      <DocumentStoreProvider store={store}>
        <CommentsEditor />
      </DocumentStoreProvider>
    </main>,
  );
  return container;
}

describe("CommentsEditor comment controls", () => {
  it("names each comment's textarea after its position (RGAA 11.1)", () => {
    mountComments([{ text: "first note" }, { text: "second note" }]);
    const first = screen.getByLabelText("Comment 1 text") as HTMLTextAreaElement;
    const second = screen.getByLabelText("Comment 2 text") as HTMLTextAreaElement;
    expect(first.tagName).toBe("TEXTAREA");
    expect(second.tagName).toBe("TEXTAREA");
    // Two distinct controls carry two distinct texts — not one element found twice.
    expect(first).not.toBe(second);
    expect(first.value).toBe("first note");
    expect(second.value).toBe("second note");
  });

  it("names each card's Delete button after the comment it deletes (RGAA 11.1)", () => {
    mountComments([{ text: "first note" }, { text: "second note" }]);
    for (const i of [1, 2]) {
      const del = screen.getByRole("button", { name: `Delete comment ${i}` });
      // The accessible name qualifies; the visible text is untouched.
      expect(del.textContent).toBe("Delete");
    }
  });
});

describe("CommentsEditor axe", () => {
  it("Comments · CommentsEditor — per-comment control names, axe smoke", async () => {
    const container = mountComments([{ text: "first note" }, { text: "second note" }]);
    await expectNoViolationsStrict(container, "CommentsEditor");
  });
});
