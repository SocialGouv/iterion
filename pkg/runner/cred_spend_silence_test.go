package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credusage"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// spendProbe wires a runner whose log is capturable and whose counter is
// observable, plus an emitter carrying one real claude_code delegate route.
func spendProbe(t *testing.T, counter credusage.Counter) (*Runner, *metricsEmitter, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelWarn, &buf), CredUsage: counter}}
	usage := newMetricsEmitter(nil, nil)
	usage.observe(store.Event{Type: store.EventLLMRequest, NodeID: "n",
		Data: map[string]any{"model": "anthropic/claude-opus-5"}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "n",
		Data: map[string]any{"backend": delegate.BackendClaudeCode, "tokens": float64(36051), "cost_usd": 2.4}})
	return r, usage, &buf
}

// #1087 — a credential whose slot resolves but carries NO fingerprint had its
// spend dropped by a bare `continue`. Its sibling decline one line above (no
// slot at all) warns; this one said nothing, so a run that really spent money
// left no trace anywhere: the counter did not move and the log did not mention
// it. Measured in production as a per-credential row frozen on every field —
// runs, cost and tokens — while runs kept completing.
func TestRecordCredentialSpend_UnfingerprintedSlotIsNotSilent(t *testing.T) {
	counter := credusage.NewMemoryCounter()
	r, usage, logbuf := spendProbe(t, counter)

	// A resolved claude_code forfait with NO fingerprint stamped: exactly the
	// shape `creds.Fingerprint(slot) == ""` describes.
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{delegate.BackendClaudeCode: "/tmp/oauth"},
		Fingerprints:         map[string]string{delegate.BackendClaudeCode: ""},
	})

	now := time.Now().UTC()
	r.recordCredentialSpend(ctx, &queue.RunMessage{RunID: "run-1", TenantID: "team-a"}, usage, now)

	rows, err := counter.List(ctx, now, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("an unfingerprinted slot must not be metered (it would merge every unstamped credential): %+v", rows)
	}
	got := logbuf.String()
	if !strings.Contains(got, "run-1") {
		t.Errorf("the dropped spend named no run in the log — a run that spent $2.40 vanished silently.\nlog: %q", got)
	}
	if !strings.Contains(strings.ToLower(got), "fingerprint") {
		t.Errorf("the log does not say the credential had no fingerprint, so nobody can diagnose it.\nlog: %q", got)
	}
}

// The three function-level guards return before any route is examined. Each
// skips the whole attempt's per-credential metering; none said anything.
func TestRecordCredentialSpend_EarlyDeclinesAreLogged(t *testing.T) {
	fullCreds := secrets.Credentials{
		OAuthCredentialFiles: map[string]string{delegate.BackendClaudeCode: "/tmp/oauth"},
		Fingerprints:         map[string]string{delegate.BackendClaudeCode: "fp-1"},
	}

	t.Run("no counter wired", func(t *testing.T) {
		_, usage, logbuf := spendProbe(t, nil)
		r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelWarn, logbuf)}}
		r.recordCredentialSpend(secrets.WithCredentials(context.Background(), fullCreds),
			&queue.RunMessage{RunID: "run-2", TenantID: "t"}, usage, time.Now().UTC())
		if !strings.Contains(logbuf.String(), "run-2") {
			t.Errorf("a runner with no credential counter meters nothing and says nothing.\nlog: %q", logbuf.String())
		}
	})

	t.Run("no credentials on the context", func(t *testing.T) {
		r, usage, logbuf := spendProbe(t, credusage.NewMemoryCounter())
		r.recordCredentialSpend(context.Background(),
			&queue.RunMessage{RunID: "run-3", TenantID: "t"}, usage, time.Now().UTC())
		if !strings.Contains(logbuf.String(), "run-3") {
			t.Errorf("a spend with no credentials in context vanished silently.\nlog: %q", logbuf.String())
		}
	})

	t.Run("no routes observed", func(t *testing.T) {
		r, _, logbuf := spendProbe(t, credusage.NewMemoryCounter())
		empty := newMetricsEmitter(nil, nil)
		r.recordCredentialSpend(secrets.WithCredentials(context.Background(), fullCreds),
			&queue.RunMessage{RunID: "run-4", TenantID: "t"}, empty, time.Now().UTC())
		// A run that consumed nothing is the ordinary case and must stay
		// quiet — the guard is only worth a line when there WAS spend.
		if strings.Contains(logbuf.String(), "run-4") {
			t.Errorf("a run with no observed route is not an anomaly and must not warn.\nlog: %q", logbuf.String())
		}
	})
}
