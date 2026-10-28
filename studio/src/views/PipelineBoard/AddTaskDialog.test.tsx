// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import AddTaskDialog from "./AddTaskDialog";
import { AuthProvider } from "@/auth/AuthContext";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

setupMatchMedia();

afterEach(cleanup);

// One mount path: a provider the dialog grows is added here and nowhere else.
function renderDialog({ advanced = true }: { advanced?: boolean } = {}) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const view = render(
    <AuthProvider>
      <QueryClientProvider client={client}>
        <AddTaskDialog open onOpenChange={() => {}} onCreated={() => {}} />
      </QueryClientProvider>
    </AuthProvider>
  );
  if (advanced) {
    fireEvent.click(screen.getByRole("button", { name: "Advanced parameters" }));
  }
  return view;
}

describe("AddTaskDialog", () => {
  it("associates the visible Labels heading with the tag field (RGAA 11.1)", () => {
    renderDialog();
    const field = screen.getByLabelText("Labels") as HTMLInputElement;
    expect(field.id).toBe("add-task-labels");
  });
});

describe("a11y / AddTaskDialog", () => {
  // Both halves: the dialog as it opens, then with the advanced panel
  // expanded — that is where the labelled parameter rows live, and a net on
  // the collapsed form alone would judge an empty fieldset.
  it("has no violation as it opens", async () => {
    renderDialog({ advanced: false });
    await expectNoViolationsStrict(screen.getByRole("dialog"), "AddTaskDialog");
  });

  it("has no violation with the advanced parameters expanded", async () => {
    renderDialog();
    await expectNoViolationsStrict(
      screen.getByRole("dialog"),
      "AddTaskDialog (advanced)",
    );
  });
});
