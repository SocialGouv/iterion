package errtrack

import (
	"math"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/log"
)

// EnvTracesSampleRate is the tracing dial: the fraction of units of
// work (an API request, one LLM generation) recorded as a Sentry
// transaction, in [0, 1].
//
// It is a SECOND opt-in on top of SENTRY_DSN, and the SDK does not read
// it on its own — Init does, below. Unset, 0, or unparsable ⇒ tracing is
// strictly off even with a DSN configured, because a transaction per
// request is a cost an operator opts into deliberately.
const EnvTracesSampleRate = "SENTRY_TRACES_SAMPLE_RATE"

// tracing mirrors "the installed client has tracing enabled". Separate
// from enabled so a hot seam can skip building a span with one atomic
// load, without asking the SDK.
var tracing atomic.Bool

// TracingEnabled reports whether transactions/spans are being recorded.
// Guard every hand-made span with it: with tracing off a span costs
// nothing because it is never created.
//
// It implies Enabled() — tracing rides the same client and DSN.
func TracingEnabled() bool { return tracing.Load() }

// resolveTracesSampleRate reads the sampling dial, in Config-then-env
// precedence, and returns 0 (off) for anything it refuses.
//
// Only called once a DSN is configured, so an operator with tracking
// off never sees a line about a var they did not set.
func resolveTracesSampleRate(cfg Config, logger *log.Logger) float64 {
	if cfg.TracesSampleRate != nil {
		return validSampleRate(*cfg.TracesSampleRate, "Config.TracesSampleRate", logger)
	}
	raw := strings.TrimSpace(os.Getenv(EnvTracesSampleRate))
	if raw == "" {
		return 0
	}
	rate, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		logger.Error("errtrack: %s=%q is not a number — tracing disabled", EnvTracesSampleRate, Redact(raw))
		return 0
	}
	return validSampleRate(rate, EnvTracesSampleRate, logger)
}

// validSampleRate returns rate when it is a usable fraction and 0
// otherwise, reporting the refusal loudly. The comparison is written as
// a negated conjunction on purpose: NaN fails every ordered comparison,
// so `rate < 0 || rate > 1` would let it through and hand the SDK a
// sampler that decides nothing.
func validSampleRate(rate float64, source string, logger *log.Logger) float64 {
	if math.IsNaN(rate) || !(rate >= 0 && rate <= 1) {
		logger.Error("errtrack: %s=%v is outside [0,1] — tracing disabled", source, rate)
		return 0
	}
	return rate
}

// scrubSpanInPlace redacts the operator-supplied surfaces of one span.
// Called from scrubEvent for every span of a transaction event, which
// reaches the wire through BeforeSendTransaction — a hook BeforeSend
// never sees.
func scrubSpanInPlace(s *sentry.Span) {
	if s == nil {
		return
	}
	s.Name = Redact(s.Name)
	s.Op = Redact(s.Op)
	s.Description = Redact(s.Description)
	for k, v := range s.Tags {
		if isSensitiveKey(k) {
			s.Tags[k] = redacted
			continue
		}
		s.Tags[k] = Redact(v)
	}
	if len(s.Data) > 0 {
		s.Data = scrubFields(s.Data)
	}
}
