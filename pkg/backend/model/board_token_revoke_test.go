package model

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The per-node host-MCP token's LIFETIME.
//
// The registry's own contract is "Register at the start of every run and Revoke
// at the end", with the TTL as the backstop for a process that died — and
// nothing called Revoke, so every token minted stayed live for 24 h after its
// node returned. Since #2758 that token authenticates the connector facade, so
// what outlived the node was the right to spend the tenant's credentials.
//
// Mutation that reddens this (producer): drop the
// `defer sess.revokeBoardToken(e.boardRevoke)` from executeBackend
// (executor_build_task.go).

// tokenRecordingBackend keeps the token the task carried, so the test can
// prove the SAME token is the one revoked.
type tokenRecordingBackend struct {
	mu    sync.Mutex
	token string
	fail  bool
}

func (b *tokenRecordingBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	b.token = task.BoardRunToken
	b.mu.Unlock()
	if b.fail {
		return delegate.Result{}, errors.New("the node failed")
	}
	return delegate.Result{Output: map[string]any{"ok": true}}, nil
}

func (b *tokenRecordingBackend) seen() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.token
}

func connectorsNode() *ir.AgentNode {
	n := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "agent"},
		LLMFields: ir.LLMFields{Backend: "crossing"},
	}
	n.Capabilities = []string{connection.CapConnectors}
	return n
}

func executorWithBoardTokens(t *testing.T, b delegate.Backend, minted *[]string, revoked *[]string) *ClawExecutor {
	t.Helper()
	reg := delegate.NewRegistry()
	reg.Register("crossing", b)
	n := 0
	e := NewClawExecutor(NewRegistry(),
		&ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}},
		WithBackendRegistry(reg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1}),
		WithBoardRegister(func([]string, string) string {
			n++
			tok := "run-token-" + string(rune('a'+n-1))
			*minted = append(*minted, tok)
			return tok
		}),
		WithBoardRevoke(func(token string) { *revoked = append(*revoked, token) }),
	)
	e.SetSandbox(unmetSandboxRun{})
	e.SetBoardEndpoint("http://127.0.0.1:1/api/v1/mcp/board")
	return e
}

func TestTheNodesHostMCPTokenIsRevokedWhenTheNodeEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail bool
	}{
		// BOTH outcomes: a node that failed is exactly the one whose token
		// an operator would never think to revoke by hand.
		{"a node that succeeded", false},
		{"a node that failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &tokenRecordingBackend{fail: tc.fail}
			var minted, revoked []string
			e := executorWithBoardTokens(t, b, &minted, &revoked)

			_, err := e.executeBackend(context.Background(), connectorsNode(), map[string]any{})
			if tc.fail && err == nil {
				t.Fatal("the stub was asked to fail")
			}
			if !tc.fail && err != nil {
				t.Fatalf("executeBackend: %v", err)
			}
			if len(minted) != 1 {
				t.Fatalf("tokens minted = %v, want exactly one for the node", minted)
			}
			if got := b.seen(); got != minted[0] {
				t.Fatalf("the task carried %q, the minter handed out %q", got, minted[0])
			}
			if len(revoked) != 1 || revoked[0] != minted[0] {
				t.Errorf("revoked = %v, want exactly the token the node held (%q) — a live token outliving its node spends the tenant's credentials for the registry's whole TTL",
					revoked, minted[0])
			}
		})
	}
}

// Two nodes in a row must each end their OWN grant: one revoke for two tokens
// would leave the first live, and a revoke of the first at the second's end
// would be a token still live while its node was gone.
func TestTwoNodesEachEndTheirOwnGrant(t *testing.T) {
	b := &tokenRecordingBackend{}
	var minted, revoked []string
	e := executorWithBoardTokens(t, b, &minted, &revoked)

	for i := 0; i < 2; i++ {
		if _, err := e.executeBackend(context.Background(), connectorsNode(), map[string]any{}); err != nil {
			t.Fatalf("executeBackend %d: %v", i, err)
		}
	}
	if len(minted) != 2 {
		t.Fatalf("tokens minted = %v, want one per node", minted)
	}
	if len(revoked) != 2 || revoked[0] != minted[0] || revoked[1] != minted[1] {
		t.Errorf("revoked = %v, want %v in order — each node's grant ends with that node", revoked, minted)
	}
}
