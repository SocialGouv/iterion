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

function seed(kind: Kind, over: Partial<AgentDecl> = {}): IterDocument {
  const decl = { ...node(kind === "agent" ? "worker" : "verdict"), ...over };
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

function mount(kind: Kind, over: Partial<AgentDecl> = {}) {
  const store: DocumentStore = createDocumentStore();
  store.getState().setDocument(seed(kind, over));
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

/** The LABEL of a field row, by its own text. A tag list drops its
 *  placeholder once it holds a tag, so the row's label is the only stable
 *  handle for both the control and the help it carries. */
function fieldRow(label: string): HTMLLabelElement {
  const row = Array.from(globalThis.document.querySelectorAll("label")).find(
    (l) => l.firstChild?.textContent?.trim() === label,
  );
  if (!row) throw new Error(`no field labelled ${label}`);
  return row;
}

function fieldInput(label: string): HTMLInputElement {
  const input = fieldRow(label).parentElement?.querySelector("input");
  if (!input) throw new Error(`field ${label} has no input`);
  return input as HTMLInputElement;
}

/** The help text a field row announces, reached the way a screen reader
 *  reaches it: the control is found by its LABEL, and its
 *  `aria-describedby` names the hint. Fetching the hint by a phrase of the
 *  text instead would put part of what the assertions check into the
 *  locator, and a wording regression would then fail as "unable to find a
 *  label" — on the locator, never on the claim. */
function fieldHelp(label: RegExp): string {
  const control = screen.getByLabelText(label);
  const id = control.getAttribute("aria-describedby");
  if (!id) throw new Error(`the control ${label} announces no help`);
  const hint = globalThis.document.getElementById(id);
  if (!hint) throw new Error(`the control ${label} describes a missing element ${id}`);
  return hint.getAttribute("aria-label") ?? "";
}

/** The help a RULE-LIST row announces. A tag list gets no
 *  `aria-describedby` — FormField's TagListField renders plain children, so
 *  only SelectField pipes the id through — which is why fieldHelp() cannot
 *  serve here: the hint is reached through the row's label instead, never by
 *  a phrase of its own text. */
function ruleHelp(label: string): string {
  const hint = fieldRow(label).querySelector("[aria-label]");
  if (!hint) throw new Error(`the ${label} list announces no help`);
  return hint.getAttribute("aria-label") ?? "";
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

  it.each(["allow", "ask", "deny"] as const)(
    "records a %s rule that differs from another only in CASE",
    (rules) => {
      const { current } = mount(kind);
      addRule(rules, "Read(.env*)");
      addRule(rules, "Read(.ENV*)");
      // The engine matches rule arguments case-sensitively: `Read(.env*)`
      // does not cover `.ENV`. The tag widget was written for board labels
      // and folds case by default, which silently dropped the second rule
      // with the draft cleared and no message — on a security control.
      expect(current()[rules]).toEqual(["Read(.env*)", "Read(.ENV*)"]);
    },
  );

  it.each(["allow", "ask", "deny"] as const)(
    "keeps a comma inside a %s rule instead of committing there",
    (rules) => {
      const { current } = mount(kind);
      const input = fieldInput(RULE_LABELS[rules]);
      // The comma is TYPED, because that is the keystroke the widget reads
      // as a separator: a rule committed there enters the list truncated
      // (`Bash(git commit -m "a`) and the author loses the rest of it.
      fireEvent.change(input, { target: { value: 'Bash(git commit -m "a' } });
      fireEvent.keyDown(input, { key: "," });
      fireEvent.change(input, { target: { value: 'Bash(git commit -m "a,b":*)' } });
      fireEvent.keyDown(input, { key: "Enter" });
      expect(current()[rules]).toEqual(['Bash(git commit -m "a,b":*)']);
    },
  );

  it.each(["allow", "ask", "deny"] as const)(
    "records a %s rule longer than the widget's label cap, whole",
    (rules) => {
      const { current } = mount(kind);
      // TagListField's 512 is sized for the LABEL-shaped lists it also
      // serves; a rule is a literal, and a cut one is a different, narrower
      // rule entered with no message.
      const rule = `Bash(git commit -m "${"x".repeat(520)}":*)`;
      expect(rule.length).toBeGreaterThan(512);
      addRule(rules, rule);
      const stored = current()[rules] ?? [];
      // Asserted as a length first: a 500-character diff of two near-equal
      // strings says nothing, "expected 512 to be 544" names the cut.
      expect(stored[0]?.length ?? 0).toBe(rule.length);
      expect(stored).toEqual([rule]);
    },
  );

  it.each(["Deny", "DENY", "ask ", " off"])(
    "shows the legal mode spelling %j as the mode it is, not as inherit",
    (spelling) => {
      mount(kind, { permission: spelling });
      expect((screen.getByLabelText(/^Permission/) as HTMLSelectElement).value).toBe(
        spelling.trim().toLowerCase(),
      );
    },
  );

  it("leaves an absent mode on the inherit option", () => {
    mount(kind);
    // C110 accepts the mode case-insensitively and trimmed, and the writer
    // round-trips it verbatim — so this document is legal and draws no
    // diagnostic. Read raw, the select fell through to "-- inherit workflow --"
    // and told the author a hard-denying node inherited its workflow. The
    // normalisation must not swallow the genuinely empty case.
    expect((screen.getByLabelText(/^Permission/) as HTMLSelectElement).value).toBe("");
  });

  it("says what each gate mode actually does to an unmatched call", () => {
    mount(kind);
    const text = fieldHelp(/^Permission/);
    // The gate's own subject, stated first: the node's LLM tool calls.
    expect(text).toMatch(/Tool-permission gate for this node/);
    // The mode decides the default for what NOTHING matched — described as
    // rule-triggered, `ask` reads as permissive and a headless node stalls on
    // the first unlisted call, `deny` reads as a blocklist and the node can
    // run nothing.
    expect(text).toMatch(/ask PAUSES it for a human/);
    expect(text).toMatch(/deny BLOCKS it with no pause/);
    // …and an `ask:` rule pauses under `deny` too: Evaluate matches the ask
    // list before it ever reaches the mode default, so "deny = the headless
    // boundary" on its own sends a cron run into a pause nobody will answer.
    expect(text).toMatch(/ask: pauses \(yes, under deny too\)/);
    // …and the one hole in that, stated because a security control's help
    // must not read stronger than the gate: an ask naming just the tool
    // (`Bash`) loses to an allow naming one exact call (`Bash(git diff)`),
    // in both armed modes — pkg/backend/permission,
    // TestEvaluate_TheBareAskAllowHoleIsTheSameInBothArmedModes.
    expect(text).toMatch(
      /An ask that names just the tool is the one rule an allow can beat, and only by naming that exact call/,
    );
    // …and `off` is not the neutral choice: it outranks ITERION_PERMISSION.
    expect(text).toMatch(/OVERRIDES ITERION_PERMISSION/);
    // …and the level this one inherits from is reachable: the workflow's
    // mode and lists live in a panel this form can never share the screen
    // with (Inspector dispatches a selected node to InspectorNode, and only
    // InspectorEmpty renders the Workflow tab), so the hint names it.
    expect(text).toMatch(/edited in the inspector's Workflow tab/);
  });

  it("says a node list replaces the workflow's, on each of the three", () => {
    mount(kind);
    // One help per kind, each reached through its own labelled row — so a
    // kind rendered without help names itself, and every claim below
    // reddens as an assertion instead of dying on the locator.
    for (const label of Object.values(RULE_LABELS)) {
      const text = ruleHelp(label);
      expect(text).toMatch(/REPLACES the workflow's list of this kind/);
      expect(text).toMatch(/never a union/);
      expect(text).toMatch(/independently per kind/);
      // "Restate anything from the workflow's list you still want" is only
      // actionable if the author can find that list: name the panel.
      expect(text).toMatch(/edited in the inspector's Workflow tab/);
    }
  });
});
