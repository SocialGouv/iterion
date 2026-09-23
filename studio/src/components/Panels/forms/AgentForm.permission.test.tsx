// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { AgentDecl, IterDocument, JudgeDecl } from "@/api/types";

// Two vendor icon barrels on this form's import path resolve their ESM
// subpaths in a way vitest cannot follow (@lobehub/icons via ProviderIcon).
// Pure presentation, and not on the branch under test — the same stub the
// run-overview suite uses.
vi.mock("@/components/icons/ProviderIcon", () => ({
  ProviderIcon: () => null,
  ProviderLabel: () => null,
}));

import {
  DocumentStoreProvider,
  createDocumentStore,
  useDocumentStore,
  type DocumentStore,
} from "@/store/document";
import AgentForm from "./AgentForm";

// #1222 made `permission:` / `allow:` / `ask:` / `deny:` declarable on agent
// and judge nodes; #1580 is the inspector's missing controls. What matters
// here is what a control WRITES into the document, because that is what the
// save unparses: the wire carries these lists with `omitempty`, so an
// emptied control that wrote `[]` would be indistinguishable from "not
// declared" — the author would think they had cleared the workflow's list
// while the node quietly inherited it (docs/permissions.md, "Declared means
// non-empty").

afterEach(cleanup);

beforeEach(() => {
  // useResolvedModel / useEffortCapabilities are react-query hooks. Nothing
  // under test reads their answer; refuse the call rather than let jsdom
  // reach the network.
  globalThis.fetch = vi.fn(async () => {
    throw new Error("no network in this test");
  }) as unknown as typeof fetch;
});

type Kind = "agent" | "judge";

function node(name: string): AgentDecl & JudgeDecl {
  return {
    name,
    model: "anthropic/claude-opus-5",
    input: "in",
    output: "out",
    system: "s",
    user: "u",
    session: "fresh",
  };
}

function seed(kind: Kind): IterDocument {
  const decl = node(kind === "agent" ? "worker" : "verdict");
  return {
    prompts: [],
    schemas: [],
    agents: kind === "agent" ? [decl] : [],
    judges: kind === "judge" ? [decl] : [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [],
    comments: [],
  };
}

/** The inspector re-reads the declaration from the store on every render;
 *  a harness holding one captured object would show the form its own
 *  pre-edit state and no second edit would ever land. */
function Inspector({ kind }: { kind: Kind }) {
  const decl = useDocumentStore((s) =>
    kind === "agent" ? s.document?.agents[0] : s.document?.judges[0],
  );
  if (!decl) return null;
  return <AgentForm decl={decl} kind={kind} />;
}

function mount(kind: Kind) {
  const store: DocumentStore = createDocumentStore();
  store.getState().setDocument(seed(kind));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <DocumentStoreProvider store={store}>
        <Inspector kind={kind} />
      </DocumentStoreProvider>
    </QueryClientProvider>,
  );
  const current = () => {
    const d = store.getState().document!;
    return (kind === "agent" ? d.agents[0] : d.judges[0])!;
  };
  return { store, current };
}

/** The control a field row labels. A tag list drops its placeholder once it
 *  holds a tag, so the row's label is the only stable handle. */
function fieldInput(label: string): HTMLInputElement {
  const row = Array.from(globalThis.document.querySelectorAll("label")).find(
    (l) => l.firstChild?.textContent?.trim() === label,
  );
  if (!row) throw new Error(`no field labelled ${label}`);
  const input = row.parentElement?.querySelector("input");
  if (!input) throw new Error(`field ${label} has no input`);
  return input as HTMLInputElement;
}

const RULE_LABELS = { allow: "Allow", ask: "Ask", deny: "Deny" } as const;

function addRule(kind: keyof typeof RULE_LABELS, rule: string) {
  const input = fieldInput(RULE_LABELS[kind]);
  fireEvent.change(input, { target: { value: rule } });
  fireEvent.keyDown(input, { key: "Enter" });
}

describe.each(["agent", "judge"] as const)("AgentForm permission controls (%s)", (kind) => {
  it("writes the gate mode, and inherits the workflow's when emptied", () => {
    const { current } = mount(kind);
    fireEvent.change(screen.getByLabelText(/^Permission/), { target: { value: "deny" } });
    expect(current().permission).toBe("deny");

    fireEvent.change(screen.getByLabelText(/^Permission/), { target: { value: "" } });
    // Not "" — an empty string would be a declared mode the DSL has no
    // value for; absent is what "inherit the workflow's" is on the wire.
    expect(current().permission).toBeUndefined();
  });

  it("offers exactly the three gate modes the DSL accepts, plus inherit", () => {
    mount(kind);
    const select = screen.getByLabelText(/^Permission/) as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.value)).toEqual(["", "off", "ask", "deny"]);
  });

  it.each(["allow", "ask", "deny"] as const)("writes the %s list", (rules) => {
    const { current } = mount(kind);
    addRule(rules, "Bash(git diff:*)");
    expect(current()[rules]).toEqual(["Bash(git diff:*)"]);

    addRule(rules, "Read(**)");
    expect(current()[rules]).toEqual(["Bash(git diff:*)", "Read(**)"]);
  });

  it.each(["allow", "ask", "deny"] as const)(
    "drops the %s list rather than declaring it empty",
    (rules) => {
      const { current } = mount(kind);
      addRule(rules, "Read(**)");
      expect(current()[rules]).toEqual(["Read(**)"]);

      fireEvent.click(screen.getByLabelText("Remove Read(**)"));
      // `[]` round-trips through `omitempty` as "not declared" anyway, so
      // writing it would make the document disagree with the file it saves
      // to. The control writes what the file will say.
      expect(current()[rules]).toBeUndefined();
    },
  );

  it("declares one kind without touching the others", () => {
    const { current } = mount(kind);
    addRule("deny", "Bash");
    expect(current().deny).toEqual(["Bash"]);
    // Replacement is per KIND: declaring deny leaves allow and ask absent,
    // so the node goes on inheriting the workflow's two.
    expect(current().allow).toBeUndefined();
    expect(current().ask).toBeUndefined();
  });

  it("says a node list replaces the workflow's, on each of the three", () => {
    mount(kind);
    const hints = screen.getAllByLabelText(/REPLACES the workflow's list of this kind/);
    expect(hints).toHaveLength(3);
    for (const hint of hints) {
      expect(hint.getAttribute("aria-label")).toMatch(/never a union/);
      expect(hint.getAttribute("aria-label")).toMatch(/independently per kind/);
    }
  });
});
