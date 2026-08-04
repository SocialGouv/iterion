package parser

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// ---- agent ----

func (p *parser) parseAgentDecl() *ast.AgentDecl {
	start, name, ok := p.parseDeclHeader("agent")
	if !ok {
		return nil
	}

	ad := &ast.AgentDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
	}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseLLMProp(&ad.LLMDecl, t, "agent")
	}
	return ad
}

// parseLLMProp parses one property line shared by agent and judge nodes
// into the embedded ast.LLMDecl. The two declaration kinds are
// structurally identical, so a single switch serves both; kind ("agent"
// /"judge") only personalises the unknown-property diagnostic.
func (p *parser) parseLLMProp(d *ast.LLMDecl, propTok Token, kind string) {
	p.next() // consume property keyword
	switch propTok.Type {
	case TokenModel:
		p.expect(TokenColon)
		d.Model = p.expectString()
	case TokenInput:
		p.expect(TokenColon)
		d.Input = p.expectIdent()
	case TokenOutput:
		p.expect(TokenColon)
		d.Output = p.expectIdent()
	case TokenPublish:
		p.expect(TokenColon)
		d.Publish = p.expectIdent()
	case TokenArtifactLabels:
		p.expect(TokenColon)
		d.ArtifactLabels = p.parseToolList()
	case TokenSystem:
		p.expect(TokenColon)
		d.System = p.expectIdent()
	case TokenUser:
		p.expect(TokenColon)
		d.User = p.expectIdent()
	case TokenSession:
		p.expect(TokenColon)
		d.Session = p.parseSessionMode()
	case TokenTools:
		p.expect(TokenColon)
		d.Tools = p.parseToolList()
	case TokenToolPolicy:
		p.expect(TokenColon)
		d.ToolPolicy = p.parseToolList()
	case TokenCapabilities:
		p.expect(TokenColon)
		d.Capabilities = p.parseToolList()
	case TokenSkills:
		p.expect(TokenColon)
		d.Skills = p.parseSkillList()
	case TokenToolMaxSteps:
		p.expect(TokenColon)
		d.ToolMaxSteps = p.expectInt()
	case TokenMaxTokens:
		p.expect(TokenColon)
		d.MaxTokens = p.expectInt()
	case TokenReasoningEffort:
		d.ReasoningEffort = p.parseReasoningEffort()
	case TokenIdent:
		// `timeout:` and `description:` are not reserved keywords (the wait
		// node also parses them as bare idents), so match on the value here
		// rather than a token.
		switch propTok.Value {
		case "timeout":
			p.expect(TokenColon)
			d.Timeout = p.expectString()
		case "description":
			p.expect(TokenColon)
			d.Description = p.expectString()
		case "fallbacks":
			d.Fallbacks = p.parseFallbacksBlock(propTok)
		default:
			p.addError(DiagUnknownProperty, propTok, "unknown "+kind+" property '"+propTok.Value+"'")
			p.skipToNewline()
		}
	case TokenReadonly:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			d.Readonly = *v
		}
	case TokenFullAccess:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			d.FullAccess = *v
		}
	case TokenImages:
		p.expect(TokenColon)
		d.Images = p.parseStringList()
	case TokenMCP:
		p.backup()
		d.MCP = p.parseMCPConfigBlock()
	case TokenBackend:
		p.expect(TokenColon)
		d.Backend = p.expectString()
	case TokenCompress:
		p.expect(TokenColon)
		d.Compress = p.expectIdent()
	case TokenPermission:
		p.expect(TokenColon)
		d.Permission = p.expectIdent()
	case TokenNeeds:
		p.expect(TokenColon)
		d.Needs = p.parseNeedsList()
	case TokenProvider:
		p.expect(TokenColon)
		d.Provider = p.expectString()
	case TokenCommand:
		p.expect(TokenColon)
		d.Command = p.expectString()
	case TokenInteraction:
		p.expect(TokenColon)
		d.Interaction = p.parseInteractionMode()
	case TokenInteractionPrompt:
		p.expect(TokenColon)
		d.InteractionPrompt = p.expectIdent()
	case TokenInteractionModel:
		p.expect(TokenColon)
		d.InteractionModel = p.expectString()
	case TokenAwait:
		p.expect(TokenColon)
		d.Await = p.parseAwaitMode()
	case TokenCompaction:
		p.backup()
		d.Compaction = p.parseCompactionBlock()
	case TokenMemory:
		p.backup()
		d.Memory = p.parseMemoryBlock()
	case TokenSandbox:
		p.backup()
		d.Sandbox = p.parseSandboxBlock()
	case TokenCursors:
		p.backup()
		d.Cursors = p.parseCursorsBlock()
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown "+kind+" property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

// ---- judge ----

func (p *parser) parseJudgeDecl() *ast.JudgeDecl {
	start, name, ok := p.parseDeclHeader("judge")
	if !ok {
		return nil
	}

	jd := &ast.JudgeDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
	}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseLLMProp(&jd.LLMDecl, t, "judge")
	}
	return jd
}

// ---- router ----

func (p *parser) parseRouterDecl() *ast.RouterDecl {
	start, name, ok := p.parseDeclHeader("router")
	if !ok {
		return nil
	}

	rd := &ast.RouterDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
	}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		switch t.Type {
		case TokenMode:
			p.next()
			p.expect(TokenColon)
			rd.Mode = p.parseRouterMode()
		case TokenModel:
			p.next()
			p.expect(TokenColon)
			rd.Model = p.expectString()
		case TokenBackend:
			p.next()
			p.expect(TokenColon)
			rd.Backend = p.expectString()
		case TokenProvider:
			p.next()
			p.expect(TokenColon)
			rd.Provider = p.expectString()
		case TokenSystem:
			p.next()
			p.expect(TokenColon)
			rd.System = p.expectIdent()
		case TokenUser:
			p.next()
			p.expect(TokenColon)
			rd.User = p.expectIdent()
		case TokenMulti:
			p.next()
			p.expect(TokenColon)
			bt := p.next()
			if bt.Type == TokenTrue {
				rd.Multi = true
			} else if bt.Type != TokenFalse {
				p.addError(DiagInvalidValue, bt, "expected true or false for 'multi'")
			}
		case TokenOver:
			p.next()
			p.expect(TokenColon)
			rd.Over = p.expectString()
		case TokenAs:
			p.next()
			p.expect(TokenColon)
			rd.As = p.expectIdent()
		case TokenKey:
			p.next()
			p.expect(TokenColon)
			rd.Key = p.expectIdent()
		case TokenDependsOn:
			p.next()
			p.expect(TokenColon)
			rd.DependsOn = p.expectIdent()
		case TokenNeeds:
			p.next()
			p.expect(TokenColon)
			rd.Needs = p.parseNeedsList()
		case TokenReasoningEffort:
			p.next()
			rd.ReasoningEffort = p.parseReasoningEffort()
		case TokenIdent:
			if t.Value == "description" {
				p.next()
				p.expect(TokenColon)
				rd.Description = p.expectString()
			} else {
				p.addError(DiagUnknownProperty, t, "unknown router property '"+t.Value+"'")
				p.next()
				p.skipToNewline()
			}
		default:
			p.addError(DiagUnknownProperty, t, "unknown router property '"+t.Value+"'")
			p.next()
			p.skipToNewline()
		}
		p.skipNewlines()
	}
	return rd
}

func (p *parser) parseRouterMode() ast.RouterMode {
	t := p.next()
	switch t.Type {
	case TokenFanOutAll:
		return ast.RouterFanOutAll
	case TokenFanOutEach:
		return ast.RouterFanOutEach
	case TokenCondition:
		return ast.RouterCondition
	case TokenRoundRobin:
		return ast.RouterRoundRobin
	case TokenLLM:
		return ast.RouterLLM
	default:
		p.addError(DiagInvalidValue, t, "expected router mode (fan_out_all, fan_out_each, condition, round_robin, llm), got '"+t.Value+"'")
		return ast.RouterFanOutAll
	}
}

// ---- await (convergence strategy) ----

func (p *parser) parseAwaitMode() ast.AwaitMode {
	t := p.next()
	switch t.Type {
	case TokenWaitAll:
		return ast.AwaitWaitAll
	case TokenBestEffort:
		return ast.AwaitBestEffort
	default:
		p.addError(DiagInvalidValue, t, "expected await mode (wait_all, best_effort), got '"+t.Value+"'")
		return ast.AwaitWaitAll
	}
}

// ---- human ----

func (p *parser) parseHumanDecl() *ast.HumanDecl {
	start, name, ok := p.parseDeclHeader("human")
	if !ok {
		return nil
	}

	hd := &ast.HumanDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
	}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseHumanProp(hd, t)
	}
	return hd
}

func (p *parser) parseHumanProp(hd *ast.HumanDecl, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenInput:
		p.expect(TokenColon)
		hd.Input = p.expectIdent()
	case TokenOutput:
		p.expect(TokenColon)
		hd.Output = p.expectIdent()
	case TokenPublish:
		p.expect(TokenColon)
		hd.Publish = p.expectIdent()
	case TokenArtifactLabels:
		p.expect(TokenColon)
		hd.ArtifactLabels = p.parseToolList()
	case TokenInstructions:
		p.expect(TokenColon)
		hd.Instructions = p.expectIdent()
	case TokenInteraction:
		p.expect(TokenColon)
		hd.Interaction = p.parseInteractionMode()
	case TokenInteractionPrompt:
		p.expect(TokenColon)
		hd.InteractionPrompt = p.expectIdent()
	case TokenInteractionModel:
		p.expect(TokenColon)
		hd.InteractionModel = p.expectString()
	case TokenModel:
		p.expect(TokenColon)
		hd.Model = p.expectString()
	case TokenSystem:
		p.expect(TokenColon)
		hd.System = p.expectIdent()
	case TokenAwait:
		p.expect(TokenColon)
		hd.Await = p.parseAwaitMode()
	case TokenIdent:
		switch propTok.Value {
		case "description":
			p.expect(TokenColon)
			hd.Description = p.expectString()
		case "min_answers":
			p.expect(TokenColon)
			hd.MinAnswers = p.expectInt()
		case "review_url":
			p.expect(TokenColon)
			hd.ReviewURL = p.expectString()
		case "posture":
			p.expect(TokenColon)
			hd.Posture = p.expectStringOrIdent()
		case "merge_strategy":
			p.expect(TokenColon)
			hd.MergeStrategy = p.expectStringOrIdent()
		case "merge_into":
			p.expect(TokenColon)
			hd.MergeInto = p.expectStringOrIdent()
		case "max_turns":
			p.expect(TokenColon)
			hd.MaxTurns = p.expectInt()
		default:
			p.addError(DiagUnknownProperty, propTok, "unknown human property '"+propTok.Value+"'")
			p.skipToNewline()
		}
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown human property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

func (p *parser) parseInteractionMode() ast.InteractionMode {
	t := p.next()
	switch t.Value {
	case "none":
		return ast.InteractionNone
	case "human":
		return ast.InteractionHuman
	case "llm":
		return ast.InteractionLLM
	case "llm_or_human":
		return ast.InteractionLLMOrHuman
	case "review":
		return ast.InteractionReview
	case "async":
		return ast.InteractionAsync
	default:
		p.addError(DiagInvalidValue, t, "expected interaction mode (none, human, llm, llm_or_human, review, async), got '"+t.Value+"'")
		return ast.InteractionNone
	}
}

// ---- tool node ----

func (p *parser) parseToolNodeDecl() *ast.ToolNodeDecl {
	start, name, ok := p.parseDeclHeader("tool")
	if !ok {
		return nil
	}

	td := &ast.ToolNodeDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
	}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseToolNodeProp(td, t)
	}
	return td
}

func (p *parser) parseToolNodeProp(td *ast.ToolNodeDecl, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenCommand:
		p.expect(TokenColon)
		td.Command = p.expectString()
	case TokenScript:
		p.expect(TokenColon)
		td.Script = p.expectString()
	case TokenLanguage:
		p.expect(TokenColon)
		td.Language = p.expectIdent()
	case TokenInput:
		p.expect(TokenColon)
		td.Input = p.expectIdent()
	case TokenOutput:
		p.expect(TokenColon)
		td.Output = p.expectIdent()
	case TokenPublish:
		p.expect(TokenColon)
		td.Publish = p.expectIdent()
	case TokenArtifactLabels:
		p.expect(TokenColon)
		td.ArtifactLabels = p.parseToolList()
	case TokenAwait:
		p.expect(TokenColon)
		td.Await = p.parseAwaitMode()
	case TokenSandbox:
		p.backup()
		td.Sandbox = p.parseSandboxBlock()
	case TokenCompress:
		p.expect(TokenColon)
		td.Compress = p.expectIdent()
	case TokenPermission:
		p.expect(TokenColon)
		td.Permission = p.expectIdent()
	case TokenNeeds:
		p.expect(TokenColon)
		td.Needs = p.parseNeedsList()
	case TokenIdent:
		// Verified Action quad (ADR-044). These property names are not
		// reserved keywords, so they arrive as plain identifiers (the
		// same convention compute's `expr` block uses).
		switch propTok.Value {
		case "description":
			p.expect(TokenColon)
			td.Description = p.expectString()
		case "goal":
			p.expect(TokenColon)
			td.Goal = p.expectString()
		case "postcondition":
			p.expect(TokenColon)
			td.Postcondition = p.expectString()
		case "policy":
			p.expect(TokenColon)
			td.Policy = p.expectIdent()
		case "recovery":
			td.Recovery = p.parseRecoveryBlock(propTok)
		case "parallel_safe":
			p.expect(TokenColon)
			if v := p.parseBool(); v != nil {
				td.ParallelSafe = *v
			}
		default:
			p.addError(DiagUnknownProperty, propTok, "unknown tool property '"+propTok.Value+"'")
			p.skipToNewline()
		}
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown tool property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

// parseRecoveryBlock parses the indented `recovery:` block of a Verified
// Action tool node (ADR-044). The `recovery` identifier has already been
// consumed by the parseToolNodeProp prologue; this consumes the colon and
// the indented body.
//
//	recovery:
//	  max_repair_attempts: 2
//	  max_agent_attempts: 1
//	  model: "anthropic/claude-sonnet-4-6"
//	  agent_tools: [bash, read_file]
func (p *parser) parseRecoveryBlock(propTok Token) *ast.RecoveryBlock {
	p.expect(TokenColon)
	rb := &ast.RecoveryBlock{Span: ast.Span{Start: p.pos(propTok)}}
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		// Empty block — recover gracefully.
		return rb
	}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		if t.Type != TokenIdent && !isKeywordToken(t.Type) {
			p.addError(DiagUnexpectedToken, t, "unexpected token '"+t.Value+"' in recovery block")
			p.next()
			p.skipToNewline()
			continue
		}
		name := t.Value
		p.next()
		p.expect(TokenColon)
		switch name {
		case "max_repair_attempts":
			rb.MaxRepairAttempts = p.expectInt()
		case "max_agent_attempts":
			rb.MaxAgentAttempts = p.expectInt()
		case "model":
			rb.Model = p.expectString()
		case "agent_tools":
			rb.AgentTools = p.parseToolList()
		default:
			p.addError(DiagUnknownProperty, t, "unknown recovery property '"+name+"'")
			p.skipToNewline()
		}
		p.skipNewlines()
	}
	return rb
}

// parseFallbacksBlock parses a node's `fallbacks:` block — an ordered
// set of NAMED alternative routes (ADR-087):
//
//	fallbacks:
//	  api:
//	    backend: "claw"
//	    model: "anthropic/claude-opus-5"
//	    on: [usage_window]
//
// Named entries rather than a YAML-style bullet list because the lexer
// has no sequence token (`-` is only ever the start of `->`), and
// because a name gives each route a stable id for the fall-through
// event and the run report. Order is the try order and is preserved.
//
// Reached from parseLLMProp's TokenIdent arm, so `fallbacks` stays a
// plain identifier — making it a reserved keyword would break any bot
// with a node, prompt or schema of that name.
func (p *parser) parseFallbacksBlock(propTok Token) []*ast.FallbackDecl {
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		// Empty block — recover gracefully; the IR validator reports it.
		return nil
	}
	var out []*ast.FallbackDecl
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		if fd := p.parseFallbackEntry(); fd != nil {
			out = append(out, fd)
		}
	}
	return out
}

// parseFallbackEntry parses one named route of a `fallbacks:` block.
func (p *parser) parseFallbackEntry() *ast.FallbackDecl {
	nameT := p.next()
	if nameT.Type != TokenIdent && !isKeywordToken(nameT.Type) {
		p.addError(DiagExpectedToken, nameT, "expected fallback name, got "+nameT.Type.String())
		p.skipToNewline()
		return nil
	}
	p.expect(TokenColon)
	fd := &ast.FallbackDecl{
		Name: nameT.Value,
		Span: ast.Span{Start: p.pos(nameT), End: p.pos(nameT)},
	}
	p.skipNewlines()
	if p.peek().Type != TokenIndent {
		// A name with no body declares nothing routable; the IR
		// validator reports it (C173) rather than the parser, so a
		// document built straight from JSON hits the same check.
		return fd
	}
	p.next() // consume indent
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		if t.Type != TokenIdent && !isKeywordToken(t.Type) {
			p.addError(DiagUnexpectedToken, t, "unexpected token in fallback block: "+t.Value)
			p.next()
			p.skipToNewline()
			continue
		}
		propName := t.Value
		p.next()
		p.expect(TokenColon)
		switch propName {
		case "backend":
			fd.Backend = p.expectString()
		case "model":
			fd.Model = p.expectString()
		case "provider":
			fd.Provider = p.expectString()
		case "on":
			fd.On = p.parseIdentList()
		case "metered":
			if v := p.parseBool(); v != nil {
				fd.Metered = *v
			}
		default:
			p.addError(DiagUnknownProperty, t, "unknown fallback property '"+propName+"'")
			p.skipToNewline()
		}
		fd.Span.End = p.pos(t)
		p.skipNewlines()
	}
	return fd
}

// ---- compute ----

func (p *parser) parseComputeDecl() *ast.ComputeDecl {
	start, name, ok := p.parseDeclHeader("compute")
	if !ok {
		return nil
	}

	cd := &ast.ComputeDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
	}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseComputeProp(cd, t)
	}
	return cd
}

func (p *parser) parseComputeProp(cd *ast.ComputeDecl, propTok Token) {
	// Most compute properties are plain identifiers (input, output, expr,
	// await). We resolve by token TYPE for the ones that carry dedicated
	// keywords and by token VALUE for the others.
	p.next()
	switch propTok.Type {
	case TokenInput:
		p.expect(TokenColon)
		cd.Input = p.expectIdent()
	case TokenOutput:
		p.expect(TokenColon)
		cd.Output = p.expectIdent()
	case TokenPublish:
		p.expect(TokenColon)
		cd.Publish = p.expectIdent()
	case TokenArtifactLabels:
		p.expect(TokenColon)
		cd.ArtifactLabels = p.parseToolList()
	case TokenAwait:
		p.expect(TokenColon)
		cd.Await = p.parseAwaitMode()
	case TokenIdent:
		switch propTok.Value {
		case "expr":
			cd.Expr = p.parseComputeExprBlock()
		case "description":
			p.expect(TokenColon)
			cd.Description = p.expectString()
		default:
			p.addError(DiagUnknownProperty, propTok, "unknown compute property '"+propTok.Value+"'")
			p.skipToNewline()
		}
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown compute property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

// parseComputeExprBlock parses the indented `expr:` block:
//
//	expr:
//	  field_a: "input.x && input.y"
//	  field_b: "vars.n + 1"
func (p *parser) parseComputeExprBlock() []*ast.ComputeExpr {
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	var entries []*ast.ComputeExpr
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		keyT := p.next()
		key := tokenAsIdent(keyT)
		if key == "" {
			p.addError(DiagExpectedToken, keyT, "expected field name in compute expr block")
			p.skipToNewline()
			continue
		}
		p.expect(TokenColon)
		valT := p.next()
		if valT.Type != TokenString {
			p.addError(DiagExpectedToken, valT, "expected string expression in compute expr block")
			p.skipToNewline()
			continue
		}
		entries = append(entries, &ast.ComputeExpr{
			Key:  key,
			Expr: valT.Value,
			Span: ast.Span{Start: p.pos(keyT), End: p.pos(valT)},
		})
		p.skipNewlines()
	}
	return entries
}

// ---- group / use / subbot ----

// parseGroupDecl parses a reusable node-cluster:
//
//	group <name>(<param>, ...):
//	  <agent|judge|router|human|tool|compute decls>
//	  <internal edges>
func (p *parser) parseGroupDecl() *ast.GroupDecl {
	start := p.next() // consume "group"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected group name")
		p.skipToNextTopLevel()
		return nil
	}
	gd := &ast.GroupDecl{Name: name, Span: ast.Span{Start: p.pos(start)}}

	// Optional parameter list: (p1, p2, ...)
	if p.peek().Type == TokenLParen {
		p.next()
		for p.peek().Type != TokenRParen && p.peek().Type != TokenEOF && p.peek().Type != TokenNewline {
			pn := tokenAsIdent(p.next())
			if pn != "" {
				gd.Params = append(gd.Params, pn)
			}
			if p.peek().Type == TokenComma {
				p.next()
			}
		}
		p.expect(TokenRParen)
	}

	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return gd
	}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		switch t.Type {
		case TokenAgent:
			if ad := p.parseAgentDecl(); ad != nil {
				gd.Agents = append(gd.Agents, ad)
			}
		case TokenJudge:
			if jd := p.parseJudgeDecl(); jd != nil {
				gd.Judges = append(gd.Judges, jd)
			}
		case TokenRouter:
			if rd := p.parseRouterDecl(); rd != nil {
				gd.Routers = append(gd.Routers, rd)
			}
		case TokenHuman:
			if hd := p.parseHumanDecl(); hd != nil {
				gd.Humans = append(gd.Humans, hd)
			}
		case TokenTool:
			if td := p.parseToolNodeDecl(); td != nil {
				gd.Tools = append(gd.Tools, td)
			}
		case TokenCompute:
			if cd := p.parseComputeDecl(); cd != nil {
				gd.Computes = append(gd.Computes, cd)
			}
		case TokenComment:
			p.next()
		default:
			if t.Type == TokenIdent || isKeywordToken(t.Type) {
				if e := p.parseEdge(); e != nil {
					gd.Edges = append(gd.Edges, e)
				}
			} else {
				p.addError(DiagUnexpectedToken, t, "unexpected token '"+t.Value+"' in group body")
				p.next()
			}
		}
	}
	return gd
}

// parseUseDecl parses a group instantiation:
//
//	use <group> as <prefix> [with { <param>: "<value>", ... }]
func (p *parser) parseUseDecl() *ast.UseDecl {
	start := p.next() // consume "use"
	group := tokenAsIdent(p.next())
	ud := &ast.UseDecl{Group: group, Span: ast.Span{Start: p.pos(start)}}
	if group == "" {
		p.addError(DiagExpectedToken, p.peek(), "expected group name after 'use'")
		p.skipToNewline()
		return ud
	}
	if p.peek().Type == TokenAs {
		p.next()
		ud.Prefix = p.expectIdent()
	} else {
		p.addError(DiagExpectedToken, p.peek(), "expected 'as <prefix>' after 'use "+group+"'")
	}
	if p.peek().Type == TokenWith {
		ud.With = p.parseWithBlock()
	}
	p.skipNewlines()
	return ud
}

// parseSubbotDecl parses a sub-bot node:
//
//	subbot <name>:
//	  source: "child.bot"
//	  with: { var: "value", ... }
//	  output: <schema>
//	  needs: <resource>
//	  isolated: true
func (p *parser) parseSubbotDecl() *ast.SubbotDecl {
	start, name, ok := p.parseDeclHeader("subbot")
	if !ok {
		return nil
	}
	sd := &ast.SubbotDecl{Name: name, Span: ast.Span{Start: p.pos(start)}}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		switch {
		case t.Type == TokenOutput:
			p.next()
			p.expect(TokenColon)
			sd.Output = p.expectIdent()
		case t.Type == TokenWith:
			sd.With = p.parseWithBlock()
			p.skipNewlines()
		case t.Type == TokenIdent && t.Value == "source":
			p.next()
			p.expect(TokenColon)
			sd.Source = p.expectString()
		case t.Type == TokenIdent && t.Value == "description":
			p.next()
			p.expect(TokenColon)
			sd.Description = p.expectString()
		case t.Type == TokenIdent && t.Value == "isolated":
			p.next()
			p.expect(TokenColon)
			if v := p.parseBool(); v != nil {
				sd.Isolated = *v
			}
		case t.Type == TokenNeeds:
			p.next()
			p.expect(TokenColon)
			sd.Needs = p.parseNeedsList()
		default:
			p.addError(DiagUnknownProperty, t, "unknown subbot property '"+t.Value+"'")
			p.next()
			p.skipToNewline()
		}
	}
	return sd
}

// ---- emit / wait (ADR-051) ----

// parseEmitDecl parses an emit node (ADR-051):
//
//	emit <name>:
//	  event: "ready"
//	  with: { value: "{{outputs.producer.n}}" }
func (p *parser) parseEmitDecl() *ast.EmitDecl {
	start, name, ok := p.parseDeclHeader("emit")
	if !ok {
		return nil
	}
	ed := &ast.EmitDecl{Name: name, Span: ast.Span{Start: p.pos(start)}}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		switch {
		case t.Type == TokenWith:
			ed.With = p.parseWithBlock()
			p.skipNewlines()
		case t.Type == TokenIdent && t.Value == "event":
			p.next()
			p.expect(TokenColon)
			ed.Event = p.expectString()
		case t.Type == TokenIdent && t.Value == "description":
			p.next()
			p.expect(TokenColon)
			ed.Description = p.expectString()
		default:
			p.addError(DiagUnknownProperty, t, "unknown emit property '"+t.Value+"'")
			p.next()
			p.skipToNewline()
		}
	}
	return ed
}

// parseWaitDecl parses a wait node (ADR-051):
//
//	wait <name>:
//	  event: "ready"
//	  timeout: "30s"
//	  output: <schema>
func (p *parser) parseWaitDecl() *ast.WaitDecl {
	start, name, ok := p.parseDeclHeader("wait")
	if !ok {
		return nil
	}
	wd := &ast.WaitDecl{Name: name, Span: ast.Span{Start: p.pos(start)}}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		switch {
		case t.Type == TokenOutput:
			p.next()
			p.expect(TokenColon)
			wd.Output = p.expectIdent()
		case t.Type == TokenIdent && t.Value == "event":
			p.next()
			p.expect(TokenColon)
			wd.Event = p.expectString()
		case t.Type == TokenIdent && t.Value == "timeout":
			p.next()
			p.expect(TokenColon)
			wd.Timeout = p.expectString()
		case t.Type == TokenIdent && t.Value == "description":
			p.next()
			p.expect(TokenColon)
			wd.Description = p.expectString()
		default:
			p.addError(DiagUnknownProperty, t, "unknown wait property '"+t.Value+"'")
			p.next()
			p.skipToNewline()
		}
	}
	return wd
}

// parseAwaitAnswersDecl parses an await_answers node — the deterministic sync
// point for async human questions:
//
//	await_answers <name>:
//	  from: gatherer
//	  timeout: "30m"
func (p *parser) parseAwaitAnswersDecl() *ast.AwaitAnswersDecl {
	start, name, ok := p.parseDeclHeader("await_answers")
	if !ok {
		return nil
	}
	ad := &ast.AwaitAnswersDecl{Name: name, Span: ast.Span{Start: p.pos(start)}}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		switch {
		case t.Type == TokenIdent && t.Value == "from":
			p.next()
			p.expect(TokenColon)
			ad.From = p.expectStringOrIdent()
		case t.Type == TokenIdent && t.Value == "timeout":
			p.next()
			p.expect(TokenColon)
			ad.Timeout = p.expectString()
		case t.Type == TokenIdent && t.Value == "description":
			p.next()
			p.expect(TokenColon)
			ad.Description = p.expectString()
		default:
			p.addError(DiagUnknownProperty, t, "unknown await_answers property '"+t.Value+"'")
			p.next()
			p.skipToNewline()
		}
	}
	return ad
}
