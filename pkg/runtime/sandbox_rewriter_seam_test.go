package runtime

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The runtime hands the executor's guard to the egress proxy through a
// structural interface asserted at run time: an executor that stopped
// satisfying it would turn inspection off with every suite still green.
// The assertion below fails the build instead.
var _ secretEgressRewriter = (*model.ClawExecutor)(nil)

// A ClawExecutor carrying a scoped secret is the proxy's rewriter: it
// substitutes toward the secret's host, refuses a result past the limit, and
// its content DLP fires toward another host.
func TestTheClawExecutorIsTheProxysRewriter(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_TLS_INSPECT", "")
	value := "FAKEVALUE-" + strings.Repeat("z", 30)
	ph := secretguard.PlaceholderForName("tok")
	g := secretguard.New([]secretguard.Secret{{Name: "tok", Value: value, Placeholder: ph, Hosts: []string{"api.example.com"}}}, secretguard.DefaultConfig())
	wf := &ir.Workflow{}
	rw := New(wf, nil, model.NewClawExecutor(nil, wf, model.WithSecretGuard(g))).resolveSecretRewriter()
	if rw == nil {
		t.Fatal("no rewriter for a ClawExecutor with secrets: inspection would be off")
	}
	if out, ok := rw.MaterializeForHostWithin("Bearer "+ph, "api.example.com", 1<<20); !ok || out != "Bearer "+value {
		t.Errorf("substitution toward the secret's host: ok %v, %d bytes", ok, len(out))
	}
	if out, ok := rw.MaterializeForHostWithin("Bearer "+ph, "api.example.com", len("Bearer "+value)-1); ok || out != "Bearer "+ph {
		t.Errorf("a result past the limit: ok %v, %d bytes; want the input back, refused", ok, len(out))
	}
	if !rw.ExfiltratesTo("x "+value, "other.example") {
		t.Error("content DLP toward another host did not fire")
	}
}
