package credparity

import (
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// RecoveryRows is the parity table of the call no site names
// (model.CallSites): the structured-output recovery, the direct claw
// generation the executor makes on the detector's pick once a node's answer
// failed its schema and its corrections ran out — in process on the host,
// beside the node's own dispatch. The admission and the pre-flight never
// judge it, by design: it fires on an answer that already failed, and is
// skipped where nothing funds it. Its readers are the dispatch (the
// credential its request carries), the spend ledger and the plan's fine
// table, which book its own delegate event (`reask: "extract"`) on the
// credential keys that event carries (ADR-161 decision 8) — the run slot the
// recovery spent, never the node's row by prediction.
//
// Each row's node answers prose its schema refuses, twice; the pod's
// Anthropic key is what the detector reads, so the recovery sends the
// detector's anthropic pick.
func RecoveryRows() []Row {
	pod := map[string]string{"ANTHROPIC_API_KEY": PodAnthropic}
	return []Row{
		{
			// SEEN RED (#3069 rebased under ADR-161): the recovery's own
			// delegate event carried no credential keys, and the meter
			// booked it on nobody — the donor whose lent key paid for it
			// was under-booked.
			Name:    "the structured-output recovery on a run's own Anthropic key",
			Origin:  "#3069 × ADR-161 decision 8 (the recovery's delegate event carried no credential keys; the fine table booked it on nobody)",
			Posture: pod,
			Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderAnthropic: RunAnthropic}, Plan: true},
			Node:    onBackend("claude_code", claudeSpec),
			Role:    AuxiliaryRole,
			Trust:   Trusted,
			Want:    Spent(0, RunKey, string(secrets.ProviderAnthropic), RunAnthropic),
		},
		{
			// The pod's key funds the recovery beside a codex node the plan
			// books on the run's OpenAI key: nobody is booked for it, and it
			// is no miss — the fine table stands.
			Name:    "the structured-output recovery the pod's key funds beside a planned codex node",
			Origin:  "#3069 R11 P3b (the detected provider is not in the node's plan) under ADR-161 decision 8",
			Posture: pod,
			Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderOpenAI: RunOpenAI}, Plan: true},
			Node:    onBackend("codex", "openai/gpt-5.4"),
			Role:    AuxiliaryRole,
			Trust:   Trusted,
			Want:    Spent(0, PodEnv, "", PodAnthropic),
		},
		{
			// The stamp binds the recovery as it binds the node: the pod
			// alone would fund it, so it sends nothing and books nothing.
			Name:    "the structured-output recovery a stamped run's pod alone would fund",
			Origin:  "ADR-131 §7 × #3069 (the recovery resolves through the claw arm, under the stamp)",
			Posture: pod,
			Run:     Run{Keys: map[secrets.Provider]string{secrets.ProviderZAI: RunZAI}, Stamped: true},
			Node:    onBackend("claude_code", claudeSpec),
			Role:    AuxiliaryRole,
			Trust:   Trusted,
			Want:    Refuses(AmbientStamp),
		},
	}
}
