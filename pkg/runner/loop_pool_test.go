package runner

import (
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

type fakePoolDelivery struct{ termed bool }

func (f *fakePoolDelivery) Ack() error                       { return nil }
func (f *fakePoolDelivery) Nak() error                       { return nil }
func (f *fakePoolDelivery) NakWithDelay(time.Duration) error { return nil }
func (f *fakePoolDelivery) Term() error                      { f.termed = true; return nil }
func (f *fakePoolDelivery) NumDelivered() int                { return 1 }

// The pool admission: the pod serves ONE pool, and the three stamps —
// message, frozen document, pod — must agree, or the delivery is Termed
// (never executed). Each leg reddens its own revert:
//   - dropping the pod leg admits a foreign-pool delivery;
//   - dropping the document leg admits a corrupted publish;
//   - an unstamped message on the shared pod stays admitted (the default).
func TestVerifyPoolOrTerm(t *testing.T) {
	logger := iterlog.Nop()
	tests := []struct {
		name     string
		pod      string
		msg      string
		doc      string
		want     bool
		wantTerm bool
	}{
		{"agreed stamps proceed", "honorabilite", "honorabilite", "honorabilite", true, false},
		{"shared pod admits unstamped", "", "", "", true, false},
		{"foreign pool termed", "honorabilite", "other-pool", "other-pool", false, true},
		{"unstamped msg on pool pod termed", "honorabilite", "", "honorabilite", false, true},
		{"doc disagrees with message termed", "honorabilite", "honorabilite", "", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{cfg: Config{RunnerPool: tc.pod, Logger: logger}}
			msg := &queue.RunMessage{RunID: "run-p", RunnerPool: tc.msg}
			pre := preconditionOutcome{preRun: &store.Run{ID: "run-p", RunnerPool: tc.doc}}
			delivery := &fakePoolDelivery{}
			got := r.verifyPoolOrTerm(pre, msg, delivery, logger)
			if got != tc.want {
				t.Fatalf("proceed = %v, want %v", got, tc.want)
			}
			if delivery.termed != tc.wantTerm {
				t.Fatalf("termed = %v, want %v", delivery.termed, tc.wantTerm)
			}
		})
	}
}
