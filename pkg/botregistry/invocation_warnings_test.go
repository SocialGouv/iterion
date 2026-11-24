package botregistry

import (
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
