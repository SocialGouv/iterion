// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";
import {
  referenceForRoute,
  type TypedReference,
} from "@/lib/chatDock/routeReference";

import AttachedReferences from "./AttachedReferences";
import { ChatDockShell } from "./ChatDockShell";
import ContextChip from "./ContextChip";
import { ConversationStrip } from "./ConversationStrip";

afterEach(cleanup);

// The assistant dock repeats its chrome per conversation and per attached
// reference, so every one of those controls has to say WHICH row it acts on
// (#2591's surface triage). Visible text is untouched throughout.
const CONVERSATIONS = [
  { id: "c-1", botId: "copilot", runId: "run-1" },
  { id: "c-2", botId: "vigie", runId: "run-2" },
];

const LABELS: Record<string, string> = { "c-1": "Copi", "c-2": "Vigie" };

// The hostile case this surface really produces: nothing dedupes two
// conversations with the SAME bot, and then one label names both tabs and
// both close controls.
const SAME_BOT = [
  { id: "c-1", botId: "copilot", runId: "run-1" },
  { id: "c-2", botId: "copilot", runId: "run-2" },
];

const REFERENCES: TypedReference[] = [
  { ref: "run:run-1", kind: "run", label: "nightly deploy" },
  { ref: "bot:bots/vigie/main.bot", kind: "bot", label: "vigie" },
];

function mountStrip() {
  return render(
    <main>
      <ConversationStrip
        conversations={CONVERSATIONS}
        activeId="c-1"
        atLimit={false}
        onSelect={() => {}}
        onClose={() => {}}
        onOpen={() => {}}
        labelFor={(c) => LABELS[c.id] ?? c.botId}
        unreadWatchIds={new Set(["c-2"])}
      />
    </main>,
  );
}

function mountReferences() {
  return render(
    <main>
      <AttachedReferences references={REFERENCES} onDetach={() => {}} />
    </main>,
  );
}

function mountShell() {
  return render(
    <ChatDockShell
      dock="docked-right"
      onDockChange={() => {}}
      title="Assistant"
      titleHint="Ask about the page you are on"
      dockedRightMode="self"
    >
      <div>body</div>
    </ChatDockShell>,
  );
}

describe("assistant dock naming", () => {
  it("names each conversation's close control after that conversation", () => {
    mountStrip();
    expect(
      screen.getByRole("button", {
        name: "Close Copi conversation and stop its run",
      }),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", {
        name: "Close Vigie conversation and stop its run",
      }),
    ).toBeTruthy();
  });

  it("separates two conversations of the same bot, tab and close alike", () => {
    render(
      <main>
        <ConversationStrip
          conversations={SAME_BOT}
          activeId="c-1"
          atLimit={false}
          onSelect={() => {}}
          onClose={() => {}}
          onOpen={() => {}}
          labelFor={() => "Copi"}
        />
      </main>,
    );
    for (const name of [
      "Copi 1",
      "Copi 2",
      "Close Copi 1 conversation and stop its run",
      "Close Copi 2 conversation and stop its run",
    ]) {
      expect(screen.getAllByRole("button", { name })).toHaveLength(1);
    }
    // The visible text is untouched: both tabs still read "Copi".
    expect(screen.getByRole("button", { name: "Copi 1" }).textContent).toBe("Copi");
  });

  it("leaves the ordinary single-label tab named exactly what it shows", () => {
    mountStrip();
    // Two distinct bots: no position, no aria-label — the name is the text.
    const tab = screen.getByRole("button", { name: "Copi" });
    expect(tab.getAttribute("aria-label")).toBeNull();
  });

  it("says a watch landed as the tab's description, never as its name", () => {
    mountStrip();
    // Both options are the real algorithms: `name` and `description` are
    // computed by dom-accessibility-api, not read off an attribute.
    expect(
      screen.getByRole("button", {
        name: "Vigie",
        description: "New automatic watch result",
      }),
    ).toBeTruthy();
    // The name does not move with the state: the unread tab is still "Vigie",
    // and the quiet one is still "Copi" with no description.
    expect(screen.getByRole("button", { name: "Copi" })).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Copi" }).getAttribute("aria-describedby"),
    ).toBeNull();
  });

  it("names each attached reference's remove after the reference", () => {
    mountReferences();
    expect(
      screen.getByRole("button", { name: "Remove run:run-1" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Remove bot:bots/vigie/main.bot" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("group", { name: "Attached references" }),
    ).toBeTruthy();
  });

  it("names the context banner's dismiss after the page it would drop", () => {
    const reference = referenceForRoute("/board");
    if (!reference) throw new Error("the board route must mint a reference");
    render(
      <main>
        <ContextChip
          state="pending"
          reference={reference}
          onDismiss={vi.fn()}
        />
      </main>,
    );
    expect(
      screen.getByRole("button", {
        name: `Remove ${reference.label} from the first message context`,
      }),
    ).toBeTruthy();
  });
});

describe("assistant dock axe", () => {
  it("ChatDock · conversation strip — axe smoke, incompletes included", async () => {
    const { container } = mountStrip();
    await expectNoViolationsStrict(container, "ConversationStrip");
  });

  it("ChatDock · attached references — axe smoke, incompletes included", async () => {
    const { container } = mountReferences();
    await expectNoViolationsStrict(container, "AttachedReferences");
  });

  it("ChatDock · context banner — axe smoke, incompletes included", async () => {
    const reference = referenceForRoute("/board");
    if (!reference) throw new Error("the board route must mint a reference");
    const { container } = render(
      <main>
        <ContextChip state="pending" reference={reference} onDismiss={vi.fn()} />
      </main>,
    );
    await expectNoViolationsStrict(container, "ContextChip");
  });

  it("ChatDock · shell chrome — axe smoke, incompletes included", async () => {
    mountShell();
    // The shell portals its panel chrome to the body, so the whole document
    // is the surface under test.
    await expectNoViolationsStrict(document.body, "ChatDockShell");
  });
});
