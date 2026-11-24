package server

import (
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/intake"
)

// The worker reads its policies and its tick from the dials; a dial that
// does not parse is an error naming it, never a silent default.
func TestTheConnectorWorkerReadsItsDials(t *testing.T) {
	s := newWebhookTestServer(t)
	s.connectorIntakes = intake.NewMemoryStore()
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

	w, every, err := s.connectorWorker(env(nil))
	if err != nil || every != defaultConnectorWorkerInterval || w.Pairs != intake.DefaultPolicy || w.Intakes != intake.DefaultPolicy {
		t.Fatalf("no dial = %v, %s, %+v; want the defaults", err, every, w)
	}
	if !strings.Contains(w.Owner, ":") || w.Matcher == nil || w.Submitter == nil || w.Store == nil {
		t.Fatalf("worker = %+v, want an owner and its three parts", w)
	}

	w, every, err = s.connectorWorker(env(map[string]string{
		EnvConnectorWorkerInterval:            "500ms",
		intake.EnvPairPrefix + "MAX_ATTEMPTS": "3",
		intake.EnvIntakePrefix + "CLAIM_TTL":  "90s",
	}))
	if err != nil || every != 500*time.Millisecond || w.Pairs.MaxAttempts != 3 || w.Intakes.ClaimTTL != 90*time.Second {
		t.Fatalf("dials = %v, %s, pairs %+v intakes %+v", err, every, w.Pairs, w.Intakes)
	}

	for name, kv := range map[string]map[string]string{
		"interval": {EnvConnectorWorkerInterval: "soon"},
		"zero":     {EnvConnectorWorkerInterval: "0s"},
		"pairs":    {intake.EnvPairPrefix + "BACKOFF": "fast"},
		"intakes":  {intake.EnvIntakePrefix + "MAX_ATTEMPTS": "-1"},
	} {
		if _, _, err := s.connectorWorker(env(kv)); err == nil {
			t.Errorf("%s: a dial that does not parse built a worker", name)
		}
	}
}
