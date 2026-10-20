// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { createDocumentStore, DocumentStoreProvider } from "@/store/document";
import InspectorNode from "./InspectorNode";

// The node forms share an import path whose vendor barrels vitest cannot
// resolve (@lobehub/ui → @base-ui/react/merge-props, same trap as
// AgentForm.permission.test). They are not the subject here — the rename
// field is.
vi.mock("@/components/Panels/forms/AgentForm", () => ({ default: () => null }));
vi.mock("@/components/Panels/forms/RouterForm", () => ({ default: () => null }));
vi.mock("@/components/Panels/forms/HumanForm", () => ({ default: () => null }));
vi.mock("@/components/Panels/forms/ToolForm", () => ({ default: () => null }));
vi.mock("@/components/Panels/forms/ComputeForm", () => ({ default: () => null }));

afterEach(cleanup);

describe("InspectorNode", () => {
  it("names its inline rename field (RGAA 11.1)", () => {
    const store = createDocumentStore();
    store.getState().setDocument({
      prompts: [],
      schemas: [],
      agents: [],
      judges: [],
      routers: [],
      humans: [],
      tools: [{ name: "scanner", command: "echo hi" }],
      computes: [],
      workflows: [],
      comments: [],
    } as never);
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <DocumentStoreProvider store={store}>
          <InspectorNode nodeId="scanner" />
        </DocumentStoreProvider>
      </QueryClientProvider>,
    );
    fireEvent.click(screen.getByTitle("Click to rename"));
    expect(screen.getByRole("textbox", { name: "Rename node" })).toBeTruthy();
  });
});
