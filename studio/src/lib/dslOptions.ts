// Shared select-option constants for the DSL forms. The VALUE sets of the
// registry-enum-backed groups are derived from iterDsl.generated.ts
// (iterDslEnumValuesByProperty, regenerated from pkg/dsl/spec by `task
// dsl:gen`), so a new enum word rides the same dsl:check freshness gate as
// the Monaco keywords and dslOptions.test.ts reddens on drift. Labels and
// help text stay curated here. The groups WITHOUT a registry enum
// counterpart (backend, permission, fallback on, worktree) keep their hand
// lists — the reason is stated on each one.

import { iterDslEnumValuesByProperty } from "./iterDsl.generated";

export interface SelectOption {
  value: string;
  label: string;
}

// optionsFrom maps registry enum words to SelectOptions, with per-value
// label overrides where the bare word reads poorly in a dropdown.
const optionsFrom = (
  values: readonly string[],
  labels: Record<string, string> = {},
): SelectOption[] => values.map((value) => ({ value, label: labels[value] ?? value }));

// orderedFrom is optionsFrom with a curated display order: words the order
// lists come in that order, a registry word the order misses appends sorted
// (a new registry word still reaches the dropdown) — and dslOptions.test.ts
// pins the curated orders to cover the registry exactly, so that new word
// also reddens the suite until the curation catches up.
const orderedFrom = (
  values: readonly string[],
  order: readonly string[],
  labels: Record<string, string> = {},
): SelectOption[] => {
  const rank = new Map(order.map((value, i) => [value, i]));
  const sorted = [...values].sort((a, b) => {
    const ra = rank.get(a);
    const rb = rank.get(b);
    if (ra !== undefined && rb !== undefined) return ra - rb;
    if (ra !== undefined) return -1;
    if (rb !== undefined) return 1;
    return a < b ? -1 : a > b ? 1 : 0;
  });
  return optionsFrom(sorted, labels);
};

// No registry enum counterpart: `backend` is a String property
// (pkg/dsl/spec/spec.go pBackend) — a {{vars.x}} reference or ${VAR:-default}
// resolves at run time, so the accepted words live only in the doc string.
export const BACKEND_OPTIONS: SelectOption[] = [
  { value: "", label: "(unset · resolves to claw)" },
  { value: "claw", label: "claw" },
  { value: "claude_code", label: "claude_code" },
  { value: "pi", label: "pi" },
  { value: "kimi", label: "kimi" },
  { value: "grok", label: "grok" },
  { value: "codex", label: "codex" },
  { value: "opencode", label: "opencode" },
];

export const BACKEND_HELP =
  "Execution backend. Empty resolves to the workflow default (claw if not set). claw runs in-process; every other value shells out to that agent CLI. Pick pi/kimi/grok/opencode to reach a model claude_code cannot. pi supports iterion's permission gate, ask_user, board capabilities and mcp_server blocks through an embedded extension; kimi, grok and opencode run their own tool set, so those blocks do not apply to them. opencode cannot enforce the permission gate at all, so a gated node routed to it is refused at compile time.";

// "No await" is the ABSENT property, not a value — the registry enum knows
// only wait_all and best_effort, so the forms pass allowEmpty for the
// default instead of listing a `none` option (which the parser refuses).
export const AWAIT_OPTIONS: SelectOption[] = optionsFrom(iterDslEnumValuesByProperty.await);

export const AWAIT_HELP =
  "Implicit convergence: wait_all = wait for all incoming branches; best_effort = continue when available results are ready. Unset = no await (default).";

// Curated order: the default (fresh) first, then the inherit family.
const SESSION_ORDER = ["fresh", "inherit", "inherit_if_available", "fork", "artifacts_only", "persist"] as const;
export const SESSION_OPTIONS: SelectOption[] = orderedFrom(
  iterDslEnumValuesByProperty.session,
  SESSION_ORDER,
);

export const SESSION_HELP =
  "fresh = new context; inherit = reuse parent conversation; inherit_if_available = inherit, retried fresh once if the session cannot load; fork = non-consuming branch from parent session; artifacts_only = share published artifacts only; persist = resume this node's own last CLI conversation on re-entry (trunk nodes only).";

// Empty value means "inherit workflow default" on agent/judge forms,
// or "none" semantically. Forms decide the empty-label wording.
export const INTERACTION_OPTIONS: SelectOption[] = optionsFrom(
  iterDslEnumValuesByProperty.interaction,
  { llm_or_human: "llm_or_human (escalation)" },
);

export const INTERACTION_HELP =
  "How ask_user / human-in-the-loop requests are routed. llm_or_human asks the LLM first, escalates to a human if undecided; human_or_host lets the host application answer in the operator's place, whichever comes first; review opens a review gate; async (agent/judge only) posts non-blocking questions and keeps working.";

// Human nodes pre-select "human" by default and frame the choices in
// terms of what happens at the pause point — a curated SUBSET of the
// interaction enum. Only `async` is refused on a human node (C240);
// `review` and `human_or_host` are valid human-node modes (the human
// kind carries the full interaction + review-gate fields, spec.go)
// omitted here pending form support — so an existing
// `interaction: review` / `human_or_host` human node matches no option
// and the select displays its first entry, "human (always pause)".
// The test pins this exact subset: adding a mode (e.g. the C240-refused
// async) reddens.
const HUMAN_INTERACTION_MODES = ["human", "llm", "llm_or_human"] as const;
export const HUMAN_INTERACTION_OPTIONS: SelectOption[] = optionsFrom(
  iterDslEnumValuesByProperty.interaction.filter((v) =>
    (HUMAN_INTERACTION_MODES as readonly string[]).includes(v),
  ),
  {
    human: "human (always pause)",
    llm: "llm (auto-answer)",
    llm_or_human: "llm_or_human (escalation)",
  },
);

export const HUMAN_INTERACTION_HELP =
  "human = always wait for input; llm = LLM generates answer (requires model); llm_or_human = LLM tries first, escalates to human if undecided.";

// No registry enum counterpart: `permission` is declared with checked()
// (Ident form narrowed by the compiler, spec.go pPermission), so its words
// never reach iterDslEnumValuesByProperty.
// Node-level tool-permission gate (docs/permissions.md). The empty value is
// "inherit the workflow's mode", which is why the forms pass allowEmpty
// rather than listing a fourth option.
export const PERMISSION_OPTIONS: SelectOption[] = [
  { value: "off", label: "off (no gate)" },
  { value: "ask", label: "ask (pause for approval)" },
  { value: "deny", label: "deny (block)" },
];

// Every clause maps to a line of pkg/backend/permission/permission.go.
// Evaluate's order (:302-315) matches deny, then ask, then allow BEFORE it
// reaches "otherwise the mode default" — so the rules behave identically in
// both armed modes, and an `ask:` rule pauses even under `deny`. What the
// mode decides is the default for what nothing matched. And an explicit
// `off` is not the same as leaving the field empty: it wins over the
// workflow AND over ITERION_PERMISSION (`cmp.Or(override, node, workflow,
// env)`, executor_build_task.go:169).
export const PERMISSION_HELP =
  "Tool-permission gate for this node. Empty inherits: workflow → ITERION_PERMISSION → off. off = no gate, and it OVERRIDES ITERION_PERMISSION, so it is not the same as empty. The rules are read the same way in both armed modes — deny: always blocks, ask: always pauses (yes, under deny too), allow: approves what neither matched. The mode decides only what nothing matched: ask PAUSES it for a human, deny BLOCKS it with no pause.";

// The one thing the feature is easy to get backwards, so every rule-list
// control repeats it: a node list REPLACES, it never adds.
export const PERMISSION_RULES_HELP =
  "A non-empty list REPLACES the workflow's list of this kind — never a union, and independently per kind. Restate anything from the workflow's list of this kind you still want. Empty inherits it. Tool(pattern) syntax, e.g. Bash(git diff:*).";

// Curated order: ascending effort, the orchestration tier last.
const REASONING_EFFORT_ORDER = ["low", "medium", "high", "xhigh", "max", "ultracode"] as const;
export const REASONING_EFFORT_OPTIONS: SelectOption[] = [
  { value: "", label: "(default)" },
  ...orderedFrom(iterDslEnumValuesByProperty.reasoning_effort, REASONING_EFFORT_ORDER, {
    none: "none (no reasoning)",
    ultracode: "ultracode (xhigh + orchestration)",
  }),
];

export const REASONING_EFFORT_HELP =
  "For reasoning-capable models (e.g. o-series, claude-extended-thinking). " +
  "none disables reasoning on the models that carry it (GPT-6 Sol/Luna); elsewhere claw and claude_code clamp it to low, pi spells it off, codex's CLI refuses it on a model without it. " +
  "ultracode = xhigh + standing consent to orchestrate multi-agent workflows; reliable only on claude-opus-4-8.";

// No registry enum counterpart: a fallback route's `on:` is an IdentList
// (spec.go), not an Enum — a list of triggers, so its words are not
// exported as enum values.
export const FALLBACK_ON_OPTIONS: SelectOption[] = [
  { value: "usage_window", label: "usage_window" },
  { value: "unavailable", label: "unavailable" },
  { value: "transient_exhausted", label: "transient_exhausted" },
  { value: "auth", label: "auth" },
  { value: "any", label: "any" },
];

export const FALLBACKS_HELP =
  "Ordered alternative routes tried when this node's primary fails (subscription window, model unreachable). " +
  "A route that changes backend must pin its own model. Empty `on:` defaults to usage_window + unavailable. " +
  "Judges are never re-routed by the launch-level fallback; only this block applies.";

// No registry enum counterpart: `worktree` is declared with checked()
// (Ident form, spec.go), so its words never reach
// iterDslEnumValuesByProperty.
export const WORKTREE_OPTIONS: SelectOption[] = [
  { value: "auto", label: "auto (per-run worktree)" },
  { value: "none", label: "none (run in place)" },
];

export const WORKTREE_HELP =
  "auto creates a per-run git worktree at <store-dir>/worktrees/<run-id>/ so the workflow can mutate the repo without touching your main working tree. Omit or set 'none' to run in place.";
