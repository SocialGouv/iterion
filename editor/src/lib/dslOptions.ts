// Shared select-option constants for the DSL forms. Centralised so a
// new value (e.g. a new backend, or a new InteractionMode) only needs
// to be added in one place. The form components import these instead
// of re-declaring the lists inline.

export interface SelectOption {
  value: string;
  label: string;
}

export const BACKEND_OPTIONS: SelectOption[] = [
  { value: "", label: "(unset · resolves to claw)" },
  { value: "claw", label: "claw" },
  { value: "claude_code", label: "claude_code" },
  { value: "codex", label: "codex" },
];

export const BACKEND_HELP =
  "Execution backend. Empty resolves to the workflow default (claw if not set). claude_code/codex shell out to the corresponding CLI; claw runs in-process.";

export const AWAIT_OPTIONS: SelectOption[] = [
  { value: "none", label: "none" },
  { value: "wait_all", label: "wait_all" },
  { value: "best_effort", label: "best_effort" },
];

export const AWAIT_HELP =
  "Implicit convergence: wait_all = wait for all incoming branches; best_effort = continue when available results are ready; none = no await (default).";

export const SESSION_OPTIONS: SelectOption[] = [
  { value: "fresh", label: "fresh" },
  { value: "inherit", label: "inherit" },
  { value: "fork", label: "fork" },
  { value: "artifacts_only", label: "artifacts_only" },
];

export const SESSION_HELP =
  "fresh = new context; inherit = reuse parent conversation; fork = non-consuming branch from parent session; artifacts_only = share published artifacts only.";

// Empty value means "inherit workflow default" on agent/judge forms,
// or "none" semantically. Forms decide the empty-label wording.
export const INTERACTION_OPTIONS: SelectOption[] = [
  { value: "none", label: "none" },
  { value: "human", label: "human" },
  { value: "llm", label: "llm" },
  { value: "llm_or_human", label: "llm_or_human (escalation)" },
];

export const INTERACTION_HELP =
  "How ask_user / human-in-the-loop requests are routed. llm_or_human asks the LLM first, escalates to a human if undecided.";

// Human nodes pre-select "human" by default and frame the choices in
// terms of what happens at the pause point. Same enum, different
// surface wording.
export const HUMAN_INTERACTION_OPTIONS: SelectOption[] = [
  { value: "human", label: "human (always pause)" },
  { value: "llm", label: "llm (auto-answer)" },
  { value: "llm_or_human", label: "llm_or_human (escalation)" },
];

export const HUMAN_INTERACTION_HELP =
  "human = always wait for input; llm = LLM generates answer (requires model); llm_or_human = LLM tries first, escalates to human if undecided.";

export const REASONING_EFFORT_OPTIONS: SelectOption[] = [
  { value: "", label: "(default)" },
  { value: "low", label: "low" },
  { value: "medium", label: "medium" },
  { value: "high", label: "high" },
  { value: "xhigh", label: "xhigh" },
  { value: "max", label: "max" },
];

export const REASONING_EFFORT_HELP =
  "For reasoning-capable models (e.g. o-series, claude-extended-thinking).";

export const WORKTREE_OPTIONS: SelectOption[] = [
  { value: "auto", label: "auto (per-run worktree)" },
  { value: "none", label: "none (run in place)" },
];

export const WORKTREE_HELP =
  "auto creates a per-run git worktree at <store-dir>/worktrees/<run-id>/ so the workflow can mutate the repo without touching your main working tree. Omit or set 'none' to run in place.";
