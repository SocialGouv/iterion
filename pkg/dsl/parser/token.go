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
	TokenEntry
	TokenMCP
	TokenBudget
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
	TokenToolMaxSteps
	TokenReasoningEffort
	TokenMode
	TokenStrategy
	TokenRequire
	TokenInstructions
	TokenCommand
	TokenArgs
	TokenURL
	TokenAuth
	TokenReadonly
	TokenBackend
	TokenDefaultBackend
	TokenInteraction
	TokenInteractionPrompt
	TokenInteractionModel
	TokenAwait
	TokenWhen
	TokenNot
	TokenAs
	TokenWith
	TokenEnum
	// Session modes
	TokenFresh
	TokenInherit
	TokenArtifactsOnly
	TokenFork
	// Router modes
	TokenFanOutAll
	TokenCondition
	TokenRoundRobin
	TokenLLM
	// Router properties
	TokenMulti
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
	// Worktree
	TokenWorktree
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

	TokenVars:              "vars",
	TokenMCPServer:         "mcp_server",
	TokenPrompt:            "prompt",
	TokenSchema:            "schema",
	TokenAgent:             "agent",
	TokenJudge:             "judge",
	TokenRouter:            "router",
	TokenJoin:              "join",
	TokenHuman:             "human",
	TokenTool:              "tool",
	TokenWorkflow:          "workflow",
	TokenCompute:           "compute",
	TokenEntry:             "entry",
	TokenMCP:               "mcp",
	TokenBudget:            "budget",
	TokenTransport:         "transport",
	TokenServers:           "servers",
	TokenDisable:           "disable",
	TokenAutoloadProject:   "autoload_project",
	TokenModel:             "model",
	TokenInput:             "input",
	TokenOutput:            "output",
	TokenPublish:           "publish",
	TokenSystem:            "system",
	TokenUser:              "user",
	TokenSession:           "session",
	TokenTools:             "tools",
	TokenToolPolicy:        "tool_policy",
	TokenToolMaxSteps:      "tool_max_steps",
	TokenReasoningEffort:   "reasoning_effort",
	TokenMode:              "mode",
	TokenStrategy:          "strategy",
	TokenRequire:           "require",
	TokenInstructions:      "instructions",
	TokenCommand:           "command",
	TokenArgs:              "args",
	TokenURL:               "url",
	TokenReadonly:          "readonly",
	TokenBackend:           "backend",
	TokenDefaultBackend:    "default_backend",
	TokenInteraction:       "interaction",
	TokenInteractionPrompt: "interaction_prompt",
	TokenInteractionModel:  "interaction_model",
	TokenAwait:             "await",
	TokenWhen:              "when",
	TokenNot:               "not",
	TokenAs:                "as",
	TokenWith:              "with",
	TokenEnum:              "enum",
	TokenFresh:             "fresh",
	TokenInherit:           "inherit",
	TokenArtifactsOnly:     "artifacts_only",
	TokenFork:              "fork",
	TokenFanOutAll:         "fan_out_all",
	TokenCondition:         "condition",
	TokenRoundRobin:        "round_robin",
	TokenLLM:               "llm",
	TokenMulti:             "multi",
	TokenWaitAll:           "wait_all",
	TokenBestEffort:        "best_effort",
	TokenTrue:              "true",
	TokenFalse:             "false",
	TokenTypeString:        "string",
	TokenTypeBool:          "bool",
	TokenTypeInt:           "int",
	TokenTypeFloat:         "float",
	TokenTypeJSON:          "json",
	TokenTypeStringArray:   "string[]",

	TokenMaxParallelBranches: "max_parallel_branches",
	TokenMaxDuration:         "max_duration",
	TokenMaxCostUSD:          "max_cost_usd",
	TokenMaxTokens:           "max_tokens",
	TokenMaxIterations:       "max_iterations",

	TokenCompaction:     "compaction",
	TokenThreshold:      "threshold",
	TokenPreserveRecent: "preserve_recent",
	TokenWorktree:       "worktree",

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
	"entry":                 TokenEntry,
	"mcp":                   TokenMCP,
	"budget":                TokenBudget,
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
	"tool_max_steps":        TokenToolMaxSteps,
	"reasoning_effort":      TokenReasoningEffort,
	"mode":                  TokenMode,
	"strategy":              TokenStrategy,
	"require":               TokenRequire,
	"instructions":          TokenInstructions,
	"command":               TokenCommand,
	"args":                  TokenArgs,
	"url":                   TokenURL,
	"auth":                  TokenAuth,
	"readonly":              TokenReadonly,
	"backend":               TokenBackend,
	"default_backend":       TokenDefaultBackend,
	"interaction":           TokenInteraction,
	"interaction_prompt":    TokenInteractionPrompt,
	"interaction_model":     TokenInteractionModel,
	"await":                 TokenAwait,
	"when":                  TokenWhen,
	"not":                   TokenNot,
	"as":                    TokenAs,
	"with":                  TokenWith,
	"enum":                  TokenEnum,
	"fresh":                 TokenFresh,
	"inherit":               TokenInherit,
	"artifacts_only":        TokenArtifactsOnly,
	"fork":                  TokenFork,
	"fan_out_all":           TokenFanOutAll,
	"condition":             TokenCondition,
	"round_robin":           TokenRoundRobin,
	"llm":                   TokenLLM,
	"multi":                 TokenMulti,
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
	"worktree":              TokenWorktree,
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

func (t Token) String() string {
	if t.Value != "" {
		return fmt.Sprintf("%s(%q)@%d:%d", t.Type, t.Value, t.Line, t.Column)
	}
	return fmt.Sprintf("%s@%d:%d", t.Type, t.Line, t.Column)
}
