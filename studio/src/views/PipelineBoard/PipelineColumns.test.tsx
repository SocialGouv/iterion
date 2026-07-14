import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";

import type { PipelineBoardCard, PipelineBoardColumn } from "@/api/pipelineBoards";

vi.mock("@/api/runs", () => ({
  resumeRun: vi.fn(),
}));

vi.mock("wouter", () => ({
  Link: ({
    href,
    children,
    ...props
  }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) => (
    <a href={href} {...props}>
      {children}
    </a>
  ),
}));

vi.mock("@/components/Runs/conversation/HumanPromptForm", () => ({
  default: (props: {
    runId: string;
    nodeId: string;
    questions: Record<string, unknown>;
    sourceOverride?: string | null;
  }) => (
    <div
      data-testid="human-prompt"
      data-run-id={props.runId}
      data-node-id={props.nodeId}
      data-source-null={props.sourceOverride === null ? "yes" : "no"}
    >
      {JSON.stringify(props.questions)}
    </div>
  ),
}));

import { PipelineColumns } from "./PipelineColumns";

const columns: PipelineBoardColumn[] = [
  { id: "running", title: "Running", kind: "running" },
  {
    id: "approval",
    title: "Approval",
    kind: "interaction",
    node_id: "approval",
  },
];

describe("PipelineColumns", () => {
  it("renders an indented child interaction for the exact child run", () => {
    const cards: PipelineBoardCard[] = [
      {
        id: "root",
        kind: "run",
        column_id: "running",
        title: "Root pipeline",
        run_id: "run-root",
        depth: 0,
        status: "running",
        children_count: 1,
      },
      {
        id: "child",
        kind: "run",
        column_id: "approval",
        title: "Child review",
        run_id: "run-child",
        root_run_id: "run-root",
        parent_run_id: "run-root",
        depth: 2,
        status: "paused_waiting_human",
        node_id: "approval",
        questions: { approved: "Ship it?" },
        attempts: [{ run_id: "old" }, { run_id: "run-child" }],
      },
    ];

    const html = renderToStaticMarkup(
      <PipelineColumns columns={columns} cards={cards} onChanged={() => {}} />,
    );

    expect(html).toContain('data-card-id="child"');
    expect(html).toContain('data-depth="2"');
    expect(html).toContain("Child of");
    expect(html).toContain("Root pipeline");
    expect(html).toContain("2 attempts");
    expect(html).toContain("1 child");
    expect(html).toContain('role="region"');
    expect(html).toContain('role="article"');
    expect(html).toContain('aria-label="Open run run-child in the run console"');
    expect(html).not.toContain('draggable="true"');
    expect(html).toContain('data-run-id="run-child"');
    expect(html).toContain('data-node-id="approval"');
    expect(html).toContain('data-source-null="yes"');
    expect(html).toContain("Ship it?");
  });

  it("renders the explicit resume action for an operator pause", () => {
    const html = renderToStaticMarkup(
      <PipelineColumns
        columns={columns}
        cards={[
          {
            id: "operator-pause",
            kind: "run",
            column_id: "running",
            title: "Inspect before continuing",
            run_id: "run-op",
            depth: 0,
            status: "paused_operator",
          },
        ]}
        onChanged={() => {}}
      />,
    );

    expect(html).toContain("Paused (operator)");
    expect(html).toContain("Resume run");
    expect(html).toContain("/runs/run-op");
  });

  it("keeps cards visible when their column is absent from the topology", () => {
    const html = renderToStaticMarkup(
      <PipelineColumns
        columns={columns}
        cards={[
          {
            id: "dynamic",
            kind: "run",
            column_id: "dynamic-input",
            title: "Runtime ask_user",
            run_id: "run-dynamic",
            depth: 0,
            status: "paused_waiting_human",
            node_id: "agent",
          },
        ]}
        onChanged={() => {}}
      />,
    );

    expect(html).toContain("dynamic-input");
    expect(html).toContain("Runtime ask_user");
  });

  it("explains why a legacy human pause without a node cannot render a form", () => {
    const html = renderToStaticMarkup(
      <PipelineColumns
        columns={columns}
        cards={[
          {
            id: "legacy-pause",
            kind: "run",
            column_id: "approval",
            title: "Legacy approval",
            run_id: "run-legacy",
            depth: 0,
            status: "paused_waiting_human",
          },
        ]}
        onChanged={() => {}}
      />,
    );

    expect(html).toContain("cannot be answered inline");
    expect(html).not.toContain('data-run-id="run-legacy"');
  });
});
