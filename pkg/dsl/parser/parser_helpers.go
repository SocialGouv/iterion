package parser

import (
	"fmt"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// ---- shared helpers ----

func (p *parser) parseReasoningEffort() string {
	p.expect(TokenColon)
	t := p.next()
	// Quoted string form: env-overridable, e.g. "${VIBE_EFFORT:-max}".
	// Stored as-is; resolved + validated at runtime.
	if t.Type == TokenString {
		return t.Value
	}
	value := tokenAsIdent(t)
	switch value {
	case "low", "medium", "high", "xhigh", "max", "ultracode":
		return value
	default:
		p.addError(DiagInvalidValue, t, "expected reasoning effort (low, medium, high, xhigh, max, ultracode) or a quoted env-substituted string, got '"+t.Value+"'")
		return ""
	}
}

func (p *parser) parseSessionMode() ast.SessionMode {
	t := p.next()
	switch t.Type {
	case TokenFresh:
		return ast.SessionFresh
	case TokenInherit:
		return ast.SessionInherit
	case TokenInheritIfAvailable:
		return ast.SessionInheritIfAvailable
	case TokenArtifactsOnly:
		return ast.SessionArtifactsOnly
	case TokenFork:
		return ast.SessionFork
	default:
		p.addError(DiagInvalidValue, t, "expected session mode (fresh, inherit, inherit_if_available, fork, artifacts_only), got '"+t.Value+"'")
		return ast.SessionFresh
	}
}

// parseNeedsList parses a node's `needs:` value — either a single resource
// name (`needs: godot`) or a bracketed list (`needs: [godot, blender]`).
func (p *parser) parseNeedsList() []string {
	if p.peek().Type == TokenLBrack {
		return p.parseIdentList()
	}
	id := p.expectIdent()
	if id == "" {
		return nil
	}
	return []string{id}
}

func (p *parser) parseIdentList() []string {
	p.expect(TokenLBrack)
	var names []string
	if p.peek().Type == TokenRBrack { // empty list: [] (matches parseStringList)
		p.next()
		return names
	}
	t := p.next()
	id := tokenAsIdent(t)
	if id != "" {
		names = append(names, id)
	}
	for p.peek().Type == TokenComma {
		p.next() // consume ,
		t = p.next()
		id = tokenAsIdent(t)
		if id != "" {
			names = append(names, id)
		}
	}
	p.expect(TokenRBrack)
	return names
}

func (p *parser) parseStringList() []string {
	p.expect(TokenLBrack)
	var vals []string
	if p.peek().Type == TokenRBrack {
		p.next()
		return vals
	}
	vals = append(vals, p.expectString())
	for p.peek().Type == TokenComma {
		p.next()
		vals = append(vals, p.expectString())
	}
	p.expect(TokenRBrack)
	return vals
}

// parseToolList parses a bracketed list of tool references that may contain
// dotted qualified names (e.g. [git_diff, mcp.claude_code.delegate]).
func (p *parser) parseToolList() []string {
	p.expect(TokenLBrack)
	var names []string
	if p.peek().Type == TokenRBrack { // empty list: [] (matches parseStringList)
		p.next()
		return names
	}
	name := p.parseToolRef()
	if name != "" {
		names = append(names, name)
	}
	for p.peek().Type == TokenComma {
		p.next() // consume ,
		name = p.parseToolRef()
		if name != "" {
			names = append(names, name)
		}
	}
	p.expect(TokenRBrack)
	return names
}

// parseToolRef parses a single tool reference: IDENT { "." IDENT } or
// IDENT { "." IDENT } "." "*" for MCP server wildcards (e.g. mcp.claude_code.*).
func (p *parser) parseToolRef() string {
	t := p.next()
	id := tokenAsIdent(t)
	if id == "" {
		return ""
	}
	for p.peek().Type == TokenDot {
		p.next() // consume .
		if p.peek().Type == TokenStar {
			p.next() // consume *
			id += ".*"
			break
		}
		t = p.next()
		part := tokenAsIdent(t)
		if part == "" {
			break
		}
		id += "." + part
	}
	return id
}

func (p *parser) expectString() string {
	t := p.next()
	if t.Type == TokenString {
		return t.Value
	}
	p.addError(DiagExpectedToken, t, "expected string literal, got "+t.Type.String())
	return t.Value
}

func (p *parser) expectIdent() string {
	t := p.next()
	id := tokenAsIdent(t)
	if id != "" {
		return id
	}
	p.addError(DiagExpectedToken, t, "expected identifier, got "+t.Type.String())
	return t.Value
}

func (p *parser) expectInt() int {
	t := p.next()
	if t.Type == TokenInt {
		v, err := strconv.Atoi(t.Value)
		if err != nil {
			p.addError(DiagExpectedToken, t, fmt.Sprintf("invalid integer %q: %v", t.Value, err))
			return 0
		}
		return v
	}
	p.addError(DiagExpectedToken, t, "expected integer, got "+t.Type.String())
	return 0
}

func (p *parser) expectNumber() float64 {
	t := p.next()
	switch t.Type {
	case TokenInt, TokenFloat:
		v, err := strconv.ParseFloat(t.Value, 64)
		if err != nil {
			p.addError(DiagExpectedToken, t, fmt.Sprintf("invalid number %q: %v", t.Value, err))
			return 0
		}
		return v
	default:
		p.addError(DiagExpectedToken, t, "expected number, got "+t.Type.String())
		return 0
	}
}

func (p *parser) skipToNewline() {
	for {
		t := p.peek()
		if t.Type == TokenNewline || t.Type == TokenEOF || t.Type == TokenDedent {
			return
		}
		p.next()
	}
}

// tokenAsIdent returns the identifier string for a token.
// Keywords are also valid as identifiers in name positions.
func tokenAsIdent(t Token) string {
	if t.Type == TokenIdent {
		return t.Value
	}
	// Keywords can be used as identifiers (e.g., node named "input")
	if isKeywordToken(t.Type) {
		return t.Value
	}
	return ""
}

func isKeywordToken(tt TokenType) bool {
	switch tt {
	case TokenVars, TokenPresets, TokenMCPServer, TokenPrompt, TokenSchema, TokenAgent, TokenJudge,
		TokenRouter, TokenHuman, TokenTool, TokenCompute, TokenWorkflow,
		TokenJoin,
		TokenEntry, TokenMCP, TokenBudget, TokenTransport, TokenServers,
		TokenDisable, TokenAutoloadProject, TokenModel, TokenInput, TokenOutput,
		TokenPublish, TokenSystem, TokenUser, TokenSession, TokenTools, TokenToolPolicy,
		TokenCapabilities, TokenArtifactLabels, TokenToolMaxSteps, TokenReasoningEffort, TokenMode, TokenStrategy, TokenRequire,
		TokenInstructions, TokenCommand, TokenScript, TokenLanguage, TokenArgs, TokenURL,
		TokenAuth, TokenReadonly,
		TokenDefaultBackend,
		TokenInteraction, TokenInteractionPrompt, TokenInteractionModel,
		TokenBackend, TokenProvider, TokenBaseURL, TokenAPIKeyEnv, TokenAwait, TokenWhen, TokenNot, TokenAs,
		TokenWith, TokenEnum, TokenFresh, TokenInherit, TokenArtifactsOnly,
		TokenFork,
		TokenFanOutAll, TokenCondition, TokenRoundRobin, TokenLLM, TokenMulti,
		TokenWaitAll, TokenBestEffort,
		TokenTrue, TokenFalse,
		TokenTypeString, TokenTypeBool, TokenTypeInt, TokenTypeFloat,
		TokenTypeJSON, TokenTypeStringArray,
		TokenMaxParallelBranches, TokenMaxDuration, TokenMaxCostUSD,
		TokenMaxTokens, TokenMaxIterations,
		TokenCompaction, TokenThreshold, TokenPreserveRecent,
		TokenMemory, TokenEnabled, TokenScope, TokenAutoload, TokenRead, TokenWrite, TokenPreCompactInject,
		TokenWorktree,
		TokenCompress,
		TokenPermission, TokenAllow, TokenAsk, TokenDeny,
		TokenSandbox,
		TokenCursor, TokenCursors, TokenValues, TokenBands,
		TokenGroup, TokenUse, TokenSubbot,
		TokenAttachments, TokenTypeFile, TokenTypeImage,
		// secrets / sandbox-host-state / forge keywords usable as identifiers in name positions
		TokenSecrets, TokenInheritIfAvailable, TokenProjectRoot, TokenVisibility,
		TokenDone, TokenFail:
		return true
	}
	return false
}
