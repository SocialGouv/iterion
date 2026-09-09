import type { languages } from "monaco-editor";

export const ITER_LANGUAGE_ID = "iter";

export const iterLanguageConfig: languages.LanguageConfiguration = {
  comments: {
    lineComment: "#",
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
    "low", "medium", "high", "xhigh", "max", "ultracode",
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
      // A prompt declaration: its body is TEXT on the following lines,
      // where `# Heading` is a heading, not a comment (the lexer keeps it as
      // prompt text). The header's indentation rides on the state name so
      // the body ends at the first line indented no deeper than the header —
      // a prompt inside a `group` is handled like a top-level one.
      [/^(\s*)(prompt)(\s+)([A-Za-z_]\w*)(\s*:)/, [
        "white", "keyword", "white", "identifier",
        { token: "delimiter", next: "@promptHeader.$1" },
      ]],

      // A block scalar (`command: |`): its body is a script on the following
      // lines, where `# note` is a shell comment inside the value.
      [/^(\s*)([A-Za-z_]\w*)(\s*:\s*)(\|[-+]?)(\s*)$/, [
        "white", "keyword", "delimiter",
        { token: "delimiter", next: "@blockScalar.$1" },
        "white",
      ]],

      // Raw strings: a backtick opens a shell command or a literal that a
      // `#` must not close — `echo "#1"` is a command, not a comment. Read
      // before the comment rule so the hash inside stays string-coloured.
      [/`/, { token: "string.quote", next: "@rawString" }],

      // Comments
      [/#.*$/, "comment"],

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

    // The rest of a `prompt <name>:` header line. On the following lines,
    // only a line indented STRICTLY deeper than the header ($S2 followed by
    // at least one more space) belongs to the body; any other non-blank line
    // — at the header's indent, shallower, or at column 0 — ends the
    // declaration and is re-read by the block grammar.
    promptHeader: [
      [/^(\s*)(?=\S)/, {
        cases: {
          "$1~$S2\\s+": { token: "white", switchTo: "@promptBody.$S2" },
          "@default": { token: "@rematch", next: "@pop" },
        },
      }],
      [/#.*$/, "comment"],
      [/\s+/, "white"],
      [/./, "white"],
    ],

    // A prompt body: text, with templates and env refs coloured, until a
    // line indented no deeper than the header.
    promptBody: [
      [/^(\s*)(?=\S)/, {
        cases: {
          "$1~$S2\\s+": "white",
          "@default": { token: "@rematch", next: "@pop" },
        },
      }],
      [/\{\{/, { token: "delimiter.template", next: "@stringTemplate" }],
      [/\$\{[^}]+\}/, "variable"],
      [/[^{$]+/, "string"],
      [/[{$]/, "string"],
    ],

    // A block scalar body: the same shape as a prompt body, ended by a line
    // indented no deeper than its key — the key may sit deeper than its
    // block's siblings (`recovery: / repair: / command: |`), so "shallower
    // than the key" must end it, not only "exactly the key's indent".
    blockScalar: [
      [/^(\s*)(?=\S)/, {
        cases: {
          "$1~$S2\\s+": "white",
          "@default": { token: "@rematch", next: "@pop" },
        },
      }],
      [/\{\{/, { token: "delimiter.template", next: "@stringTemplate" }],
      [/\$\{[^}]+\}/, "variable"],
      [/[^{$]+/, "string"],
      [/[{$]/, "string"],
    ],

    // A raw string has no escape and may span lines: only the closing
    // backtick ends it. Templates and env refs keep their colour inside it.
    rawString: [
      [/\{\{/, { token: "delimiter.template", next: "@stringTemplate" }],
      [/\$\{[^}]+\}/, "variable"],
      [/[^`{$]+/, "string"],
      [/[{$]/, "string"],
      [/`/, { token: "string.quote", next: "@pop" }],
    ],

    stringTemplate: [
      [/\}\}/, { token: "delimiter.template", next: "@pop" }],
      [/[^}]+/, "variable.template"],
    ],
  },
};
