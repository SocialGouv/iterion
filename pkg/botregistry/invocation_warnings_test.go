package botregistry

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// The studio's invocation warnings carry every invocation finding the lint
// knows — an undeclared args_var or connector var, a mode no effect honours —
// and only those.
func TestInvocationWarningsCarryEveryInvocationFinding(t *testing.T) {
	e := Entry{Invocations: []bundle.Invocation{
		{Kind: bundle.InvocationKindCommand, ArgsVar: "notes_typo"},
		{Kind: bundle.InvocationKindConnector, Connector: &bundle.InvocationConnector{
			Trigger: "linear.issue.created", Vars: map[string]string{"titel": "event.title"},
		}},
		{Kind: bundle.InvocationKindCommand, Mode: bundle.ExecutionDeliver},
		{Kind: bundle.InvocationKindCommand, ArgsVar: "notes"},
	}}
	vars := &VarsBlock{Fields: []*VarField{{Name: "notes"}, {Name: "title"}}}

	warns := invocationVarWarnings(e, vars)
	joined := strings.Join(warns, "\n")
	for _, want := range []string{`"notes_typo"`, `"titel"`, `"deliver"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning names %s; got:\n%s", want, joined)
		}
	}
	if len(warns) != 3 {
		t.Errorf("got %d warnings, want 3:\n%s", len(warns), joined)
	}
}

// A bundle whose manifest declares two forge invocations of one key is still
// discovered — the manifest loads, the bot is listed, discovery reports
// nothing — and the studio's invocation warnings name the invocation that
// is ignored. Driven through the listing the studio reads, so the witness
// holds the manifest decode and the warning's wiring at once.
func TestInvocationWarningsNameADuplicateForgeKey(t *testing.T) {
	ClearSchemaCache()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "twice", "manifest.yaml"), `name: twice
invocations:
  - kind: forge
    mode: direct
    forge: { event: pull_request, actions: [opened, reopened] }
  - kind: forge
    mode: board
    forge: { event: pull_request, actions: [reopened, opened] }
`)
	writeFile(t, filepath.Join(dir, "twice", "main.bot"), sampleBot)
	writeFile(t, filepath.Join(dir, "once", "manifest.yaml"), `name: once
invocations:
  - kind: forge
    mode: direct
    forge: { event: pull_request, actions: [opened, reopened] }
  - kind: forge
    mode: direct
    forge: { event: pull_request, actions: [synchronize] }
`)
	writeFile(t, filepath.Join(dir, "once", "main.bot"), sampleBot)

	entries, diags, err := ListWithSchemaDiagnostics(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("discovery refused a bundle: %+v", diags)
	}
	byName := map[string]EntryWithSchema{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	twice, ok := byName["twice"]
	if !ok {
		t.Fatalf("the bundle with a repeated forge key is not listed: its manifest no longer loads (%d entries)", len(entries))
	}
	if twice.SchemaError != "" {
		t.Fatalf("schema error on the fixture: %s", twice.SchemaError)
	}
	if len(twice.Invocations) != 2 {
		t.Fatalf("the listed bot carries %d invocations, want both declared", len(twice.Invocations))
	}
	if len(twice.InvocationWarnings) != 1 {
		t.Fatalf("got %d invocation warnings, want the one naming the ignored invocation: %q", len(twice.InvocationWarnings), twice.InvocationWarnings)
	}
	for _, want := range []string{"invocations[1]:", "two forge invocations derive the same key: the second is ignored", "invocations[0]"} {
		if !strings.Contains(twice.InvocationWarnings[0], want) {
			t.Errorf("warning %q does not carry %q", twice.InvocationWarnings[0], want)
		}
	}
	// Two forge invocations that differ are told nothing.
	if once := byName["once"]; len(once.InvocationWarnings) != 0 {
		t.Errorf("a bundle with no repeated forge key is warned: %q", once.InvocationWarnings)
	}
}
