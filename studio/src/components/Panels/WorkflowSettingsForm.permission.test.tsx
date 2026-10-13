// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { IterDocument, WorkflowDecl } from "@/api/types";
import {
  DocumentStoreProvider,
  createDocumentStore,
  type DocumentStore,
} from "@/store/document";
import WorkflowSettingsForm from "./WorkflowSettingsForm";

// The workflow block owns the gate its agent and judge nodes inherit — and
// the one over the recovery agent a tool node's Verified Action dispatches at
// its agent rung, which declares nothing of its own
// (`permission:` + `allow:` / `ask:` / `deny:`, docs/permissions.md). What
// matters here is what a control WRITES into the document, because the save
// POSTs that document as JSON and the server unparses it into the `.bot`: a
// control that wrote `[]` for an emptied list makes the file unsaveable —
// `[]` arrives as a non-nil empty slice, `ir.SameProgram` reads a different
// program and the endpoint answers 422 with a message that names no field
// (pkg/server, TestSaveFileRefusesAnEmptiedPermissionRuleList).

afterEach(cleanup);

function seed(over: Partial<WorkflowDecl> = {}): IterDocument {
  return {
    prompts: [],
    schemas: [],
    agents: [],
    judges: [],
    routers: [],
    humans: [],
    tools: [],
    computes: [],
    workflows: [{ name: "main", entry: "start", edges: [], ...over }],
    comments: [],
  };
}

function mount(over: Partial<WorkflowDecl> = {}) {
  const store: DocumentStore = createDocumentStore();
  store.getState().setDocument(seed(over));
  render(
    <DocumentStoreProvider store={store}>
      <WorkflowSettingsForm />
    </DocumentStoreProvider>,
  );
  const current = () => store.getState().document!.workflows[0]!;
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

const RULE_LABELS = { allow: "Allow", ask: "Ask", deny: "Deny" } as const;

function addRule(kind: keyof typeof RULE_LABELS, rule: string) {
  const input = fieldInput(RULE_LABELS[kind]);
  fireEvent.change(input, { target: { value: rule } });
  fireEvent.keyDown(input, { key: "Enter" });
}

describe("WorkflowSettingsForm permission controls", () => {
  it("writes the gate mode, and drops it rather than declaring an empty one", () => {
    const { current } = mount();
    fireEvent.change(screen.getByLabelText(/^Permission/), { target: { value: "deny" } });
    expect(current().permission).toBe("deny");

    fireEvent.change(screen.getByLabelText(/^Permission/), { target: { value: "" } });
    // Absent is the shape `permission?: string` gives "not declared", and
    // the shape every reader of a WorkflowDecl tests for. The save accepts
    // `""` too (the server drops an empty mode, 200 with no `permission:`
    // line), so this guards the document's contract — unlike an emptied
    // rule LIST, where `[]` is refused outright.
    expect(current().permission).toBeUndefined();
  });

  it("offers exactly the three gate modes the DSL accepts, plus unset", () => {
    mount();
    const select = screen.getByLabelText(/^Permission/) as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.value)).toEqual(["", "off", "ask", "deny"]);
  });

  it.each(["allow", "ask", "deny"] as const)("writes the %s list", (rules) => {
    const { current } = mount();
    addRule(rules, "Bash(git diff:*)");
    expect(current()[rules]).toEqual(["Bash(git diff:*)"]);

    addRule(rules, "Read(**)");
    expect(current()[rules]).toEqual(["Bash(git diff:*)", "Read(**)"]);
  });

  it.each(["allow", "ask", "deny"] as const)(
    "drops the %s list rather than declaring it empty",
    (rules) => {
      const { current } = mount();
      addRule(rules, "Read(**)");
      expect(current()[rules]).toEqual(["Read(**)"]);

      fireEvent.click(screen.getByLabelText("Remove Read(**)"));
      expect(current()[rules]).toBeUndefined();
      // The save POSTs JSON.stringify(document): an emptied list must leave
      // no key at all, or the unparser writes the line back.
      expect(Object.keys(JSON.parse(JSON.stringify(current())))).not.toContain(rules);
    },
  );

  it("declares one kind without touching the others", () => {
    const { current } = mount();
    addRule("deny", "Bash");
    expect(current().deny).toEqual(["Bash"]);
    // The lists are independent per kind: declaring deny must not invent an
    // empty allow/ask, which a node would then "replace" for nothing.
    expect(current().allow).toBeUndefined();
    expect(current().ask).toBeUndefined();
  });

  it.each(["allow", "ask", "deny"] as const)(
    "records a %s rule that differs from another only in CASE",
    (rules) => {
      const { current } = mount();
      addRule(rules, "Read(.env*)");
      addRule(rules, "Read(.ENV*)");
      // The engine matches rule arguments case-sensitively: `Read(.env*)`
      // does not cover `.ENV`. The tag widget folds case by default, which
      // drops the second rule with no message — on a security control.
      expect(current()[rules]).toEqual(["Read(.env*)", "Read(.ENV*)"]);
    },
  );

  it.each(["allow", "ask", "deny"] as const)(
    "keeps a comma inside a %s rule instead of committing there",
    (rules) => {
      const { current } = mount();
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
      const { current } = mount();
      // TagListField's 512 is sized for the LABEL-shaped lists it also
      // serves; a rule is a literal, and a cut one is a different, narrower
      // rule entered with no message. The shape below loses its closing
      // `":*)` and at least draws C154, but a bare long tool name
      // (`mcp__…`) would truncate into a still-parseable rule that silently
      // matches less than the author wrote.
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
    "shows the legal mode spelling %j as the mode it is, not as unset",
    (spelling) => {
      mount({ permission: spelling });
      expect((screen.getByLabelText(/^Permission/) as HTMLSelectElement).value).toBe(
        spelling.trim().toLowerCase(),
      );
    },
  );

  it("leaves an absent mode on the unset option", () => {
    mount();
    // The normalisation that reads `Deny` as deny must not swallow the
    // genuinely empty case, which is the one that inherits.
    expect((screen.getByLabelText(/^Permission/) as HTMLSelectElement).value).toBe("");
  });

  it("says where the workflow mode sits in the precedence", () => {
    mount();
    const text = fieldHelp(/^Permission/);
    // A node's own `permission:` outranks this one, and an unset workflow
    // mode is not "off": ITERION_PERMISSION can still arm the gate.
    expect(text).toMatch(/A node's own permission: overrides it/);
    expect(text).toMatch(/Empty falls through to ITERION_PERMISSION, then off/);
    // …and a DECLARED `off` is not the same as unset: it outranks the
    // environment floor. Measured: with ITERION_PERMISSION=deny, an unset
    // workflow mode resolves to deny (source "env") and `off` resolves to
    // off (source "workflow") — pkg/server buildEffectiveSettings,
    // pkg/backend/model resolvePermissionPolicy. The workflow level is the
    // one an operator sets for a whole bot, so the control that can remove
    // a fleet-wide floor has to say so.
    expect(text).toMatch(
      /off = no gate, and it OVERRIDES ITERION_PERMISSION, so it is not the same as empty/,
    );
    // The mode decides what NOTHING matched — described as rule-triggered,
    // `ask` reads as permissive and a headless run stalls on the first
    // unlisted call, `deny` reads as a blocklist and nothing can run.
    expect(text).toMatch(/ask PAUSES it for a human/);
    expect(text).toMatch(/deny BLOCKS it with no pause/);
    // …and how the lists themselves are read, which the mode does not
    // change: an `ask:` rule pauses under `deny` too, with one hole — an ask
    // naming just the tool loses to an allow naming one exact call, in both
    // armed modes (pkg/backend/permission,
    // TestEvaluate_TheBareAskAllowHoleIsTheSameInBothArmedModes). This
    // workflow-level string carries the clause itself, so it is pinned here
    // and not only on the node form's twin.
    expect(text).toMatch(/ask: pauses \(yes, under deny too\)/);
    expect(text).toMatch(
      /An ask that names just the tool is the one rule an allow can beat, and only by naming that exact call/,
    );
  });

  it("says a node list replaces this one, on each of the three", () => {
    mount();
    // One help per kind, each reached through its own labelled row — so a
    // kind rendered without help names itself, and every claim below
    // reddens as an assertion instead of dying on the locator.
    for (const label of Object.values(RULE_LABELS)) {
      const text = ruleHelp(label);
      expect(text).toMatch(/A node's non-empty list of this kind REPLACES it/);
      expect(text).toMatch(/never a union/);
      expect(text).toMatch(/independently per kind/);
      // The kinds that inherit it, named: a rule list reaches a node through
      // the task the model executor builds, and that task is built for an
      // agent or a judge — plus the synthetic agent of a tool node's
      // recovery rung, which declares no list of any kind, so it inherits
      // all three (pkg/dsl/ir,
      // TestPermissionRulesReachNoGatedNodeMoreShapes/a_recovery_agent_rung_reads_all_three_workflow_lists).
      expect(text).toMatch(/every agent and judge node inherits it/);
      expect(text).toMatch(/so does the recovery agent of a tool node's Verified Action/);
    }
  });

  it("names the node kinds the gate reaches, and the one it does not", () => {
    mount();
    const text = fieldHelp(/^Permission/);
    // The gate governs LLM-issued tool calls: the policy is built from the
    // task of an agent or a judge (extractBackendFields accepts those two
    // kinds and refuses every other, pkg/backend/model), so a plain tool
    // node's `command:` runs ungated. Nothing else tells the author: C112
    // fires only when the TOOL NODE declares its own `permission:`, and a
    // workflow-level gate over a `tool ship: command: "rm -rf …"` compiles
    // with zero diagnostics. A security control that overstated its reach
    // would be believed.
    expect(text).toMatch(/agent and judge nodes/);
    expect(text).toMatch(/A plain tool node's own command: is NOT gated/);
    // The third subject, and the only one the compiler never names: rung 4
    // of a tool node's Verified Action dispatches a synthesised agent that
    // declares no mode and no lists, so this block alone gates it
    // (pkg/backend/model, TestWorkflowGateGovernsAToolNodesRecoveryAgent;
    // the tool node's own `permission:` is not copied onto it,
    // TestAToolNodesOwnPermissionNeverReachesItsRecoveryAgent). On a
    // workflow of nothing but such a node, a help text that stopped at
    // agent/judge would read as "this gate has no subject here" — and the
    // author would delete a live bound.
    expect(text).toMatch(/recovery agent a tool node's Verified Action dispatches at its agent rung/);
    expect(text).toMatch(/so this block is its only gate/);
  });

  it("carries the whole gate into what the save serialises", () => {
    const { current } = mount();
    fireEvent.change(screen.getByLabelText(/^Permission/), { target: { value: "ask" } });
    addRule("allow", "Read(**)");
    addRule("ask", "Bash(git push:*)");
    addRule("deny", "Bash(rm:*)");
    // The shape the save POSTs, and the server unparses into the `.bot`.
    expect(JSON.parse(JSON.stringify(current()))).toMatchObject({
      permission: "ask",
      allow: ["Read(**)"],
      ask: ["Bash(git push:*)"],
      deny: ["Bash(rm:*)"],
    });
  });
});
