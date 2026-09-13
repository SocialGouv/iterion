// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ConversationStrip } from "./ConversationStrip";

const conversation = { id: "c-1", botId: "copilot", runId: "run-1" };

afterEach(cleanup);

describe("ConversationStrip disposal affordance", () => {
  it("shows an explicit stop control for the only conversation", () => {
    const onClose = vi.fn();
    render(
      <ConversationStrip
        conversations={[conversation]}
        activeId="c-1"
        atLimit={false}
        onSelect={() => {}}
        onClose={onClose}
        onOpen={() => {}}
        labelFor={() => "Copi"}
      />,
    );
    const close = screen.getByRole("button", {
      name: "Close Copi conversation and stop its run",
    });
    expect(close.getAttribute("title")).toMatch(/stop its run/i);
    fireEvent.click(close);
    expect(onClose).toHaveBeenCalledWith("c-1");
  });

  it("disables selection and closing while cancellation is being accepted", () => {
    render(
      <ConversationStrip
        conversations={[conversation]}
        activeId="c-1"
        atLimit={false}
        onSelect={() => {}}
        onClose={() => {}}
        onOpen={() => {}}
        labelFor={() => "Copi"}
        closingIds={new Set(["c-1"])}
      />,
    );
    expect(
      (screen.getByRole("button", { name: "Closing Copi conversation" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    expect(
      (screen.getByRole("button", { name: "Copi" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  it("marks a background conversation parked on a human gate", () => {
    render(
      <ConversationStrip
        conversations={[
          conversation,
          { id: "c-2", botId: "copilot", runId: "run-2" },
        ]}
        activeId="c-1"
        atLimit={false}
        onSelect={() => {}}
        onClose={() => {}}
        onOpen={() => {}}
        labelFor={() => "Copi"}
        waitingIds={new Set(["c-2"])}
      />,
    );
    expect(screen.getByLabelText("Ready for your next message")).toBeTruthy();
  });

  it("distinguishes a new automatic watch result from an ordinary wait", () => {
    render(
      <ConversationStrip
        conversations={[conversation]}
        activeId="c-1"
        atLimit={false}
        onSelect={() => {}}
        onClose={() => {}}
        onOpen={() => {}}
        labelFor={() => "Copi"}
        waitingIds={new Set(["c-1"])}
        unreadWatchIds={new Set(["c-1"])}
      />,
    );
    expect(screen.getByLabelText("New automatic watch result")).toBeTruthy();
    expect(screen.queryByLabelText("Ready for your next message")).toBeNull();
  });
});
