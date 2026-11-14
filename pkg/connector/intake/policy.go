package intake

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Policy paces one state machine's retries.
type Policy struct {
	// MaxAttempts is how many attempts a row gets before its dead letter;
	// 0 removes the bound — a row retries until it succeeds or an operator
	// acts.
	MaxAttempts int
	// Backoff is the delay after the first failed attempt; each further one
	// doubles it, up to BackoffMax.
	Backoff    time.Duration
	BackoffMax time.Duration
	// ClaimTTL is how long a claim holds its row. The work under a claim is
	// bounded to half of it (Worker), so a worker that overruns has returned
	// before another can take the row.
	ClaimTTL time.Duration
}

// DefaultPolicy is the pacing both machines start from.
var DefaultPolicy = Policy{MaxAttempts: 5, Backoff: 15 * time.Second, BackoffMax: 10 * time.Minute, ClaimTTL: 2 * time.Minute}

// Delay is the backoff before the attempt after attempt n (1-based).
func (p Policy) Delay(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := p.Backoff
	for i := 1; i < n && d < p.BackoffMax; i++ {
		d *= 2
	}
	if d > p.BackoffMax {
		d = p.BackoffMax
	}
	return d
}

// exhausted reports whether attempts spent the policy's budget.
func (p Policy) exhausted(attempts int) bool {
	return p.MaxAttempts > 0 && attempts >= p.MaxAttempts
}

// Validate refuses a policy no worker could run.
func (p Policy) Validate() error {
	switch {
	case p.MaxAttempts < 0:
		return fmt.Errorf("intake: max attempts %d", p.MaxAttempts)
	case p.Backoff <= 0:
		return fmt.Errorf("intake: backoff %s", p.Backoff)
	case p.BackoffMax < p.Backoff:
		return fmt.Errorf("intake: backoff ceiling %s below the backoff %s", p.BackoffMax, p.Backoff)
	case p.ClaimTTL <= 0:
		return fmt.Errorf("intake: claim ttl %s", p.ClaimTTL)
	}
	return nil
}

// The operator's dials. Each machine reads its own family, so pairs — which
// spend a launch — and intakes — which spend an evaluation — are paced apart.
const (
	EnvPairPrefix   = "ITERION_CONNECTOR_PAIR_"
	EnvIntakePrefix = "ITERION_CONNECTOR_INTAKE_"
	// EnvRetention is how long an intake and its pairs are kept (a Go
	// duration; 0 keeps them).
	EnvRetention = "ITERION_CONNECTOR_INTAKE_RETENTION"
)

// DefaultRetention keeps an intake and its pairs thirty days: the dedup
// window a vendor's redelivery meets, and the dead-letter view's horizon.
const DefaultRetention = 30 * 24 * time.Hour

// PolicyFromEnv reads prefix + MAX_ATTEMPTS, BACKOFF, BACKOFF_MAX and
// CLAIM_TTL over DefaultPolicy. A value that does not parse is an error
// naming the variable — never a silent default.
func PolicyFromEnv(prefix string, getenv func(string) string) (Policy, error) {
	p := DefaultPolicy
	if v := strings.TrimSpace(getenv(prefix + "MAX_ATTEMPTS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Policy{}, fmt.Errorf("intake: %sMAX_ATTEMPTS=%q: want a count, 0 for no bound", prefix, v)
		}
		p.MaxAttempts = n
	}
	for _, d := range []struct {
		name string
		into *time.Duration
	}{
		{"BACKOFF", &p.Backoff},
		{"BACKOFF_MAX", &p.BackoffMax},
		{"CLAIM_TTL", &p.ClaimTTL},
	} {
		v := strings.TrimSpace(getenv(prefix + d.name))
		if v == "" {
			continue
		}
		parsed, err := time.ParseDuration(v)
		if err != nil || parsed <= 0 {
			return Policy{}, fmt.Errorf("intake: %s%s=%q: want a positive Go duration", prefix, d.name, v)
		}
		*d.into = parsed
	}
	if err := p.Validate(); err != nil {
		return Policy{}, fmt.Errorf("intake: %s*: %w", prefix, err)
	}
	return p, nil
}

// RetentionFromEnv reads EnvRetention over DefaultRetention; 0 keeps rows
// forever.
func RetentionFromEnv(getenv func(string) string) (time.Duration, error) {
	v := strings.TrimSpace(getenv(EnvRetention))
	if v == "" {
		return DefaultRetention, nil
	}
	if v == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("intake: %s=%q: want a Go duration, 0 to keep rows", EnvRetention, v)
	}
	return d, nil
}

// ExpiryFor is when a row received at receivedAt expires under retention;
// nil when retention keeps it.
func ExpiryFor(receivedAt time.Time, retention time.Duration) *time.Time {
	if retention <= 0 {
		return nil
	}
	t := stamp(receivedAt.Add(retention))
	return &t
}
