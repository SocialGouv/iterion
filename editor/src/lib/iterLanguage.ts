import type { languages } from "monaco-editor";

export const ITER_LANGUAGE_ID = "iter";

export const iterLanguageConfig: languages.LanguageConfiguration = {
  comments: {
    lineComment: "##",
  },
  brackets: [
    ["{", "}"],
    ["[", "]"],
  ],
  autoClosingPairs: [
    { open: "{", close: "}" },
    { open: "[", close: "]" },
    { open: '"', close: '"' },
    { open: "{{", close: "}}" },
  ],
  surroundingPairs: [
    { open: "{", close: "}" },
    { open: "[", close: "]" },
    { open: '"', close: '"' },
  ],
};

export const iterTokensProvider: languages.IMonarchLanguage = {
  keywords: [
    // Top-level / declaration kinds
    "vars", "prompt", "schema", "agent", "judge", "router", "human", "tool", "compute", "workflow",
    "mcp_server",
    // Workflow + node fields
    "entry", "default_backend", "budget", "compaction", "mcp", "worktree",
    "model", "backend", "input", "output", "publish", "system", "user", "session",
    "tools", "tool_policy", "tool_max_steps", "max_tokens", "reasoning_effort", "readonly",
    "interaction", "interaction_prompt", "interaction_model",
    "instructions", "min_answers", "command", "expr",
    "mode", "multi", "await",
    // MCP server block
    "transport", "args", "url", "auth",
    "type", "auth_url", "token_url", "revoke_url", "client_id", "scopes",
    // MCP config block
    "autoload_project", "inherit", "servers", "disable",
    // Compaction block
    "threshold", "preserve_recent",
    // Edge syntax
    "when", "not", "as", "with", "enum",
  ],
  typeKeywords: [
    "string", "bool", "int", "float", "json", "string[]",
  ],
  valueKeywords: [
    // Session
    "fresh", "inherit", "fork", "artifacts_only",
    // Router mode
    "fan_out_all", "condition", "round_robin",
    // Await
    "wait_all", "best_effort", "none",
    // Worktree
    "auto",
    // Interaction (replaces the legacy human "mode" values
    // pause_until_answers / auto_answer / auto_or_pause)
    "human", "llm", "llm_or_human",
    // Reasoning effort
    "low", "medium", "high", "xhigh", "max",
    // MCP transport
    "stdio", "http", "sse",
    // OAuth
    "oauth2",
    "true", "false",
  ],
  builtinNodes: ["done", "fail"],
  budgetKeys: [
    "max_parallel_branches", "max_duration", "max_cost_usd", "max_tokens", "max_iterations",
  ],

  tokenizer: {
    root: [
      // Comments
      [/##.*$/, "comment"],

      // Template expressions {{...}}
      [/\{\{/, { token: "delimiter.template", next: "@template" }],

      // Env var references ${...}
      [/\$\{[^}]+\}/, "variable"],

      // Strings
      [/"/, { token: "string.quote", next: "@string" }],

      // Arrow operator
      [/->/, "operator"],

      // Numbers
      [/\b\d+(\.\d+)?\b/, "number"],

      // Colon after identifiers (field definitions)
      [/:/, "delimiter"],

      // Keywords and identifiers
      [/[a-zA-Z_]\w*/, {
        cases: {
          "@keywords": "keyword",
          "@typeKeywords": "type",
          "@valueKeywords": "constant",
          "@builtinNodes": "type.builtin",
          "@budgetKeys": "keyword",
          "@default": "identifier",
        },
      }],

      // Brackets
      [/[{}[\]]/, "delimiter.bracket"],

      // Whitespace
      [/\s+/, "white"],
    ],

    template: [
      [/\}\}/, { token: "delimiter.template", next: "@pop" }],
      [/[^}]+/, "variable.template"],
    ],

    string: [
      [/\{\{/, { token: "delimiter.template", next: "@stringTemplate" }],
      [/\$\{[^}]+\}/, "variable"],
      [/[^"\\{$]+/, "string"],
      [/\\./, "string.escape"],
      [/"/, { token: "string.quote", next: "@pop" }],
    ],

    stringTemplate: [
      [/\}\}/, { token: "delimiter.template", next: "@pop" }],
      [/[^}]+/, "variable.template"],
    ],
  },
};
