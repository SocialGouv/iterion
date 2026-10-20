// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import AddTaskDialog from "./AddTaskDialog";
import { AuthProvider } from "@/auth/AuthContext";

afterEach(cleanup);

function renderDialog() {
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
  fireEvent.click(screen.getByRole("button", { name: "Advanced parameters" }));
  return view;
}

describe("AddTaskDialog", () => {
  it("associates the visible Labels heading with the tag field (RGAA 11.1)", () => {
    renderDialog();
    const field = screen.getByLabelText("Labels") as HTMLInputElement;
    expect(field.id).toBe("add-task-labels");
  });
});
