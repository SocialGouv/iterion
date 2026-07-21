package parser

import "fmt"

// TokenType identifies the kind of a lexical token.
type TokenType int

const (
	// Special
	TokenEOF    TokenType = iota
	TokenError            // lexer error
	TokenIndent           // virtual: indentation increase
	TokenDedent           // virtual: indentation decrease

	// Literals
	TokenIdent      // identifier
	TokenString     // "..." string literal
	TokenInt        // integer literal
	TokenFloat      // float literal
	TokenPromptLine // raw prompt body line

	// Punctuation
	TokenColon   // :
	TokenArrow   // ->
	TokenEquals  // =
	TokenComma   // ,
	TokenLBrack  // [
	TokenRBrack  // ]
	TokenLBrace  // {
	TokenRBrace  // }
	TokenLParen  // (
	TokenRParen  // )
	TokenDot     // .
	TokenStar    // *
	TokenNewline // logical newline (non-blank)

	// Comment
	TokenComment // ## ...

	// Keywords (contextual — also valid as identifiers in some positions)
	TokenVars
	TokenPresets
	TokenAttachments
	TokenSecrets
	TokenMCPServer
	TokenPrompt
	TokenSchema
	TokenAgent
	TokenJudge
	TokenRouter
	TokenJoin
	TokenHuman
	TokenTool
	TokenWorkflow
	TokenCompute
	TokenEmit
	TokenWait
	TokenAwaitAnswers
	TokenEntry
	TokenMCP
	TokenBudget
	TokenResources
	TokenTransport
	TokenServers
	TokenDisable
	TokenAutoloadProject
	TokenModel
	TokenInput
	TokenOutput
	TokenPublish
	TokenSystem
	TokenUser
	TokenSession
	TokenTools
	TokenToolPolicy
	TokenCapabilities
	TokenSkills
	TokenArtifactLabels
	TokenToolMaxSteps
	TokenReasoningEffort
	TokenMode
	TokenStrategy
	TokenRequire
	TokenInstructions
	TokenCommand
	TokenScript
	TokenLanguage
	TokenArgs
	TokenURL
	TokenAuth
	TokenReadonly
	TokenFullAccess
	TokenImages
	TokenBackend
	TokenNeeds
	TokenDefaultBackend
	TokenProvider
	TokenInteraction
	TokenInteractionPrompt
	TokenInteractionModel
	TokenAwait
	TokenWhen
	TokenElse
	TokenNot
	TokenAs
	TokenWith
	TokenEnum
	// Session modes
	TokenFresh
	TokenInherit
	TokenInheritIfAvailable
	TokenArtifactsOnly
	TokenFork
	// Router modes
	TokenFanOutAll
	TokenFanOutEach
	TokenCondition
	TokenRoundRobin
	TokenLLM
	// Router properties
	TokenMulti
	TokenOver
	TokenKey
	TokenDependsOn
	// Join strategies
	TokenWaitAll
	TokenBestEffort
	// Booleans
	TokenTrue
	TokenFalse
	// Type keywords
	TokenTypeString
	TokenTypeBool
	TokenTypeInt
	TokenTypeFloat
	TokenTypeJSON
	TokenTypeStringArray
	// Attachment type keywords
	TokenTypeFile
	TokenTypeImage
	// Budget properties
	TokenMaxParallelBranches
	TokenMaxDuration
	TokenMaxCostUSD
	TokenMaxTokens
	TokenMaxIterations
	// Compaction block + properties
	TokenCompaction
	TokenThreshold
	TokenPreserveRecent
	// Memory block + properties (workspace memory, opt-in)
	TokenMemory
	TokenEnabled
	TokenScope
	TokenAutoload
	TokenRead
	TokenWrite
	TokenPreCompactInject
	TokenProjectRoot
	TokenVisibility
	// Worktree
	TokenWorktree
	// Compress output-compression mode: on|off|ultra
	TokenCompress
	// Permission gate: scalar mode + allow/ask/deny rule lists
	TokenPermission
	TokenAllow
	TokenAsk
	TokenDeny
	// Sandbox (Phase 0: simple "sandbox: ident" form for none|auto)
	TokenSandbox
	// Cursor declaration + activation block
	TokenCursor
	TokenCursors
	TokenValues
	TokenBands
	// Group declaration (reusable node-cluster macro) + use instantiation
	TokenGroup
	TokenUse
	// Subbot node (runs another .bot as a nested run)
	TokenSubbot
	// Supervisor declaration (concurrent node-watcher)
	TokenSupervisor
	// Terminal node names (reserved identifiers)
	TokenDone
	TokenFail
)

var tokenNames = map[TokenType]string{
	TokenEOF:    "EOF",
	TokenError:  "Error",
	TokenIndent: "INDENT",
	TokenDedent: "DEDENT",

	TokenIdent:      "Ident",
	TokenString:     "String",
	TokenInt:        "Int",
	TokenFloat:      "Float",
	TokenPromptLine: "PromptLine",

	TokenColon:   ":",
	TokenArrow:   "->",
	TokenEquals:  "=",
	TokenComma:   ",",
	TokenLBrack:  "[",
	TokenRBrack:  "]",
	TokenLBrace:  "{",
	TokenRBrace:  "}",
	TokenLParen:  "(",
	TokenRParen:  ")",
	TokenDot:     ".",
	TokenStar:    "*",
	TokenNewline: "Newline",
	TokenComment: "Comment",

	TokenVars:               "vars",
	TokenPresets:            "presets",
	TokenAttachments:        "attachments",
	TokenSecrets:            "secrets",
	TokenTypeFile:           "file",
	TokenTypeImage:          "image",
	TokenMCPServer:          "mcp_server",
	TokenPrompt:             "prompt",
	TokenSchema:             "schema",
	TokenAgent:              "agent",
	TokenJudge:              "judge",
	TokenRouter:             "router",
	TokenJoin:               "join",
	TokenHuman:              "human",
	TokenTool:               "tool",
	TokenWorkflow:           "workflow",
	TokenCompute:            "compute",
	TokenEmit:               "emit",
	TokenWait:               "wait",
	TokenAwaitAnswers:       "await_answers",
	TokenEntry:              "entry",
	TokenMCP:                "mcp",
	TokenBudget:             "budget",
	TokenResources:          "resources",
	TokenTransport:          "transport",
	TokenServers:            "servers",
	TokenDisable:            "disable",
	TokenAutoloadProject:    "autoload_project",
	TokenModel:              "model",
	TokenInput:              "input",
	TokenOutput:             "output",
	TokenPublish:            "publish",
	TokenSystem:             "system",
	TokenUser:               "user",
	TokenSession:            "session",
	TokenTools:              "tools",
	TokenToolPolicy:         "tool_policy",
	TokenCapabilities:       "capabilities",
	TokenSkills:             "skills",
	TokenArtifactLabels:     "artifact_labels",
	TokenToolMaxSteps:       "tool_max_steps",
	TokenReasoningEffort:    "reasoning_effort",
	TokenMode:               "mode",
	TokenStrategy:           "strategy",
	TokenRequire:            "require",
	TokenInstructions:       "instructions",
	TokenCommand:            "command",
	TokenScript:             "script",
	TokenLanguage:           "language",
	TokenArgs:               "args",
	TokenURL:                "url",
	TokenReadonly:           "readonly",
	TokenFullAccess:         "full_access",
	TokenImages:             "images",
	TokenBackend:            "backend",
	TokenNeeds:              "needs",
	TokenDefaultBackend:     "default_backend",
	TokenProvider:           "provider",
	TokenInteraction:        "interaction",
	TokenInteractionPrompt:  "interaction_prompt",
	TokenInteractionModel:   "interaction_model",
	TokenAwait:              "await",
	TokenWhen:               "when",
	TokenElse:               "else",
	TokenNot:                "not",
	TokenAs:                 "as",
	TokenWith:               "with",
	TokenEnum:               "enum",
	TokenFresh:              "fresh",
	TokenInherit:            "inherit",
	TokenInheritIfAvailable: "inherit_if_available",
	TokenArtifactsOnly:      "artifacts_only",
	TokenFork:               "fork",
	TokenFanOutAll:          "fan_out_all",
	TokenFanOutEach:         "fan_out_each",
	TokenCondition:          "condition",
	TokenRoundRobin:         "round_robin",
	TokenLLM:                "llm",
	TokenMulti:              "multi",
	TokenOver:               "over",
	TokenKey:                "key",
	TokenDependsOn:          "depends_on",
	TokenWaitAll:            "wait_all",
	TokenBestEffort:         "best_effort",
	TokenTrue:               "true",
	TokenFalse:              "false",
	TokenTypeString:         "string",
	TokenTypeBool:           "bool",
	TokenTypeInt:            "int",
	TokenTypeFloat:          "float",
	TokenTypeJSON:           "json",
	TokenTypeStringArray:    "string[]",

	TokenMaxParallelBranches: "max_parallel_branches",
	TokenMaxDuration:         "max_duration",
	TokenMaxCostUSD:          "max_cost_usd",
	TokenMaxTokens:           "max_tokens",
	TokenMaxIterations:       "max_iterations",

	TokenCompaction:       "compaction",
	TokenThreshold:        "threshold",
	TokenPreserveRecent:   "preserve_recent",
	TokenMemory:           "memory",
	TokenEnabled:          "enabled",
	TokenScope:            "scope",
	TokenAutoload:         "autoload",
	TokenRead:             "read",
	TokenWrite:            "write",
	TokenPreCompactInject: "pre_compact_inject",
	TokenProjectRoot:      "project_root",
	TokenVisibility:       "visibility",
	TokenWorktree:         "worktree",
	TokenCompress:         "compress",
	TokenPermission:       "permission",
	TokenAllow:            "allow",
	TokenAsk:              "ask",
	TokenDeny:             "deny",
	TokenSandbox:          "sandbox",
	TokenCursor:           "cursor",
	TokenGroup:            "group",
	TokenUse:              "use",
	TokenSubbot:           "subbot",
	TokenSupervisor:       "supervisor",
	TokenCursors:          "cursors",
	TokenValues:           "values",
	TokenBands:            "bands",

	TokenDone: "done",
	TokenFail: "fail",
}

func (t TokenType) String() string {
	if name, ok := tokenNames[t]; ok {
		return name
	}
	return fmt.Sprintf("TokenType(%d)", int(t))
}

// keywords maps keyword strings to their token types.
// The lexer uses this to distinguish keywords from plain identifiers.
var keywords = map[string]TokenType{
	"vars":                  TokenVars,
	"presets":               TokenPresets,
	"attachments":           TokenAttachments,
	"secrets":               TokenSecrets,
	"file":                  TokenTypeFile,
	"image":                 TokenTypeImage,
	"mcp_server":            TokenMCPServer,
	"prompt":                TokenPrompt,
	"schema":                TokenSchema,
	"agent":                 TokenAgent,
	"judge":                 TokenJudge,
	"router":                TokenRouter,
	"join":                  TokenJoin,
	"human":                 TokenHuman,
	"tool":                  TokenTool,
	"workflow":              TokenWorkflow,
	"compute":               TokenCompute,
	"emit":                  TokenEmit,
	"wait":                  TokenWait,
	"await_answers":         TokenAwaitAnswers,
	"entry":                 TokenEntry,
	"mcp":                   TokenMCP,
	"budget":                TokenBudget,
	"resources":             TokenResources,
	"transport":             TokenTransport,
	"servers":               TokenServers,
	"disable":               TokenDisable,
	"autoload_project":      TokenAutoloadProject,
	"model":                 TokenModel,
	"input":                 TokenInput,
	"output":                TokenOutput,
	"publish":               TokenPublish,
	"system":                TokenSystem,
	"user":                  TokenUser,
	"session":               TokenSession,
	"tools":                 TokenTools,
	"tool_policy":           TokenToolPolicy,
	"capabilities":          TokenCapabilities,
	"skills":                TokenSkills,
	"artifact_labels":       TokenArtifactLabels,
	"tool_max_steps":        TokenToolMaxSteps,
	"reasoning_effort":      TokenReasoningEffort,
	"mode":                  TokenMode,
	"strategy":              TokenStrategy,
	"require":               TokenRequire,
	"instructions":          TokenInstructions,
	"command":               TokenCommand,
	"script":                TokenScript,
	"language":              TokenLanguage,
	"args":                  TokenArgs,
	"url":                   TokenURL,
	"auth":                  TokenAuth,
	"readonly":              TokenReadonly,
	"full_access":           TokenFullAccess,
	"images":                TokenImages,
	"backend":               TokenBackend,
	"needs":                 TokenNeeds,
	"default_backend":       TokenDefaultBackend,
	"provider":              TokenProvider,
	"interaction":           TokenInteraction,
	"interaction_prompt":    TokenInteractionPrompt,
	"interaction_model":     TokenInteractionModel,
	"await":                 TokenAwait,
	"when":                  TokenWhen,
	"else":                  TokenElse,
	"not":                   TokenNot,
	"as":                    TokenAs,
	"with":                  TokenWith,
	"enum":                  TokenEnum,
	"fresh":                 TokenFresh,
	"inherit":               TokenInherit,
	"inherit_if_available":  TokenInheritIfAvailable,
	"artifacts_only":        TokenArtifactsOnly,
	"fork":                  TokenFork,
	"fan_out_all":           TokenFanOutAll,
	"fan_out_each":          TokenFanOutEach,
	"condition":             TokenCondition,
	"round_robin":           TokenRoundRobin,
	"llm":                   TokenLLM,
	"multi":                 TokenMulti,
	"over":                  TokenOver,
	"key":                   TokenKey,
	"depends_on":            TokenDependsOn,
	"wait_all":              TokenWaitAll,
	"best_effort":           TokenBestEffort,
	"true":                  TokenTrue,
	"false":                 TokenFalse,
	"string":                TokenTypeString,
	"bool":                  TokenTypeBool,
	"int":                   TokenTypeInt,
	"float":                 TokenTypeFloat,
	"json":                  TokenTypeJSON,
	"max_parallel_branches": TokenMaxParallelBranches,
	"max_duration":          TokenMaxDuration,
	"max_cost_usd":          TokenMaxCostUSD,
	"max_tokens":            TokenMaxTokens,
	"max_iterations":        TokenMaxIterations,
	"compaction":            TokenCompaction,
	"threshold":             TokenThreshold,
	"preserve_recent":       TokenPreserveRecent,
	"memory":                TokenMemory,
	"enabled":               TokenEnabled,
	"scope":                 TokenScope,
	"autoload":              TokenAutoload,
	"read":                  TokenRead,
	"write":                 TokenWrite,
	"pre_compact_inject":    TokenPreCompactInject,
	"project_root":          TokenProjectRoot,
	"visibility":            TokenVisibility,
	"worktree":              TokenWorktree,
	"compress":              TokenCompress,
	"permission":            TokenPermission,
	"allow":                 TokenAllow,
	"ask":                   TokenAsk,
	"deny":                  TokenDeny,
	"sandbox":               TokenSandbox,
	"cursor":                TokenCursor,
	"group":                 TokenGroup,
	"use":                   TokenUse,
	"subbot":                TokenSubbot,
	"supervisor":            TokenSupervisor,
	"cursors":               TokenCursors,
	"values":                TokenValues,
	"bands":                 TokenBands,
	"done":                  TokenDone,
	"fail":                  TokenFail,
}

// Token is a single lexical token produced by the lexer.
type Token struct {
	Type   TokenType
	Value  string // raw text of the token
	Line   int    // 1-based
	Column int    // 1-based
}
