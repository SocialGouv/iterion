package server

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The server-wide admission control on DSL readings (#2693).
//
// The read budget (#1690, ITERION_DSL_READ_BUDGET) bounds ONE reading: a
// single budget-edge parse costs ~400–530 MiB of RSS. Nothing bounded the
// PROCESS across concurrent readings — three concurrent budget-edge
// /api/parse requests measured ~1.4 GiB, past the chart's default 1Gi server
// limit, and the pod's OOM-kill interrupts every run and stream it owns.
// This gate bounds the AGGREGATE: every DSL reading the server performs for
// a request (parse, unparse, validate, the bot-source writes) acquires a
// weight proportional to its input's size, and a request that would push the
// aggregate past the ceiling waits a bounded grace, then is refused 503 +
// Retry-After — the shape the authoring lock's TryLockContext timeout
// already answers. The ceiling is per replica by design: N replicas admit
// N × the ceiling, which is the per-pod OOM the ticket scopes.

const (
	// EnvReadAdmissionMaxBytes is the operator's override of the aggregate
	// ceiling, in weighted bytes (the admission weight, not RSS). 0 removes
	// the bound — the greppable escape hatch every load-bearing limit keeps.
	EnvReadAdmissionMaxBytes = "ITERION_DSL_READ_ADMISSION_MAX_BYTES"
	// EnvReadAdmissionWait is how long a request past the ceiling waits for
	// headroom before its 503 (a Go duration; 0 refuses immediately).
	EnvReadAdmissionWait = "ITERION_DSL_READ_ADMISSION_WAIT"

	// defaultReadAdmissionMaxBytes leaves half of the chart's default 1Gi
	// server limit to everything else the pod does — one budget-edge
	// reading (~533 MiB measured) plus small concurrent ones stay under it,
	// two budget-edge readings do not overlap.
	defaultReadAdmissionMaxBytes = 512 << 20
	// defaultReadAdmissionWait is the grace a request past the ceiling gets
	// before its 503: long enough for a budget-edge reading ahead of it to
	// finish (a few seconds), short enough for an editor to stay responsive.
	defaultReadAdmissionWait = 2 * time.Second

	// readWeightPerInputByte is the weight one byte of input carries — the
	// measured worst-case memory amplification of a reading. The read budget
	// counts records (default 2^20) and a record costs ~500 bytes of live
	// memory at the budget's edge; a record needs at least a byte of source
	// (a dense shape — a line of one-letter names, a run of commas), so one
	// input byte can grow to ~1 KiB of heap. Small inputs overestimate (a
	// normal 100 KiB bot weighs 50 MiB against a real cost a tenth of
	// that), which is the conservative direction: the bound exists for the
	// dense edge, not to throttle ordinary edits.
	readWeightPerInputByte = 512
)

// readAdmissionGate is the weighted semaphore itself: the ceiling in
// weighted bytes and the refusal grace.
type readAdmissionGate struct {
	sem      *semaphore.Weighted
	capacity int64
	wait     time.Duration
	// refusalLog throttles the refusal log line: under a deliberate flood
	// the 503s are the answer, one line each would be the attack's second
	// front.
	lastRefusalLog atomicTime
}

// atomicTime is a unix-nano timestamp behind a mutex — written rarely (the
// throttled refusal log), so no atomic trickery is worth its readability.
type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) swapIfOlder(now time.Time, window time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Sub(a.t) < window {
		return false
	}
	a.t = now
	return true
}

// readAdmissionFromEnv resolves the gate from the environment. A zero
// ceiling is the operator's explicit removal of the bound (nil gate);
// an invalid value keeps the default and says so once, as an error no log
// level silences — the read budget's own discipline (pkg/dsl/readbudget).
func readAdmissionFromEnv() *readAdmissionGate {
	maxBytes := int64(defaultReadAdmissionMaxBytes)
	if v := strings.TrimSpace(envOperator(EnvReadAdmissionMaxBytes)); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		switch {
		case err != nil || n < 0:
			iterlog.NewFromEnv(os.Stderr).Error("server: invalid %s=%q (a count of weighted bytes, 0 to disable); using the default %d", EnvReadAdmissionMaxBytes, v, defaultReadAdmissionMaxBytes)
		case n == 0:
			return nil
		default:
			maxBytes = n
		}
	}
	wait := defaultReadAdmissionWait
	if v := strings.TrimSpace(envOperator(EnvReadAdmissionWait)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			iterlog.NewFromEnv(os.Stderr).Error("server: invalid %s=%q (a Go duration, 0 to refuse immediately); using the default %s", EnvReadAdmissionWait, v, defaultReadAdmissionWait)
		} else {
			wait = d
		}
	}
	return &readAdmissionGate{sem: semaphore.NewWeighted(maxBytes), capacity: maxBytes, wait: wait}
}

// admissionWeight maps an input's size to its share of the ceiling.
func admissionWeight(inputBytes int64) int64 {
	if inputBytes <= 0 {
		return 1
	}
	// Inputs are body-capped (10–16 MB by the routes), so the product cannot
	// overflow int64.
	return inputBytes * readWeightPerInputByte
}

// admit takes inputBytes' share of the ceiling, blocking at most the
// refusal grace. refused means the caller owes the caller a 503. An input
// whose weight exceeds the WHOLE ceiling — a legitimate big bot the read
// budget itself admits — is admitted alone once the gate drains (its share
// clamps to the ceiling, said at info), never refused outright: the bound
// exists against CONCURRENCY, not against one large reading.
func (g *readAdmissionGate) admit(ctx context.Context, inputBytes int64, info func(string, ...any)) (release func(), refused bool) {
	w := admissionWeight(inputBytes)
	if w > g.capacity {
		if info != nil {
			info("server: DSL reading of %d input bytes exceeds the admission ceiling (%d weighted bytes) alone — admitted by itself once concurrent readings drain (%s)", inputBytes, g.capacity, EnvReadAdmissionMaxBytes)
		}
		w = g.capacity
	}
	if g.wait <= 0 {
		// No grace: the answer is immediate — the slot or the refusal.
		if !g.sem.TryAcquire(w) {
			return nil, true
		}
		return func() { g.sem.Release(w) }, false
	}
	actx, cancel := context.WithTimeout(ctx, g.wait)
	defer cancel()
	if err := g.sem.Acquire(actx, w); err != nil {
		return nil, true
	}
	return func() { g.sem.Release(w) }, false
}

// admitDSLRead gates one request's DSL reading on the server-wide aggregate
// bound (#2693). On refusal it has already written 503 + Retry-After; the
// caller returns. The release MUST run when the reading ends (defer).
// A nil gate (the operator's 0) is a no-op admission.
func (s *Server) admitDSLRead(w http.ResponseWriter, r *http.Request, inputBytes int64) (func(), bool) {
	g := s.readAdmission
	if g == nil {
		return func() {}, true
	}
	release, refused := g.admit(r.Context(), inputBytes, func(format string, args ...any) {
		s.logger.Info(format, args...)
	})
	if !refused {
		return release, true
	}
	retryAfter := int(g.wait / time.Second)
	if retryAfter < 1 {
		retryAfter = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	s.httpErrorCode(w, r, http.StatusServiceUnavailable, "dsl_read_admission",
		"the server's concurrent DSL readings are at their aggregate memory bound — retry in a few seconds (operators: %s, 0 disables; %s tunes the grace)",
		EnvReadAdmissionMaxBytes, EnvReadAdmissionWait)
	if g.lastRefusalLog.swapIfOlder(time.Now(), 10*time.Second) {
		s.logger.Warn("server: refusing DSL readings with 503 — the aggregate admission bound is reached (%s holds the ceiling)", EnvReadAdmissionMaxBytes)
	}
	return nil, false
}

// mapBytes sums the sizes of a files map's contents — the input size of a
// bundle-shaped request.
func mapBytes(files map[string]string) int64 {
	var total int64
	for _, content := range files {
		total += int64(len(content))
	}
	return total
}

// diskUnitBytes sums the sizes of the workflow files under the directory of
// a workspace path — the input a unit load of it reads (its main and the
// fragments its imports reach). 0 when the path resolves to nothing the
// gate can weigh (the load's own error then answers). The walk caps itself
// at 64 MiB summed: past that the estimate is far past any ceiling and
// precision buys nothing.
func (s *Server) diskUnitBytes(path string) int64 {
	if path == "" {
		return 0
	}
	abs, err := s.safePath(path)
	if err != nil {
		return 0
	}
	root := filepath.Dir(abs)
	var total int64
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !workflowfile.IsWorkflowFile(d.Name()) {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		if total > 64<<20 {
			return fs.SkipAll
		}
		return nil
	})
	return total
}
