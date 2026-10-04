package delegate

import (
	"errors"
	"fmt"
	"testing"
)

// The stderr fixtures below are the #2142 collision class (#1645's measured
// Bun crash, generalised): a JavaScript CLI's crash dump quotes arbitrary
// source — Node echoes the throwing source line at column 0, codeframes
// indent it — and that quoted text can carry a network signature verbatim.
// Matching prose off such a dump re-types a DETERMINISTIC failure as
// transient and burns the retry budget. retypeNetworkError must read only
// the transport tier (matchesTransportStderr) off stderr, while the
// process's own error text keeps the full signature list.
func TestRetypeNetworkErrorScopedByChannel(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: testLogger()}
	opaqueErr := fmt.Errorf("delegate: claude-code failed: session ended without result message (cli_exit_code=1)")

	tests := []struct {
		name   string
		err    error
		stderr string
		want   bool // true = re-typed to *ErrTransient (network)
	}{
		{
			// Measured on opencode 1.1.19 (#1645): a credential-less host
			// dies deterministically with this Bun crash; the quoted prose
			// matches "unexpected eof". Not a transport fault — surface it.
			name:   "bun JSON parse crash quoting prose signature",
			err:    opaqueErr,
			stderr: "error: JSON Parse error: Unexpected EOF\n      at /$bunfs/root/agent:2:20\n      at loadConfig (/$bunfs/root/agent:9:11)\n",
			want:   false,
		},
		{
			// Node's uncaught-exception render echoes the throwing source
			// line at column 0; for a minified bundle that echo can carry
			// the CLI's own error-render literals. Short and unindented,
			// so only the transport-tier restriction excludes it.
			name:   "node column-0 source echo quoting prose signature",
			err:    opaqueErr,
			stderr: "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js:14891\n  throw new Error(`upstream replied ${status} ${statusText || \"internal server error\"}`);\n  ^\nError: upstream connection error while resolving the model list\n",
			want:   false,
		},
		{
			// A codeframe indents the quoted source: an errno string inside
			// quoted source is not a transport fault either.
			name:   "errno only inside quoted source",
			err:    opaqueErr,
			stderr: "  42 |   if (err.code === \"ECONNRESET\") {\n                                ^\nError: Failed to render preview\n      at /x.ts:42:5\n",
			want:   false,
		},
		{
			// The genuine undici crash: the column-0 header names the
			// transport failure, the indented [cause]: chain testifies.
			name:   "undici fetch failed with cause chain",
			err:    opaqueErr,
			stderr: "TypeError: fetch failed\n    at Object.fetch (node:internal/deps/undici/undici:11576:11)\n  [cause]: Error: connect ECONNREFUSED 140.82.112.5:443\n      at TCPConnectWrap.afterConnect [as oncomplete] (node:net:1636:16)\n",
			want:   true,
		},
		{
			name:   "column-0 errno header",
			err:    opaqueErr,
			stderr: "Error: getaddrinfo ENOTFOUND api.anthropic.com\n    at GetAddrInfoReqWrap.onlookup (node:dns:118:26)\n",
			want:   true,
		},
		{
			// The process's own error text keeps the FULL signature list —
			// prose included: this message is the CLI's own render, not a
			// crash dump quoting arbitrary source.
			name:   "prose signature on the error text still retypes",
			err:    fmt.Errorf("delegate: claude-code failed: api error: overloaded"),
			stderr: "",
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := b.retypeNetworkError(tt.err, tt.stderr, Task{NodeID: "n1", Iteration: 1})
			var trans *ErrTransient
			retyped := errors.As(got, &trans)
			if retyped != tt.want {
				t.Fatalf("retypeNetworkError retyped = %v (err=%v), want %v", retyped, got, tt.want)
			}
			if !tt.want {
				return
			}
			if trans.Reason != "network" {
				t.Fatalf("transient reason = %q, want network", trans.Reason)
			}
			var plain *ErrTransient
			if !errors.As(got, &plain) || plain.Detail == "" {
				t.Fatalf("transient detail lost the original error text: %v", got)
			}
		})
	}
}
