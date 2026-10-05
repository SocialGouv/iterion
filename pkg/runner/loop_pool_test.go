package runner

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/queue"
)

// The pure admission rule: "" when the three stamps agree (pod serves msg,
// doc frozen = msg), otherwise the mismatch reason. Every leg reddens its
// own revert:
//   - dropping the pod leg admits a foreign-pool delivery;
//   - dropping the document leg admits a corrupted publish;
//   - an unstamped message on the shared pod stays admitted (the default).
func TestPoolAdmissionDecision(t *testing.T) {
	tests := []struct {
		name string
		pod  string
		msg  string
		doc  string
		want string
	}{
		{"agreed stamps", "honorabilite", "honorabilite", "honorabilite", ""},
		{"shared pod admits unstamped", "", "", "", ""},
		{"foreign pool", "honorabilite", "other-pool", "other-pool", "message stamped \"other-pool\", pod serves \"honorabilite\""},
		{"unstamped msg on pool pod", "honorabilite", "", "honorabilite", "message stamped \"\", pod serves \"honorabilite\""},
		{"doc disagrees with message", "honorabilite", "honorabilite", "", "message stamped \"honorabilite\", document frozen \"\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := poolAdmissionDecision(tc.pod, tc.msg, tc.doc); got != tc.want {
				t.Fatalf("decision = %q, want %q", got, tc.want)
			}
		})
	}
}

// The disposition is the admission-mismatch handler with the pool kind,
// which is ALWAYS final (the stamps cannot agree on redelivery).
func TestPlanAdmissionMismatchPool(t *testing.T) {
	plan := planAdmissionMismatch(admissionMismatchPool,
		errors.New("message stamped other-pool, pod serves honorabilite"),
		0, queue.Envelope{RunID: "run-p"}, 1, 8)
	if !plan.final {
		t.Fatal("a pool mismatch must ALWAYS park (redelivery cannot reconcile the stamps)")
	}
	if !strings.Contains(plan.parkedRunError, "relaunched on the pool") {
		t.Fatalf("the parked error must name the remedy: %q", plan.parkedRunError)
	}
}

// The load-bearing call sites are pinned by source assertion (rva F1): the
// pool admission runs in processOne next to the tenant admission, and the
// production publisher config wires the registry — both deletable with
// zero red otherwise (the round-1 findings).
func TestPoolAdmissionCallSitesArePinned(t *testing.T) {
	loop, err := os.ReadFile("loop.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(loop), "if !r.verifyPoolOrTerm(pre, msg, delivery, logger) {") {
		t.Fatal("processOne no longer calls verifyPoolOrTerm — the pool admission is off")
	}
	if !strings.Contains(string(loop), "if !r.verifyTenantOrTerm(pre, msg, delivery, logger) {") {
		t.Fatal("processOne no longer calls verifyTenantOrTerm — pin dropped")
	}
	cfg, err := os.ReadFile("../../cmd/iterion/server.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "RunnerPools:                stores.runnerPools,") {
		t.Fatal("the production publisher config no longer wires the runner-pool registry — pool dispatch would be refused everywhere")
	}
}
