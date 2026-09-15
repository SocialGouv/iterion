// @vitest-environment jsdom

import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import AssistantApprovalComposer from "./AssistantApprovalComposer";

describe("AssistantApprovalComposer", () => {
  it("rejects an approval-only turn without inventing text", async () => {
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    render(
      <AssistantApprovalComposer
        hasTextField={false}
        busy={false}
        onSubmit={onSubmit}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Reject" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledWith(false, undefined));
  });

  it("collects revision text for a hybrid turn", async () => {
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    render(
      <AssistantApprovalComposer
        hasTextField
        busy={false}
        onSubmit={onSubmit}
      />,
    );

    fireEvent.click(
      screen.getByRole("button", { name: "Request revision" }),
    );
    fireEvent.change(screen.getByPlaceholderText("What should be revised?"), {
      target: { value: "Add the missing test" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(onSubmit).toHaveBeenCalledWith(false, "Add the missing test"),
    );
  });
});
