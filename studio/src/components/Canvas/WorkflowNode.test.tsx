// @vitest-environment jsdom

// studio-ui.editor-llm-meta: the agent/judge canvas card shows the RESOLVED
// model (not the raw `${VAR:-…}` the author wrote, not the word "env") and
// the authored `fallbacks:` chain. The formatting functions are unit-tested
// in lib/modelLabel.test.ts; this test pins the WIRING — the card calling
// displayModel / displayFallbackChain with the server-resolved value — by
// rendering the real WorkflowNode with the resolution cache pre-seeded the
// way fetchResolvedModel would fill it. No live model anywhere: the
// resolution answer is data in the query cache.

import type { ComponentProps, ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

// House pattern (SubbotFrameNode.test.tsx): xyflow is never mounted, so
// Handle is stubbed out; Position is a plain enum consumed by
// handlePositions.ts.
vi.mock("@xyflow/react", () => ({
  Handle: () => null,
  Position: { Top: "top", Right: "right", Bottom: "bottom", Left: "left" },
}));

vi.mock("@/hooks/useActiveWorkflow", () => ({
  useActiveWorkflow: () => ({ entry: "survey", edges: [] }),
}));
vi.mock("@/hooks/useGroupedDiagnostics", () => ({
  useGroupedDiagnostics: () => ({ byNode: new Map() }),
}));
vi.mock("@/store/selection", () => ({
  useSelectionStore: (sel: (s: { setSelectedNode: () => void }) => unknown) =>
    sel({ setSelectedNode: () => {} }),
}));
// The effort row is a different surface (its own hooks, its own fetches);
// the model label and the fallback chain must not depend on it.
vi.mock("@/hooks/useEffortCapabilities", () => ({
  effortBackendKey: (b: string | undefined) => b ?? "claw",
  useEffortCapabilities: () => ({ capabilities: undefined }),
}));
vi.mock("@/hooks/useResolvedEffort", () => ({
  useResolvedEffort: (v: string | undefined) => v,
}));
// The diagnostic badge's import chain (@/components/ui → @lobehub/ui
// Tooltip) does not resolve under vitest's node ESM (a pre-existing
// packaging gap no test hit before); the badge renders only when the node
// carries diagnostics, which this test's never does.
vi.mock("@/components/Diagnostics/DiagnosticBadge", () => ({
  default: () => null,
}));
// Same pre-existing gap via @lobehub/icons → @lobehub/ui: the provider and
// backend GLYPHS are decoration beside the label text this test asserts.
vi.mock("@/components/icons/ProviderIcon", () => ({ ProviderIcon: () => null }));
vi.mock("@/components/icons/BackendBadge", () => ({ BackendBadge: () => null }));

import WorkflowNode from "./WorkflowNode";

const AUTHORED = "${CODEX_MODEL:-openai-codex/gpt-5.6-sol}";
const RESOLVED = "openai-codex/gpt-5.6-terra";

type CardProps = ComponentProps<typeof WorkflowNode>;

function makeProps(): CardProps {
  return {
    id: "survey",
    type: "workflow",
    selected: false,
    dragging: false,
    draggable: true,
    selectable: true,
    deletable: true,
    isConnectable: true,
    zIndex: 0,
    positionAbsoluteX: 0,
    positionAbsoluteY: 0,
    data: {
      label: "survey",
      kind: "agent",
      color: "var(--color-node-agent)",
      decl: {
        name: "survey",
        backend: "claw",
        model: AUTHORED,
        fallbacks: [
          { name: "terra", model: "openai-codex/gpt-5.6-terra" },
          { name: "api", backend: "claw", model: "openai/gpt-5.5", metered: true },
        ],
      },
    },
  } as unknown as CardProps;
}

function renderCard(queryClient: QueryClient) {
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
  return render(<WorkflowNode {...makeProps()} />, { wrapper });
}

describe("WorkflowNode model metadata", () => {
  afterEach(cleanup);

  it("shows the server-resolved model, not the raw env-subst literal", () => {
    const client = new QueryClient();
    // What fetchResolvedModel would have cached for the authored literal.
    client.setQueryData(["resolved-model", AUTHORED], RESOLVED);
    renderCard(client);
    const label = screen.getByTestId("node-model");
    expect(label.textContent).toContain("gpt-5.6-terra");
    expect(label.textContent).not.toContain("env");
    expect(label.textContent).not.toContain("gpt-5.6-sol");
  });

  it("shows the authored default while the resolution has not landed", () => {
    renderCard(new QueryClient());
    const label = screen.getByTestId("node-model");
    expect(label.textContent).toContain("gpt-5.6-sol");
    expect(label.textContent).not.toContain("env");
  });

  it("renders the authored fallbacks chain in declaration order", () => {
    renderCard(new QueryClient());
    expect(screen.getByTestId("node-fallbacks").textContent).toContain(
      "gpt-5.6-terra → claw/gpt-5.5",
    );
  });
});
