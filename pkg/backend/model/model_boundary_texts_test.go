package model

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-146 axis 1a covers every text a model READS, not only the two
// prompts: delegate.Task.BuildSystemPrompt appends the auto-memory
// section, the mounted-secret hints, the calibration fragments, the skill
// hints and the preset focus AFTER the seam, and Task.HandoffSection
// splices a sealed transcript in from inside the backend. These tests pin
// each of those, and the last one refuses a FIELD or a LEAF the next
// change would let drift past the boundary.

// autoMemoryWritingBackend plays the agent of the primary rung: it writes
// into the MEMORY.md it was pointed at, then fails so the chain falls
// through to the next backend, which re-renders the section.
type autoMemoryWritingBackend struct {
	name string
	note string
	err  error
	mu   sync.Mutex
	task delegate.Task
}

func (b *autoMemoryWritingBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	b.task = task
	b.mu.Unlock()
	if task.AutoMemoryDir == "" {
		return delegate.Result{BackendName: b.name}, b.err
	}
	if err := os.WriteFile(filepath.Join(task.AutoMemoryDir, "MEMORY.md"), []byte(b.note), 0o600); err != nil {
		return delegate.Result{}, err
	}
	return delegate.Result{BackendName: b.name}, b.err
}

// The live window AutoMemoryPrompt opens: claude_code points its NATIVE
// auto-memory at the directory and carries no prompt section, so the value
// its agent writes into MEMORY.md is unscreened text on disk — the mirror
// unmaterialises on the STORE-WRITE direction only. A fall-through to claw
// re-renders the section from that file, so the next model reads what the
// previous one wrote. It must read the placeholder.
func TestTheAutoMemorySectionReachesAFallThroughBackendAsThePlaceholder(t *testing.T) {
	t.Parallel()
	const sentinel = "tok-9c2e7a1b3d4f-automemory"
	placeholder := secretguard.PlaceholderForName("api_token")
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())

	head := &autoMemoryWritingBackend{
		name: delegate.BackendClaudeCode,
		note: "## What I learned\n\n- the deploy token is " + sentinel + "\n",
		err:  &delegate.ErrTransient{Reason: "the primary harness died after writing its notes"},
	}
	tail := &capturingBackend{results: []delegate.Result{{Output: map[string]any{"ok": true}, BackendName: delegate.BackendClaw}}}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendClaw, tail)

	wf := &ir.Workflow{
		AutoMemory: "on",
		Prompts:    map[string]*ir.Prompt{"sys": {Name: "sys", Body: "do the work"}, "user": {Name: "user", Body: "go"}},
		Schemas:    map[string]*ir.Schema{},
	}
	exec := NewClawExecutor(NewRegistry(), wf,
		WithBackendRegistry(reg),
		WithSecretGuard(guard),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
		WithBotID("boundary-tester"),
		// The store dir roots the memory space (Task.StateDir): a temp one
		// keeps the mirror inside the test's own tree.
		WithStoreDir(t.TempDir()),
		WithWorkDir(t.TempDir()))
	node := &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "implement"},
		LLMFields: ir.LLMFields{
			Backend: delegate.BackendClaudeCode, Model: "anthropic/m",
			SystemPrompt: "sys", UserPrompt: "user",
		},
		// `on: [any]` so the route is taken whatever the primary's failure
		// category: the subject here is the re-render, not the trigger set.
		Fallbacks: []ir.Fallback{{Name: "claw", Backend: delegate.BackendClaw, Model: "anthropic/m", On: []string{"any"}}},
	}
	td := &TemplateData{Outputs: map[string]map[string]any{}, Nodes: map[string]ir.Node{}}
	if _, err := exec.executeBackend(WithTemplateData(context.Background(), td), node, map[string]any{}); err != nil {
		t.Fatalf("the chain should have been served by the claw element: %v", err)
	}

	head.mu.Lock()
	primary := head.task
	head.mu.Unlock()
	// The premise: claude_code points its NATIVE mechanism at the directory,
	// so the primary carries the dir and no section at all.
	if primary.AutoMemoryDir == "" {
		t.Fatalf("the primary never got an auto-memory directory, so its agent wrote nothing: %+v", primary.AutoMemoryPrompt)
	}
	if primary.AutoMemoryPrompt != "" {
		t.Fatalf("claude_code was handed a prompt section as well as the native dir: %q", primary.AutoMemoryPrompt)
	}

	tail.mu.Lock()
	defer tail.mu.Unlock()
	if len(tail.tasks) != 1 {
		t.Fatalf("the claw element ran %d times, want 1 — the fall-through never happened", len(tail.tasks))
	}
	got := tail.tasks[0]
	if got.AutoMemoryPrompt == "" {
		t.Fatal("the claw element carries no AutoMemoryPrompt: the section was never re-rendered, so this test proves nothing")
	}
	for name, text := range map[string]string{"AutoMemoryPrompt": got.AutoMemoryPrompt, "the composed system prompt": got.BuildSystemPrompt()} {
		if strings.Contains(text, sentinel) {
			t.Errorf("%s carries the value the previous harness wrote into MEMORY.md: %q", name, text)
		}
		if !strings.Contains(text, placeholder) {
			t.Errorf("%s does not carry the placeholder where the value was: %q", name, text)
		}
	}
}

// The calibration fragments and the skill hints are the other two
// BuildSystemPrompt composes after the seam, and neither is iterion's own
// text: a cursor fragment is the author's, a skill hint is the workspace's
// SKILL.md frontmatter (a checkout of the TARGET repository). The assertion
// is on the composed prompt — the artifact the backend hands the model —
// not on the fields alone.
func TestTheCalibrationAndSkillSectionsCrossTheBoundary(t *testing.T) {
	t.Parallel()
	const sentinel = "tok-1b3d4f9c2e7a-sections"
	placeholder := secretguard.PlaceholderForName("api_token")
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())
	e := &ClawExecutor{secretGuard: guard}

	in := delegate.Task{
		SystemPrompt:    "base",
		CursorFragments: []string{"**Depth:** go deep", "**Secrets:** the key is " + sentinel},
		SkillHints:      []delegate.SkillHint{{Name: "deploy", Description: "run the deploy with " + sentinel}},
	}
	out := e.taskForModel(in)
	composed := out.BuildSystemPrompt()
	if strings.Contains(composed, sentinel) {
		t.Errorf("the composed system prompt carries the value: %q", composed)
	}
	if n := strings.Count(composed, placeholder); n != 2 {
		t.Errorf("the composed system prompt carries %d placeholders, want 2 (the fragment and the hint): %q", n, composed)
	}
	// The boundary copies: the executor's own task — what the events, the
	// logs and the session ledger read — keeps what it rendered.
	if !strings.Contains(in.CursorFragments[1], sentinel) || !strings.Contains(in.SkillHints[0].Description, sentinel) {
		t.Error("the boundary mutated the caller's fragments or hints in place")
	}
}

// The routing handoff is the one model-read text taskForModel cannot
// reach: Task.Handoff is a PATH, and Task.HandoffSection reads the file
// with os.ReadFile inside the backend, downstream of the seam. So the
// grant pass is owed where the transcript is WRITTEN. The values are
// already placeholders there (the recorder observes post-redaction
// events); what the write site must still do is DEFUSE the placeholder of
// a secret whose `sinks:` withhold `prompt`, or the next harness reads a
// resolvable token its author kept off prompts and the proxy spends it on
// the wire.
func TestTheHandoffTranscriptDefusesAnUngrantedPlaceholder(t *testing.T) {
	t.Parallel()
	const wireOnly = "ghp-wire-only-7a1b3d4f9c2e"
	const onPrompts = "tok-prompts-4f9c2e7a1b3d"
	guard := secretguard.New([]secretguard.Secret{
		{Name: "gh_token", Value: wireOnly, Sinks: []string{secretsink.Command, secretsink.Egress}},
		{Name: "api_token", Value: onPrompts},
	}, secretguard.DefaultConfig())
	withheld := secretguard.PlaceholderForName("gh_token")
	granted := secretguard.PlaceholderForName("api_token")
	defused := strings.TrimSuffix(withheld, "__") + "_redacted__"

	rec := NewHandoffRecorder(t.TempDir(), guard)
	storeEmitter := &capturingEmitter{}
	chain := redactingEmitter{inner: HandoffEmitter(storeEmitter, rec), guard: guard}
	_, _ = chain.AppendEvent(context.Background(), "run", store.Event{
		Type:   store.EventAssistantText,
		NodeID: "n",
		Data:   map[string]any{"text": "curl -H \"Authorization: Bearer " + wireOnly + "\" and the api key " + onPrompts},
	})
	// The crossing seals through the SAME chain: the fallback data rides
	// the seal rather than Observe, so it takes the pass too.
	if _, err := chain.AppendEvent(context.Background(), "run", store.Event{
		Type:   store.EventModelFallback,
		NodeID: "n",
		Data:   map[string]any{"cross_harness": llmroute.CrossHarnessRestart, "reason": "spent " + wireOnly},
	}); err != nil {
		t.Fatalf("fallback event: %v", err)
	}
	path := rec.SealedPath("n")
	if path == "" {
		t.Fatal("the crossing sealed no file")
	}
	sealed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sealed file: %v", err)
	}
	body := string(sealed)
	if strings.Contains(body, wireOnly) || strings.Contains(body, onPrompts) {
		t.Fatalf("the transcript carries a RAW value: %q", body)
	}
	if strings.Contains(body, withheld+"\"") || strings.Contains(body, withheld+" ") || strings.Contains(body, withheld+"\n") {
		t.Errorf("the transcript hands the next harness the RESOLVABLE placeholder of a secret granting only [command, egress]: %q", body)
	}
	if n := strings.Count(body, defused); n != 2 {
		t.Errorf("the withheld placeholder is defused %d times, want 2 (the observed event and the fallback reason): %q", n, body)
	}
	// A secret its author DID grant on prompts keeps its resolvable
	// placeholder: the next harness must still be able to spend it.
	if !strings.Contains(body, granted) {
		t.Errorf("a prompt-granted secret lost its resolvable placeholder: %q", body)
	}
	// The defused token resolves nowhere, which is the whole claim.
	if guard.MaterializeForHost(defused, "api.github.com") != defused {
		t.Error("the defused token still resolves on the wire")
	}
}

// redactedLeaf marks a leaf the model boundary must turn back; every other
// verdict in modelReadLeaves is the REASON that leaf reaches the model as
// written.
const redactedLeaf = ""

// modelReadLeaves is the verdict for every string leaf of every
// delegate.Task field the two SHARED composers — BuildSystemPrompt and
// HandoffPrompt/HandoffSection, which every backend routes through — render
// into text a model reads, plus UserContent, the multimodal twin of
// UserPrompt. A verdict is a decision, never a field TYPE: "it is a path"
// would bless Handoff, and Handoff is how the leak of the sealed transcript
// travelled.
var modelReadLeaves = map[string]string{
	"SystemPrompt":             redactedLeaf,
	"UserPrompt":               redactedLeaf,
	"AutoMemoryPrompt":         redactedLeaf,
	"CursorFragments[]":        redactedLeaf,
	"SkillHints[].Name":        redactedLeaf,
	"SkillHints[].Description": redactedLeaf,
	"PresetFragment":           redactedLeaf,
	"UserContent[].Text":       redactedLeaf,

	// The mounted-secret hints are the guard's OWN composition: it builds
	// Name/Path/Env from the secret registration itself
	// (Guard.SecretFileHints over guardState.fileHints), so no node output
	// and no model text can reach them — and rewriting a mount path would
	// corrupt the very reference the instruction exists to hand over.
	"SecretFiles[].Name": "guard-composed from the declaration: the secret's own name",
	"SecretFiles[].Path": "guard-composed mount path, the reference the instruction exists to give",
	"SecretFiles[].Env":  "guard-composed env-var NAME, never a value",

	// The work dir is the path the ambient composers walk, never prose: the
	// TEXT they read off it is not a field at all, so it takes the pass
	// where each backend composes it — claw at the request it builds
	// (TestEverySystemTextTheRequestCarriesCrossesTheBoundary), pi at its
	// system-prompt suffix (delegate's
	// TestThePiProjectContextCrossesTheBoundaryTheTaskCarries). Rewriting
	// the path itself would point both walks at nothing.
	"WorkDir": "the workspace path the ambient walks read; their TEXT crosses at the composer, not here",

	// The handoff pair: a path and a posture. The transcript's own text
	// takes the grant pass at the WRITE site, because HandoffSection reads
	// the file inside the backend, downstream of this seam.
	"Handoff":     "a path; the transcript's text is defused in HandoffRecorder.Seal (TestTheHandoffTranscriptDefusesAnUngrantedPlaceholder)",
	"HandoffMode": "the resolved posture name, llmroute vocabulary (reuse|restart)",

	// The workspace provenance posture (ADR-162): store.RunTrust's closed
	// vocabulary, read by the ambient composers to GATE which files they
	// walk — never rendered as prose a model reads.
	"WorkspaceTrust": "the provenance posture, store's closed vocabulary, gating which files the composer walks — never prose",

	// The image half of a content block: bytes and locators, not text. A
	// replacement inside a base64 payload or a URL corrupts the attachment
	// the node declared.
	"UserContent[].Type":      "the block kind (text|image), delegate's own vocabulary",
	"UserContent[].MediaType": "a MIME type",
	"UserContent[].Data":      "the base64 image payload: bytes, and a replacement inside them corrupts the attachment",
	"UserContent[].URL":       "an image locator the backend fetches",
	"UserContent[].Path":      "the host path the read_image tool loads",
	"UserContent[].Name":      "the workflow-declared attachment name, author vocabulary",
}

// modelReadComposers are the functions whose bodies decide what a model
// reads out of a Task, by source file relative to this package. The two
// SHARED composers render Task FIELDS, and every backend routes through
// them. The ambient pairs compose text the backend reads for ITSELF under
// the task's work dir — claw's project-instructions block, pi's
// project_context block — which is why the fields they read are a path and
// a policy rather than prose: the TEXT takes its pass where each backend
// composes it (the verdict for WorkDir names both witnesses).
var modelReadComposers = map[string][]string{
	"../delegate/delegate.go":   {"BuildSystemPrompt"},
	"../delegate/handoff.go":    {"HandoffPrompt", "HandoffSection"},
	"../delegate/pi_ambient.go": {"piComposeSystemPrompt", "piSystemPromptSuffix", "piAmbientFiles", "piUsesNativeContextFiles"},
	"claw_ambient.go":           {"clawAmbientSystemContext", "clawAgentsMdParts"},
}

// modelReadPass is the boundary pass itself. A composer may apply it to the
// text it builds — that is the whole point of carrying it on the task — so
// naming it is not a path the sweep must follow.
const modelReadPass = "ScreenedModelText"

// The CLASS of #2845 item 5: a text the model reads that the boundary does
// not pass. Fixing the three fields the ticket names leaves the class
// alive — the next field would drift in the same way — so this reads the
// composers for the fields they render, walks each field's string leaves,
// and refuses any leaf that is neither turned back by taskForModel nor
// carrying the reason it is not. The behavioural half runs both ways: a
// redacted leaf must LOSE a known value, an allowlisted one must KEEP it,
// so an allowlist entry is a decision rather than an accident.
func TestEveryModelReadTextOfATaskCrossesTheBoundary(t *testing.T) {
	t.Parallel()
	composed := composedTaskFields(t)
	// Vacuity guard: a sweep that parsed nothing would pass silently.
	for _, want := range []string{"SystemPrompt", "AutoMemoryPrompt", "SecretFiles", "CursorFragments", "SkillHints", "Handoff", "WorkDir", "AmbientContext"} {
		if !composed[want] {
			t.Fatalf("the composers no longer read Task.%s — the sweep is not reading them, or the composition moved", want)
		}
	}

	// The walk domain: what the composers render, plus every field the
	// verdict table names (UserContent is read by the backends' own
	// multimodal render, not by the shared composers).
	domain := map[string]bool{}
	for f := range composed {
		domain[f] = true
	}
	for leaf := range modelReadLeaves {
		domain[strings.SplitN(strings.SplitN(leaf, "[", 2)[0], ".", 2)[0]] = true
	}

	const sentinel = "tok-2e7a1b3d4f9c-sweep"
	placeholder := secretguard.PlaceholderForName("api_token")
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())
	e := &ClawExecutor{secretGuard: guard}

	var task delegate.Task
	planted := map[string]string{}
	rt := reflect.TypeOf(task)
	rv := reflect.ValueOf(&task).Elem()
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		if !domain[name] {
			continue
		}
		plantTextLeaves(t, rv.Field(i), name, sentinel, planted)
	}
	out := e.taskForModel(task)
	ro := reflect.ValueOf(out)

	for leaf := range planted {
		verdict, known := modelReadLeaves[leaf]
		if !known {
			t.Errorf("a composer renders Task.%s into text a model reads, and nothing says what the boundary does with it: redact it in taskForModel, or add it to modelReadLeaves with the reason the model may read it as written", leaf)
			continue
		}
		field := strings.SplitN(strings.SplitN(leaf, "[", 2)[0], ".", 2)[0]
		got := readTextLeaf(t, ro.FieldByName(field), leaf)
		if verdict == redactedLeaf {
			if strings.Contains(got, sentinel) {
				t.Errorf("Task.%s reaches the model carrying a known value: %q", leaf, got)
			}
			if !strings.Contains(got, placeholder) {
				t.Errorf("Task.%s lost the value without leaving its placeholder: %q", leaf, got)
			}
			continue
		}
		if !strings.Contains(got, sentinel) {
			t.Errorf("Task.%s is allowlisted (%s) yet the boundary rewrote it: %q", leaf, verdict, got)
		}
	}
	// A table entry naming a leaf that no longer exists would silently stop
	// proving anything after a rename.
	for leaf := range modelReadLeaves {
		if _, ok := planted[leaf]; !ok {
			t.Errorf("modelReadLeaves names Task.%s, which is not a text leaf of delegate.Task any more", leaf)
		}
	}
}

// selfComposedTextWriters are the sites of this package that write a text a
// model reads which no Task field holds, keyed by the GESTURE that makes
// one and the function it is in — a system block born (an assignment to
// SystemBlocks), a compaction hook installed (an assignment to
// OnBeforeCompact), a turn marked injected (a message literal carrying
// IsInjected) — each with the chokepoint that screens what it writes and
// that chokepoint's own witness. A gesture the sweep finds outside this
// table is a writer no chokepoint is declared for.
var selfComposedTextWriters = map[string]string{
	"SystemBlocks@claw_backend.go:Execute":                   "buildRequest screens every system block the request carries (TestEverySystemTextTheRequestCarriesCrossesTheBoundary)",
	"SystemBlocks@memory_tools.go:installWorkspaceMemory":    "buildRequest screens every system block the request carries (TestEverySystemTextTheRequestCarriesCrossesTheBoundary)",
	"SystemBlocks@generation_request.go:buildRequest":        "the chokepoint itself: the blocks it assigns are the screened copies",
	"OnBeforeCompact@memory_tools.go:installWorkspaceMemory": "compactBetweenIterations wraps the hook in screenedInjector (TestTheInjectedMemoryTurnCrossesTheBoundaryBeforeTheSummariser)",
	"OnBeforeCompact@generation.go:compactBetweenIterations": "the chokepoint itself: the wrap that screens every injected turn before the summariser reads it",
	"IsInjected@memory_tools.go:memoryPreCompactInjector":    "an injected turn, screened by compactBetweenIterations before the summariser reads it",
}

// The writer-side half of the class: the field sweep above judges what the
// listed composers render out of a Task, so a producer of model-read text
// that reads no Task field — the backend composing for itself — never
// trips it. This reads every non-test file of the package for the three
// gestures that make such a text and refuses a writer the table does not
// enrol with its chokepoint, so a fourth system-block append or a second
// injector has to say what screens it. The limit, stated rather than
// claimed away: it keys on those gestures, not on every message literal of
// the package — the loop's own fixed nudges, the prompt the seam passed,
// the tool results the loop unmaterializes, the model's own replies and the
// operator's queued turn are built as plain messages and are outside it.
func TestEveryWriterOfSelfComposedModelTextNamesItsChokepoint(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := 0
	found := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		files++
		for _, d := range f.Decls {
			// A package-level literal is a writer too; it is keyed on the
			// file rather than a function.
			within := "package"
			if fn, ok := d.(*ast.FuncDecl); ok {
				within = fn.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				for _, gesture := range writerGestures(n) {
					found[gesture+"@"+name+":"+within] = true
				}
				return true
			})
		}
	}
	if files < 40 {
		t.Fatalf("parsed %d non-test files — the sweep is not reading the package", files)
	}
	for site := range found {
		if _, enrolled := selfComposedTextWriters[site]; !enrolled {
			t.Errorf("%s writes a text a model reads that no Task field holds, and no chokepoint is named for it: route it through buildRequest (a system block) or an OnBeforeCompact hook (an injected turn), and enrol it in selfComposedTextWriters with the chokepoint that screens it", site)
		}
	}
	for site := range selfComposedTextWriters {
		if !found[site] {
			t.Errorf("selfComposedTextWriters names %s, which no longer writes: the site moved and the entry proves nothing", site)
		}
	}
}

// writerGestures names the self-composed-text gestures n performs: the
// field written by an assignment to SystemBlocks or OnBeforeCompact, or
// the key of a composite literal that sets SystemBlocks or IsInjected.
func writerGestures(n ast.Node) []string {
	var out []string
	switch x := n.(type) {
	case *ast.AssignStmt:
		for _, lhs := range x.Lhs {
			if sel, ok := lhs.(*ast.SelectorExpr); ok && (sel.Sel.Name == "SystemBlocks" || sel.Sel.Name == "OnBeforeCompact") {
				out = append(out, sel.Sel.Name)
			}
		}
	case *ast.CompositeLit:
		for _, el := range x.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && (key.Name == "SystemBlocks" || key.Name == "IsInjected") {
				out = append(out, key.Name)
			}
		}
	}
	return out
}

// composedTaskFields parses the composers and returns the Task fields their
// bodies read. Two ways out of a composer are refused rather than followed:
// handing the WHOLE task to a call, and composing through a METHOD on the
// task — each hides its field reads in a body the sweep does not parse, and
// each is exactly how a model-read text would drift past this guard. A
// listed composer is fine on both counts, since the sweep reads it too.
func composedTaskFields(t *testing.T) map[string]bool {
	t.Helper()
	taskType := reflect.TypeOf(delegate.Task{})
	read := map[string]bool{modelReadPass: true}
	for _, fns := range modelReadComposers {
		for _, fn := range fns {
			read[fn] = true
		}
	}
	out := map[string]bool{}
	for file, fns := range modelReadComposers {
		path := filepath.Clean(file)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		found := map[string]bool{}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !slices.Contains(fns, fn.Name.Name) {
				continue
			}
			found[fn.Name.Name] = true
			task := taskIdent(fn, taskType.Name())
			if task == "" {
				t.Fatalf("%s neither names a receiver nor takes a %s parameter: the sweep cannot tell its field reads apart", fn.Name.Name, taskType.Name())
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					id, ok := x.X.(*ast.Ident)
					if !ok || id.Name != task {
						return true
					}
					if _, isField := taskType.FieldByName(x.Sel.Name); isField {
						out[x.Sel.Name] = true
						return true
					}
					if !read[x.Sel.Name] {
						t.Errorf("%s composes through the task's own %s method, whose body the sweep does not read — list it in modelReadComposers, or pass the fields", fn.Name.Name, x.Sel.Name)
					}
				case *ast.CallExpr:
					for _, arg := range x.Args {
						id, ok := arg.(*ast.Ident)
						if !ok || id.Name != task {
							continue
						}
						if callee := calleeName(x); !read[callee] {
							t.Errorf("%s hands the whole task to %s: the sweep can no longer see which fields it composes — pass the fields, or extend modelReadComposers", fn.Name.Name, callee)
						}
					}
				}
				return true
			})
		}
		for _, want := range fns {
			if !found[want] {
				t.Fatalf("%s declares no %s: the composer moved and the sweep read nothing", path, want)
			}
		}
	}
	return out
}

// taskIdent names the task inside fn: its receiver, else the parameter whose
// type is the Task (by name, so both `Task` and `delegate.Task` answer).
func taskIdent(fn *ast.FuncDecl, typeName string) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 && len(fn.Recv.List[0].Names) > 0 {
		return fn.Recv.List[0].Names[0].Name
	}
	if fn.Type.Params == nil {
		return ""
	}
	for _, p := range fn.Type.Params.List {
		typ := p.Type
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X
		}
		name := ""
		switch x := typ.(type) {
		case *ast.Ident:
			name = x.Name
		case *ast.SelectorExpr:
			name = x.Sel.Name
		}
		if name == typeName && len(p.Names) > 0 {
			return p.Names[0].Name
		}
	}
	return ""
}

// calleeName is the name a call expression invokes, for the refusal message.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return "a call"
}

// plantTextLeaves writes sentinel into every string leaf reachable from v
// and records the leaf's path. A shape it cannot see into fails the sweep
// rather than passing a field it never exercised; a kind that carries no
// text (a bool, an int, a func read for its nilness) yields no leaf.
func plantTextLeaves(t *testing.T, v reflect.Value, path, sentinel string, planted map[string]string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(sentinel)
		planted[path] = sentinel
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.Func:
		// A switch the composer reads, not a text it renders.
	case reflect.Slice:
		elem := v.Type().Elem()
		if k := elem.Kind(); k != reflect.String && k != reflect.Struct {
			t.Errorf("the sweep cannot see inside Task.%s (%s): extend plantTextLeaves or the field escapes the boundary unproven", path, v.Type())
			return
		}
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		plantTextLeaves(t, v.Index(0), path+"[]", sentinel, planted)
	case reflect.Struct:
		for i := range v.NumField() {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			plantTextLeaves(t, v.Field(i), path+"."+v.Type().Field(i).Name, sentinel, planted)
		}
	default:
		t.Errorf("the sweep cannot see inside Task.%s (%s): extend plantTextLeaves or the field escapes the boundary unproven", path, v.Type())
	}
}

// readTextLeaf reads back the leaf at path (relative to the field value v).
func readTextLeaf(t *testing.T, v reflect.Value, path string) string {
	t.Helper()
	rest := path
	if i := strings.IndexAny(rest, ".["); i >= 0 {
		rest = rest[i:]
	} else {
		rest = ""
	}
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "[]"):
			if v.Len() == 0 {
				t.Fatalf("Task.%s: the boundary emptied the slice", path)
			}
			v = v.Index(0)
			rest = rest[2:]
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			name := rest
			if i := strings.IndexAny(rest, ".["); i >= 0 {
				name, rest = rest[:i], rest[i:]
			} else {
				rest = ""
			}
			v = v.FieldByName(name)
			if !v.IsValid() {
				t.Fatalf("Task.%s: no field %q", path, name)
			}
		default:
			t.Fatalf("Task.%s: cannot read leaf %q", path, rest)
		}
	}
	return v.String()
}
