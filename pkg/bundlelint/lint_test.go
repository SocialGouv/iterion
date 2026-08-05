package bundlelint_test

import (
	"sort"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// wf builds a minimal workflow with the given declared var names, file
// secrets, workflow-level capabilities, and name. Enough to exercise the
// consistency checks without invoking the parser.
func wf(name string, vars []string, secrets map[string]string, caps []string, nodes ...ir.Node) *ir.Workflow {
	w := &ir.Workflow{
		Name:         name,
		Vars:         map[string]*ir.Var{},
		Secrets:      map[string]*ir.Secret{},
		Capabilities: caps,
		Nodes:        map[string]ir.Node{},
	}
	for _, v := range vars {
		w.Vars[v] = &ir.Var{Name: v}
	}
	for n, as := range secrets {
		w.Secrets[n] = &ir.Secret{Name: n, As: as}
	}
	for _, n := range nodes {
		w.Nodes[n.NodeID()] = n
	}
	return w
}

func agentWithCaps(id string, caps ...string) ir.Node {
	return &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, Capabilities: caps}
}

func agentWithBotMemory(id string) ir.Node {
	return &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, Memory: &ir.Memory{Enabled: true, Visibility: "bot"}}
}

// auto_memory is per-bot memory too, expressed on the node rather than in a
// `memory:` block.
func agentWithAutoMemory(id string) ir.Node {
	return &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, AutoMemory: "on"}
}

func codesOf(diags []bundlelint.Diag) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, string(d.Code))
	}
	sort.Strings(out)
	return out
}

func eqCodes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCheckConsistency(t *testing.T) {
	cases := []struct {
		name  string
		in    bundlelint.Input
		codes []string // expected diagnostic codes, sorted; nil = none
	}{
		{
			name: "nil manifest is a no-op",
			in:   bundlelint.Input{Manifest: nil, Workflow: wf("b", nil, nil, nil)},
		},
		{
			name: "dispatch_var declared is clean",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", DispatchVars: map[string]string{"feat": "{{issue.title}}"}},
				Workflow: wf("b", []string{"feat"}, nil, nil),
			},
		},
		{
			name: "dispatch_var typo trips C200",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", DispatchVars: map[string]string{"feet": "{{issue.title}}"}},
				Workflow: wf("b", []string{"feat"}, nil, nil),
			},
			codes: []string{"C200"},
		},
		{
			name: "context_var typo trips C201",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Invocations: []bundle.Invocation{
					{Kind: bundle.InvocationKindCommand, ContextVars: map[string]string{"nope": "x"}},
				}},
				Workflow: wf("b", []string{"yes"}, nil, nil),
			},
			codes: []string{"C201"},
		},
		{
			name: "schedule default_var typo trips C202",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Invocations: []bundle.Invocation{
					{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{DefaultVars: map[string]string{"nope": "x"}}},
				}},
				Workflow: wf("b", []string{"yes"}, nil, nil),
			},
			codes: []string{"C202"},
		},
		{
			name: "launch_var typo trips C203",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Forge: &bundle.ForgeRequirements{
					Webhook: &bundle.ForgeWebhookHints{LaunchVars: map[string]string{"nope": "x"}},
				}},
				Workflow: wf("b", []string{"yes"}, nil, nil),
			},
			codes: []string{"C203"},
		},
		{
			name: "args_var declared is clean",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Invocations: []bundle.Invocation{
					{Kind: bundle.InvocationKindCommand, ArgsVar: "scope_notes"},
				}},
				Workflow: wf("b", []string{"scope_notes"}, nil, nil),
			},
		},
		{
			name: "args_var typo trips C204",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Invocations: []bundle.Invocation{
					{Kind: bundle.InvocationKindCommand, ArgsVar: "scope_notes"},
				}},
				Workflow: wf("b", []string{"other"}, nil, nil),
			},
			codes: []string{"C204"},
		},
		{
			name: "forge secret declared as file is clean",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Forge: &bundle.ForgeRequirements{Events: []string{bundle.ForgeEventPullRequest}}},
				Workflow: wf("b", nil, map[string]string{"forge_token": "file"}, nil),
			},
		},
		{
			name: "forge secret missing trips C210",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Forge: &bundle.ForgeRequirements{Events: []string{bundle.ForgeEventPullRequest}}},
				Workflow: wf("b", nil, nil, nil),
			},
			codes: []string{"C210"},
		},
		{
			name: "forge secret via kind:forge invocation, missing, trips C210",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Invocations: []bundle.Invocation{
					{Kind: bundle.InvocationKindForge, Forge: &bundle.InvocationForge{Event: bundle.ForgeEventPullRequest}},
				}},
				Workflow: wf("b", nil, nil, nil),
			},
			codes: []string{"C210"},
		},
		{
			name: "forge secret declared as env trips C211",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Forge: &bundle.ForgeRequirements{Events: []string{bundle.ForgeEventPullRequest}, Secret: "gl_pat"}},
				Workflow: wf("b", nil, map[string]string{"gl_pat": "env"}, nil),
			},
			codes: []string{"C211"},
		},
		{
			name: "no forge block, no forge invocation: forge secret unchecked",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b"},
				Workflow: wf("b", nil, nil, nil),
			},
		},
		{
			name: "manifest cap granted by a node is clean",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Capabilities: []string{"board.create"}},
				Workflow: wf("b", nil, nil, nil, agentWithCaps("a", "board.create")),
			},
		},
		{
			name: "manifest cap granted by workflow-level is clean",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Capabilities: []string{"board.create"}},
				Workflow: wf("b", nil, nil, []string{"board.create"}),
			},
		},
		{
			name: "manifest cap granted by nobody trips C220",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b", Capabilities: []string{"board.create"}},
				Workflow: wf("b", nil, nil, nil, agentWithCaps("a", "board.read")),
			},
			codes: []string{"C220"},
		},
		{
			name: "frontmatter caps differ from manifest trips C221",
			in: bundlelint.Input{
				Manifest:    &bundle.Manifest{Name: "b", Capabilities: []string{"board.read"}},
				Workflow:    wf("b", nil, nil, nil, agentWithCaps("a", "board.move")),
				Frontmatter: &bundle.Frontmatter{Capabilities: []string{"board.move"}},
			},
			// C220 fires for the manifest cap board.read (granted by nobody),
			// C221 for the frontmatter override divergence.
			codes: []string{"C220", "C221"},
		},
		{
			name: "frontmatter caps equal to manifest: no C221",
			in: bundlelint.Input{
				Manifest:    &bundle.Manifest{Name: "b", Capabilities: []string{"board.move"}},
				Workflow:    wf("b", nil, nil, nil, agentWithCaps("a", "board.move")),
				Frontmatter: &bundle.Frontmatter{Capabilities: []string{"board.move"}},
			},
		},
		{
			name: "frontmatter override detectable with nil workflow",
			in: bundlelint.Input{
				Manifest:    &bundle.Manifest{Name: "b", Capabilities: []string{"board.read"}},
				Workflow:    nil,
				Frontmatter: &bundle.Frontmatter{Capabilities: []string{"board.move"}},
			},
			codes: []string{"C221"},
		},
		{
			name: "per-bot memory with matching names is clean",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "mybot"},
				Workflow: wf("mybot", nil, nil, nil, agentWithBotMemory("a")),
				DirName:  "mybot",
			},
		},
		{
			name: "per-bot memory name mismatch trips C230 (error)",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "mybot"},
				Workflow: wf("my_workflow", nil, nil, nil, agentWithBotMemory("a")),
				DirName:  "mybot",
			},
			codes: []string{"C230"},
		},
		{
			// auto_memory keys ONE space per (bot, repo), resolved from
			// whichever name the launching surface knows — manifest, bundle
			// dir, or workflow. Three names that disagree are three memories,
			// so the same stability rule has to cover it.
			name: "auto_memory name mismatch trips C230 too",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "mybot"},
				Workflow: wf("my_workflow", nil, nil, nil, agentWithAutoMemory("a")),
				DirName:  "mybot",
			},
			codes: []string{"C230"},
		},
		{
			name: "auto_memory with matching names is clean",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "mybot"},
				Workflow: wf("mybot", nil, nil, nil, agentWithAutoMemory("a")),
				DirName:  "mybot",
			},
		},
		{
			name: "name mismatch without per-bot memory is clean",
			in: bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "mybot"},
				Workflow: wf("my_workflow", nil, nil, nil, agentWithCaps("a")),
				DirName:  "mybot",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codesOf(bundlelint.CheckConsistency(tc.in))
			want := tc.codes
			if want == nil {
				want = []string{}
			}
			if !eqCodes(got, want) {
				t.Errorf("codes = %v, want %v", got, want)
			}
		})
	}
}

// TestC230IsError pins the one error-severity finding; everything else is a
// warning so the catalog never breaks on first roll-out.
func TestSeverities(t *testing.T) {
	mismatch := bundlelint.CheckConsistency(bundlelint.Input{
		Manifest: &bundle.Manifest{Name: "mybot"},
		Workflow: wf("other", nil, nil, nil, agentWithBotMemory("a")),
		DirName:  "mybot",
	})
	if len(mismatch) != 1 || mismatch[0].Code != bundlelint.DiagBundleNameTripleMismatch {
		t.Fatalf("expected exactly one C230, got %v", mismatch)
	}
	if mismatch[0].Severity != bundlelint.SeverityError {
		t.Errorf("C230 should be an error, got %s", mismatch[0].Severity)
	}

	warnOnly := bundlelint.CheckConsistency(bundlelint.Input{
		Manifest: &bundle.Manifest{Name: "b", DispatchVars: map[string]string{"typo": "x"}},
		Workflow: wf("b", []string{"real"}, nil, nil),
	})
	if len(warnOnly) != 1 || warnOnly[0].Severity != bundlelint.SeverityWarning {
		t.Errorf("C200 should be a warning, got %v", warnOnly)
	}
}

// TestDeterministicOrder ensures diagnostics come back sorted by (Code, Field)
// so CLI/studio output and golden tests are stable.
func TestDeterministicOrder(t *testing.T) {
	in := bundlelint.Input{
		Manifest: &bundle.Manifest{
			Name:         "b",
			DispatchVars: map[string]string{"zzz": "x", "aaa": "y"},
			Capabilities: []string{"board.read"},
		},
		Workflow: wf("b", nil, nil, nil),
	}
	got := bundlelint.CheckConsistency(in)
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if cur.Code < prev.Code || (cur.Code == prev.Code && cur.Field < prev.Field) {
			t.Errorf("diagnostics not sorted at %d: %q then %q", i, prev.Error(), cur.Error())
		}
	}
}

// TestCheckSkills covers the routability lint (C231–C234): missing name,
// missing/terse description, and duplicate names — all warnings, no prose-style
// rules. A well-formed skill set produces nothing.
func TestCheckSkills(t *testing.T) {
	cases := []struct {
		name   string
		skills []bundlelint.SkillDoc
		want   []string
	}{
		{
			name: "clean set — no diagnostics",
			skills: []bundlelint.SkillDoc{
				{Path: "skills/repo-survey.md", Name: "repo-survey", Description: "Survey a repository to map its layout, stack, and conventions."},
				{Path: "skills/roadmap.md", Name: "roadmap", Description: "Synthesize a prioritized roadmap from the collected findings."},
			},
			want: nil,
		},
		{
			name:   "missing name",
			skills: []bundlelint.SkillDoc{{Path: "skills/x.md", Description: "A description long enough to route on cleanly."}},
			want:   []string{"C231"},
		},
		{
			name:   "missing description",
			skills: []bundlelint.SkillDoc{{Path: "skills/x.md", Name: "x"}},
			want:   []string{"C232"},
		},
		{
			name:   "terse description",
			skills: []bundlelint.SkillDoc{{Path: "skills/x.md", Name: "x", Description: "Security stuff"}},
			want:   []string{"C233"},
		},
		{
			name: "duplicate name",
			skills: []bundlelint.SkillDoc{
				{Path: "skills/a.md", Name: "dup", Description: "First skill with a routable description."},
				{Path: "skills/b.md", Name: "dup", Description: "Second skill with a routable description."},
			},
			want: []string{"C234"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Manifest is required (CheckConsistency returns nil without it);
			// the skill checks are independent of the workflow.
			got := codesOf(bundlelint.CheckConsistency(bundlelint.Input{
				Manifest: &bundle.Manifest{Name: "b"},
				Skills:   tc.skills,
			}))
			if !eqCodes(got, tc.want) {
				t.Errorf("codes = %v, want %v", got, tc.want)
			}
		})
	}
}
