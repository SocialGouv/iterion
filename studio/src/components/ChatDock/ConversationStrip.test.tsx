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
    // The state is the tab's DESCRIPTION, not its name: the name is the bot
    // (plus its position, since both conversations answer to "Copi"), and it
    // must not move when a gate parks the run.
    expect(
      screen.getByRole("button", {
        name: "Copi 2",
        description: "Ready for your next message",
      }),
    ).toBeTruthy();
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
    expect(
      screen.getByRole("button", {
        name: "Copi",
        description: "New automatic watch result",
      }),
    ).toBeTruthy();
    // And only that one: a watch result is not an ordinary wait.
    expect(
      screen.queryByRole("button", {
        name: "Copi",
        description: "Ready for your next message",
      }),
    ).toBeNull();
    expect(screen.queryByLabelText("Ready for your next message")).toBeNull();
  });
});
