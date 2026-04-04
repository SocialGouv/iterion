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
    "vars", "prompt", "schema", "agent", "judge", "router", "human", "tool", "workflow",
    "entry", "budget", "model", "input", "output", "publish", "system", "user", "session", "tools",
    "tool_max_steps", "mode", "await", "instructions", "command", "delegate",
    "when", "not", "as", "with", "enum",
  ],
  typeKeywords: [
    "string", "bool", "int", "float", "json", "string[]",
  ],
  valueKeywords: [
    "fresh", "inherit", "artifacts_only", "none",
    "fan_out_all", "condition",
    "wait_all", "best_effort",
    "pause_until_answers", "auto_answer", "auto_or_pause",
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
