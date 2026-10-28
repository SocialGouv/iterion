// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { IterDocument, MCPServerDecl } from "@/api/types";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";
import {
  DocumentStoreProvider,
  createDocumentStore,
  type DocumentStore,
} from "@/store/document";
import MCPServersEditor from "./MCPServersEditor";

// One "Remove" per server card, and every card's copy said only that: a
// screen-reader user tabbing the list heard N identical buttons and none of
// them named the server it was about to delete (RGAA 11.1, repeated
// controls). The button keeps its visible "Remove" text — the server name
// travels in the aria-label.

setupMatchMedia();

afterEach(cleanup);

function seed(servers: MCPServerDecl[]): IterDocument {
  return {
    prompts: [],
    schemas: [],
    agents: [],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [],
    mcp_servers: servers,
    comments: [],
  };
}

function mountServers(servers: MCPServerDecl[]) {
  const store: DocumentStore = createDocumentStore();
  store.getState().setDocument(seed(servers));
  const { container } = render(
    <DocumentStoreProvider store={store}>
      <MCPServersEditor />
    </DocumentStoreProvider>,
  );
  return container;
}

describe("MCPServersEditor server actions", () => {
  it("names each card's Remove button after the server it deletes (RGAA 11.1)", () => {
    mountServers([
      { name: "tools_db", transport: "stdio", command: "npx" },
      { name: "tools_search", transport: "http", url: "https://mcp.example" },
    ]);
    for (const name of ["tools_db", "tools_search"]) {
      const btn = screen.getByRole("button", { name: `Remove server ${name}` });
      // The accessible name qualifies; the visible text is untouched — the
      // sighted label and the announced one describe the same action.
      expect(btn.textContent).toBe("Remove");
    }
  });
});

describe("MCPServersEditor axe", () => {
  it("MCP servers · MCPServersEditor — per-server Remove names, axe smoke", async () => {
    const container = mountServers([
      { name: "tools_db", transport: "stdio", command: "npx" },
      { name: "tools_search", transport: "http", url: "https://mcp.example" },
    ]);
    await expectNoViolationsStrict(container, "MCPServersEditor");
  });
});
