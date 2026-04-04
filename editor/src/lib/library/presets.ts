import type { LibraryItem } from "./types";

export const PRESET_ITEMS: LibraryItem[] = [
  // ── Agents ──────────────────────────────────────────────
  {
    id: "builtin:code-reviewer",
    name: "Code Reviewer",
    description: "Agent that reviews code changes and provides detailed feedback",
    category: "agent",
    tags: ["code", "review", "quality"],
    builtin: true,
    template: {
      node: {
        kind: "agent",
        data: {
          model: "${ANTHROPIC_MODEL}",
          delegate: "claude_code",
          session: "fresh",
        },
      },
      schemas: [
        {
          name: "code_review_input",
          fields: [
            { name: "code", type: "string" },
            { name: "context", type: "string" },
            { name: "instructions", type: "string" },
          ],
        },
        {
          name: "code_review_output",
          fields: [
            { name: "verdict", type: "string", enum_values: ["approved", "needs_revision", "rejected"] },
            { name: "feedback", type: "string" },
            { name: "suggestions", type: "json" },
          ],
        },
      ],
      prompts: [
        {
          name: "code_review_system",
          body: "You are a senior code reviewer. Analyze the provided code for correctness, performance, security, and maintainability. Provide actionable feedback.",
        },
        {
          name: "code_review_user",
          body: "Review the following code:\n\n{{input.code}}\n\nContext: {{input.context}}\nInstructions: {{input.instructions}}",
        },
      ],
    },
  },
  {
    id: "builtin:planner",
    name: "Planner",
    description: "Agent that breaks down a task into a structured plan",
    category: "agent",
    tags: ["plan", "decomposition", "strategy"],
    builtin: true,
    template: {
      node: {
        kind: "agent",
        data: {
          model: "${ANTHROPIC_MODEL}",
          session: "fresh",
        },
      },
      schemas: [
        {
          name: "plan_input",
          fields: [
            { name: "task", type: "string" },
            { name: "constraints", type: "string" },
          ],
        },
        {
          name: "plan_output",
          fields: [
            { name: "plan", type: "json" },
            { name: "summary", type: "string" },
          ],
        },
      ],
      prompts: [
        {
          name: "planner_system",
          body: "You are a planning agent. Break down the given task into clear, actionable steps. Consider dependencies between steps and potential risks.",
        },
        {
          name: "planner_user",
          body: "Task: {{input.task}}\n\nConstraints: {{input.constraints}}\n\nCreate a detailed plan with ordered steps.",
        },
      ],
    },
  },
  {
    id: "builtin:implementer",
    name: "Implementer",
    description: "Coding agent with tool access for implementing changes",
    category: "agent",
    tags: ["code", "implement", "tools"],
    builtin: true,
    template: {
      node: {
        kind: "agent",
        data: {
          model: "${ANTHROPIC_MODEL}",
          delegate: "claude_code",
          session: "inherit",
          tools: ["Read", "Edit", "Write", "Bash", "Glob", "Grep"],
        },
      },
      schemas: [
        {
          name: "implement_input",
          fields: [
            { name: "task", type: "string" },
            { name: "plan", type: "string" },
            { name: "workspace", type: "string" },
          ],
        },
        {
          name: "implement_output",
          fields: [
            { name: "result", type: "string" },
            { name: "files_changed", type: "json" },
          ],
        },
      ],
      prompts: [
        {
          name: "implementer_system",
          body: "You are a skilled software engineer. Implement the given task following the provided plan. Write clean, tested code.",
        },
        {
          name: "implementer_user",
          body: "Task: {{input.task}}\n\nPlan:\n{{input.plan}}\n\nWorkspace: {{input.workspace}}",
        },
      ],
    },
  },

  // ── Judges ──────────────────────────────────────────────
  {
    id: "builtin:approval-judge",
    name: "Approval Judge",
    description: "Evaluator that produces an approved/rejected verdict with reasoning",
    category: "judge",
    tags: ["review", "verdict", "quality"],
    builtin: true,
    template: {
      node: {
        kind: "judge",
        data: {
          model: "${ANTHROPIC_MODEL}",
          session: "fresh",
        },
      },
      schemas: [
        {
          name: "judge_input",
          fields: [
            { name: "content", type: "string" },
            { name: "criteria", type: "string" },
          ],
        },
        {
          name: "judge_verdict",
          fields: [
            { name: "verdict", type: "string", enum_values: ["approved", "rejected"] },
            { name: "reasoning", type: "string" },
            { name: "score", type: "float" },
          ],
        },
      ],
      prompts: [
        {
          name: "judge_system",
          body: "You are a strict evaluator. Assess the provided content against the given criteria. Be thorough and objective in your reasoning.",
        },
        {
          name: "judge_user",
          body: "Evaluate the following:\n\n{{input.content}}\n\nCriteria: {{input.criteria}}",
        },
      ],
    },
  },

  // ── Routers ─────────────────────────────────────────────
  {
    id: "builtin:fan-out-router",
    name: "Fan-out Router",
    description: "Routes to all targets in parallel",
    category: "router",
    tags: ["parallel", "fan-out", "split"],
    builtin: true,
    template: {
      node: { kind: "router", data: { mode: "fan_out_all" } },
    },
  },
  {
    id: "builtin:condition-router",
    name: "Condition Router",
    description: "Routes based on a boolean condition from upstream output",
    category: "router",
    tags: ["condition", "branch", "if-else"],
    builtin: true,
    template: {
      node: { kind: "router", data: { mode: "condition" } },
    },
  },

  // ── Humans ──────────────────────────────────────────────
  {
    id: "builtin:human-review-gate",
    name: "Human Review Gate",
    description: "Pauses workflow for human review and approval",
    category: "human",
    tags: ["review", "approval", "gate", "pause"],
    builtin: true,
    template: {
      node: {
        kind: "human",
        data: {
          mode: "pause_until_answers",
        },
      },
      schemas: [
        {
          name: "review_request",
          fields: [
            { name: "content", type: "string" },
            { name: "context", type: "string" },
          ],
        },
        {
          name: "review_response",
          fields: [
            { name: "approved", type: "bool" },
            { name: "feedback", type: "string" },
          ],
        },
      ],
      prompts: [
        {
          name: "human_review_instructions",
          body: "Please review the following content and provide your approval or feedback.\n\nContent: {{input.content}}\nContext: {{input.context}}",
        },
      ],
    },
  },

  // ── Tools ───────────────────────────────────────────────
  {
    id: "builtin:shell-command",
    name: "Shell Command",
    description: "Runs a shell command and captures output",
    category: "tool",
    tags: ["shell", "command", "exec"],
    builtin: true,
    template: {
      node: {
        kind: "tool",
        data: { command: "echo 'hello'" },
      },
      schemas: [
        {
          name: "command_output",
          fields: [
            { name: "stdout", type: "string" },
            { name: "exit_code", type: "int" },
          ],
        },
      ],
    },
  },

  // ── Primitive: Schemas ──────────────────────────────────
  {
    id: "builtin:schema-verdict",
    name: "Review Verdict",
    description: "Schema with verdict (approved/rejected/needs_revision), reasoning, and score",
    category: "schema",
    tags: ["review", "verdict", "judge"],
    builtin: true,
    template: {
      schemas: [
        {
          name: "review_verdict",
          fields: [
            { name: "verdict", type: "string", enum_values: ["approved", "rejected", "needs_revision"] },
            { name: "reasoning", type: "string" },
            { name: "score", type: "float" },
          ],
        },
      ],
    },
  },
  {
    id: "builtin:schema-task-io",
    name: "Task I/O",
    description: "Generic input/output pair for task-based nodes",
    category: "schema",
    tags: ["task", "generic", "io"],
    builtin: true,
    template: {
      schemas: [
        {
          name: "task_input",
          fields: [
            { name: "task", type: "string" },
            { name: "context", type: "string" },
          ],
        },
        {
          name: "task_output",
          fields: [
            { name: "result", type: "string" },
            { name: "metadata", type: "json" },
          ],
        },
      ],
    },
  },

  // ── Primitive: Prompts ──────────────────────────────────
  {
    id: "builtin:prompt-cot",
    name: "Chain of Thought",
    description: "System prompt that instructs step-by-step reasoning",
    category: "prompt",
    tags: ["cot", "reasoning", "system"],
    builtin: true,
    template: {
      prompts: [
        {
          name: "chain_of_thought_system",
          body: "Think through this problem step by step. For each step:\n1. State what you're considering\n2. Explain your reasoning\n3. Draw a conclusion\n\nAfter working through all steps, provide your final answer.",
        },
      ],
    },
  },
  {
    id: "builtin:prompt-structured-output",
    name: "Structured Output",
    description: "System prompt that enforces structured JSON responses",
    category: "prompt",
    tags: ["structured", "json", "format"],
    builtin: true,
    template: {
      prompts: [
        {
          name: "structured_output_system",
          body: "You must respond with valid JSON matching the required output schema. Do not include any text outside the JSON object. Be precise with field names and types.",
        },
      ],
    },
  },
];
