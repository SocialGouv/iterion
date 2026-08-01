// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { getDefaultRunStore, useRunStoreInstance } from "@/store/run";

import { AssistantProvider, AssistantStoreScope } from "./AssistantProvider";

// The real hook opens a websocket and lists runs on mount; none of that
// is what this file is about.
vi.mock("@/lib/whats-next/useWhatsNextSession", () => ({
  useWhatsNextSession: () => ({
    status: "idle",
    runId: null,
    messages: [],
    busyMessageId: null,
    runStatus: null,
    errorMessage: null,
    lastVars: null,
    discoveryError: null,
    retryDiscovery: () => {},
    sessionRepo: null,
    launchRepo: null,
    launch: async () => {},
    submitHumanAnswer: async () => {},
    newSession: () => {},
    resume: async () => {},
  }),
}));

afterEach(cleanup);

// Reports which run store its position in the tree resolves to.
function Probe({ id }: { id: string }) {
  const store = useRunStoreInstance();
  return (
    <span data-testid={id}>
      {store === getDefaultRunStore() ? "default" : "assistant"}
    </span>
  );
}

describe("AssistantProvider run-store isolation", () => {
  // The whole point of giving the always-mounted session its own store:
  // on the default store it would permanently hold the assistant's run
  // for every shell-level consumer (useDocumentTitle would then title
  // /runs/:id after the assistant's run).
  it("hands the DEFAULT store to the app below it", () => {
    render(
      <AssistantProvider>
        <Probe id="route-tree" />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("route-tree").textContent).toBe("default");
  });

  it("hands the ASSISTANT store to an AssistantStoreScope", () => {
    render(
      <AssistantProvider>
        <AssistantStoreScope>
          <Probe id="dock" />
        </AssistantStoreScope>
      </AssistantProvider>,
    );
    expect(screen.getByTestId("dock").textContent).toBe("assistant");
  });

  it("keeps both resolutions straight in one tree", () => {
    render(
      <AssistantProvider>
        <Probe id="outside" />
        <AssistantStoreScope>
          <Probe id="inside" />
        </AssistantStoreScope>
      </AssistantProvider>,
    );
    expect(screen.getByTestId("outside").textContent).toBe("default");
    expect(screen.getByTestId("inside").textContent).toBe("assistant");
  });

  // Outside the provider entirely (a surface rendered before the
  // authenticated shell), the scope must be inert rather than throw.
  it("is inert outside the provider", () => {
    render(
      <AssistantStoreScope>
        <Probe id="orphan" />
      </AssistantStoreScope>,
    );
    expect(screen.getByTestId("orphan").textContent).toBe("default");
  });
});
