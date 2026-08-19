package parser

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// ---- workflow ----

func (p *parser) parseWorkflowDecl() *ast.WorkflowDecl {
	start, name, ok := p.parseDeclHeader("workflow")
	if !ok {
		return nil
	}

	wd := &ast.WorkflowDecl{
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
		case TokenVars:
			wd.Vars = p.parseVarsBlock()

		case TokenAttachments:
			wd.Attachments = p.parseAttachmentsBlock()

		case TokenMCP:
			wd.MCP = p.parseMCPConfigBlock()

		case TokenEntry:
			p.next() // consume "entry"
			p.expect(TokenColon)
			// Accept a dotted reference so the entry can be a group-instance
			// node (`entry: r1.gate`).
			wd.Entry = p.continueDottedRef(p.expectIdent())
			p.skipNewlines()

		case TokenBudget:
			wd.Budget = p.parseBudgetBlock()

		case TokenResources:
			wd.Resources = p.parseResourcesBlock()

		case TokenCompaction:
			wd.Compaction = p.parseCompactionBlock()

		case TokenWorktree:
			p.next() // consume "worktree"
			p.expect(TokenColon)
			wd.Worktree = p.expectIdent()
			p.skipNewlines()

		case TokenCompress:
			p.next() // consume "compress"
			p.expect(TokenColon)
			wd.Compress = p.expectIdent()
			p.skipNewlines()

		case TokenAutoMemory:
			p.next() // consume "auto_memory"
			p.expect(TokenColon)
			wd.AutoMemory = p.expectIdent()
			p.skipNewlines()

		case TokenLoopBudgetGuard:
			p.next() // consume "loop_budget_guard"
			p.expect(TokenColon)
			wd.LoopBudgetGuard = p.expectIdent()
			p.skipNewlines()

		case TokenRepoDevbox:
			p.next() // consume "repo_devbox"
			p.expect(TokenColon)
			wd.RepoDevbox = p.expectIdent()
			p.skipNewlines()

		case TokenPermission:
			p.next() // consume "permission"
			p.expect(TokenColon)
			wd.Permission = p.expectIdent()
			p.skipNewlines()

		case TokenAllow:
			p.next() // consume "allow"
			p.expect(TokenColon)
			wd.Allow = p.parseStringList()
			p.skipNewlines()

		case TokenAsk:
			p.next() // consume "ask"
			p.expect(TokenColon)
			wd.Ask = p.parseStringList()
			p.skipNewlines()

		case TokenDeny:
			p.next() // consume "deny"
			p.expect(TokenColon)
			wd.Deny = p.parseStringList()
			p.skipNewlines()

		case TokenSandbox:
			wd.Sandbox = p.parseSandboxBlock()
			p.skipNewlines()

		case TokenDefaultBackend:
			p.next() // consume "default_backend"
			p.expect(TokenColon)
			wd.DefaultBackend = p.expectString()
			p.skipNewlines()

		case TokenToolPolicy:
			p.next() // consume "tool_policy"
			p.expect(TokenColon)
			wd.ToolPolicy = p.parseToolList()
			p.skipNewlines()

		case TokenCapabilities:
			p.next() // consume "capabilities"
			p.expect(TokenColon)
			wd.Capabilities = p.parseToolList()
			p.skipNewlines()

		case TokenSkills:
			p.next() // consume "skills"
			p.expect(TokenColon)
			wd.Skills = p.parseSkillList()
			p.skipNewlines()

		case TokenInteraction:
			p.next() // consume "interaction"
			p.expect(TokenColon)
			im := p.parseInteractionMode()
			wd.Interaction = &im
			p.skipNewlines()

		case TokenComment:
			p.next() // skip workflow-level comments

		default:
			// Must be an edge: IDENT -> IDENT ...
			if t.Type == TokenIdent || isKeywordToken(t.Type) {
				edge := p.parseEdge()
				if edge != nil {
					wd.Edges = append(wd.Edges, edge)
				}
			} else {
				p.addError(DiagUnexpectedToken, t, "unexpected token '"+t.Value+"' in workflow")
				p.next()
			}
		}
	}
	return wd
}

func (p *parser) parseBudgetBlock() *ast.BudgetBlock {
	start := p.next() // consume "budget"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	bb := &ast.BudgetBlock{Span: ast.Span{Start: p.pos(start)}}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseBudgetProp(bb, t)
	}
	return bb
}

func (p *parser) parseBudgetProp(bb *ast.BudgetBlock, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenMaxParallelBranches:
		p.expect(TokenColon)
		bb.MaxParallelBranches = p.expectInt()
	case TokenMaxDuration:
		p.expect(TokenColon)
		bb.MaxDuration = p.expectString()
	case TokenMaxCostUSD:
		p.expect(TokenColon)
		bb.MaxCostUSD = p.expectNumber()
	case TokenMaxTokens:
		p.expect(TokenColon)
		bb.MaxTokens = p.expectInt()
	case TokenWarnTokens:
		p.expect(TokenColon)
		bb.WarnTokens = p.expectInt()
	case TokenMaxIterations:
		p.expect(TokenColon)
		bb.MaxIterations = p.expectInt()
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown budget property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

// parseResourcesBlock parses `resources:\n  <name>: <capacity>` pairs. Each
// name is an arbitrary identifier (the resource), each value its slot count.
func (p *parser) parseResourcesBlock() *ast.ResourcesBlock {
	start := p.next() // consume "resources"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	rb := &ast.ResourcesBlock{
		Capacities: make(map[string]int),
		Span:       ast.Span{Start: p.pos(start)},
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
		p.parseResourceProp(rb, t)
	}
	return rb
}

func (p *parser) parseResourceProp(rb *ast.ResourcesBlock, propTok Token) {
	name := tokenAsIdent(p.next())
	if name == "" {
		p.addError(DiagInvalidValue, propTok, "expected a resource name")
		p.skipToNewline()
		p.skipNewlines()
		return
	}
	p.expect(TokenColon)
	if p.peek().Type == TokenLBrack {
		// Named-instance pool (lease form): godot: ["godot-s1", "godot-s2", ...].
		// Capacity = number of members; each acquire leases a distinct id. Ids
		// are quoted strings (not bare idents) so they may carry hyphens/slashes
		// — e.g. MCP server names or worktree paths.
		members := p.parseStringList()
		if rb.Members == nil {
			rb.Members = make(map[string][]string)
		}
		rb.Members[name] = members
		rb.Capacities[name] = len(members)
	} else {
		// Counting-only form: godot: 5.
		rb.Capacities[name] = p.expectInt()
	}
	p.skipNewlines()
}

func (p *parser) parseCompactionBlock() *ast.CompactionBlock {
	start := p.next() // consume "compaction"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	cb := &ast.CompactionBlock{Span: ast.Span{Start: p.pos(start)}}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseCompactionProp(cb, t)
	}
	return cb
}

// parseMemoryBlock parses a `memory:` sub-block on an agent or
// judge node. All fields are optional; IR compile applies defaults.
func (p *parser) parseMemoryBlock() *ast.MemoryBlock {
	start := p.next() // consume "memory"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	mb := &ast.MemoryBlock{Span: ast.Span{Start: p.pos(start)}}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseMemoryProp(mb, t)
	}
	return mb
}

func (p *parser) parseCompactionProp(cb *ast.CompactionBlock, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenThreshold:
		p.expect(TokenColon)
		v := p.expectNumber()
		cb.Threshold = &v
	case TokenPreserveRecent:
		p.expect(TokenColon)
		v := p.expectInt()
		cb.PreserveRecent = &v
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown compaction property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

func (p *parser) parseMemoryProp(mb *ast.MemoryBlock, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenEnabled:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			mb.Enabled = v
		}
	case TokenScope:
		p.expect(TokenColon)
		v := p.expectString()
		mb.Scope = &v
	case TokenAutoload:
		p.expect(TokenColon)
		mb.Autoload = p.parseStringList()
	case TokenRead:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			mb.Read = v
		}
	case TokenWrite:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			mb.Write = v
		}
	case TokenPreCompactInject:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			mb.PreCompactInject = v
		}
	case TokenProjectRoot:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			mb.ProjectRoot = v
		}
	case TokenVisibility:
		p.expect(TokenColon)
		v := p.expectString()
		mb.Visibility = &v
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown memory property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}
