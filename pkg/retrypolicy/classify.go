package retrypolicy

import "github.com/SocialGouv/iterion/pkg/store"

// ---------------------------------------------------------------------------
// The automatic-resume classification.
//
// Policy above answers "how long may we wait, and how many times". This
// answers the question that comes FIRST: can re-executing change anything at
// all?
//
// It exists as ONE table because the two surfaces that decide it used to
// disagree. The CLI's `--auto-resume` loop held an allow-list of codes and
// refused everything else; the cloud runner held none, naked every generic
// failure back to JetStream, and let the redelivery synthesise a resume for
// any ENGINE code. Measured on 2026-09-06 (run 01a07804): a `compute` node
// whose expression the runner's evaluator could not satisfy failed
// identically seven times in ten minutes — seven pods, seven clones, seven
// sandboxes — because nothing on the cloud path asked whether a re-execution
// could possibly differ.
//
// The bar for DispositionDeterministic is deliberately high: the failing
// step must be unable to produce a different result from the SAME
// checkpoint. A resume re-executes the failing node, so anything decided by
// an LLM node's output can differ on the next attempt and is NOT
// deterministic, however permanent it looks — see DispositionReexecutable.
// ---------------------------------------------------------------------------

// Disposition is what an automatic resume can hope to achieve for a failure
// wearing a given code.
type Disposition uint8

const (
	// DispositionUnknown: the empty code, or one this binary does not know.
	// Zero means UNKNOWN, never "safe to retry" and never "hopeless": each
	// caller keeps whatever it did before for an unclassified failure —
	// changing that silently would either strand legacy rows or re-open the
	// loop this table closes.
	DispositionUnknown Disposition = iota
	// DispositionTransient: a fault a later attempt can genuinely outlast —
	// a provider blip, a throttle, a quota window, a cap an operator raises.
	// This is the CLI auto-resume loop's allow-list.
	DispositionTransient
	// DispositionInfrastructure: the RUN did not fail — the platform took
	// it away (a drain, a lost lease, a pod that never got placed, a dead
	// owner). A fresh pod is the cure, so the cloud redelivery path keeps
	// resuming these; the CLI loop never sees them.
	DispositionInfrastructure
	// DispositionReexecutable: the failing step is NONDETERMINISTIC — it is
	// decided by an LLM node's output — so re-executing it may well
	// succeed, but no amount of WAITING helps. Neither predicate below is
	// true for these: the cloud redelivery keeps re-running them (its
	// delivery budget is the bound), and the CLI's opt-in loop keeps
	// refusing them. Named rather than folded into either, because folding
	// it into deterministic would strand runs that recover on the next
	// sample, and into transient would make the CLI loop re-drive a run
	// whose cure is a different prompt.
	DispositionReexecutable
	// DispositionDeterministic: re-executing would run the same step
	// against the same inputs and reach the same verdict. An automatic
	// resume can only burn a pod; the run stays parked for an operator who
	// changes something (`iterion resume --force` after a fix).
	DispositionDeterministic
)

// classification is the single table. Every code the engine declares
// (store.ReservedFailureCodes) has exactly one row — the conformance test
// beside this file fails when a new code is declared and not classified,
// because an unclassified code silently falls back to today's behaviour on
// both surfaces, which is how the defect above survived.
var classification = map[store.FailureCode]Disposition{
	// Transient — a later attempt can differ, and waiting is what helps.
	store.FailureExecutionFailed:     DispositionTransient, // a backend fault surfacing past the in-executor retries
	store.FailureBudgetExceeded:      DispositionTransient, // with a RAISED cap; each surface adds that condition itself
	store.FailureTimeout:             DispositionTransient,
	store.FailureRateLimited:         DispositionTransient,
	store.FailureUsageLimitBlocked:   DispositionTransient, // the window reopens; the retry is armed for that instant
	store.FailureNetworkTransient:    DispositionTransient,
	store.FailureToolFailedTransient: DispositionTransient,

	// Infrastructure — the run never failed; a healthy pod is the cure.
	store.FailureInterrupted:         DispositionInfrastructure, // runner drain, lost heartbeat
	store.FailureProcessOrphaned:     DispositionInfrastructure, // a liveness probe found the owner dead
	store.FailureSandboxSetupTimeout: DispositionInfrastructure,
	store.FailureSandboxCapacity:     DispositionInfrastructure,

	// Re-executable — an LLM node decided it, so the next sample may pass.
	// Behaviour is unchanged for these: the point of naming them is that
	// they are NOT deterministic, however permanent the wording sounds.
	store.FailureSchemaValidation: DispositionReexecutable, // raised for an agent's output as well as a compute's
	store.FailureNoOutgoingEdge:   DispositionReexecutable, // the edge is chosen from the node's output
	store.FailureJoinFailed:       DispositionReexecutable, // a branch's own failure decides it
	// Not "the redelivery carries the same sealed credential": every claim
	// re-materialises the OAuth-forfait blob into a fresh file and refreshes
	// it on the spot when it is at or past its expiry lead
	// (runner.injectCredentials → startOAuthRefreshers). A refresh that
	// failed transiently on one attempt can succeed on the next, so the
	// EFFECTIVE token differs even though the sealed blob does not. Waiting
	// still helps nothing — only a fresh attempt does.
	store.FailureAuthFailed: DispositionReexecutable,

	// Deterministic — the same step against the same checkpoint, always the
	// same verdict. Nothing here is decided by a model.
	store.FailureExpressionFailed:    DispositionDeterministic, // a compute node: no LLM, no shell
	store.FailureToolFailedPermanent: DispositionDeterministic, // a fixed exit code on unchanged inputs
	store.FailureWorkspaceSafety:     DispositionDeterministic, // a static property of the graph
	store.FailureNodeNotFound:        DispositionDeterministic, // the graph does not change between attempts
	store.FailureLoopExhausted:       DispositionDeterministic, // the counter rides the checkpoint
	store.FailureResumeInvalid:       DispositionDeterministic, // the resume spec itself is the problem
	store.FailureFailNode:            DispositionDeterministic, // the workflow refused on purpose
	// The in-node recipe (recovery.ContextLengthRecipe) already compacted
	// TWICE and gave up with "compaction did not reduce context enough to
	// fit the model window"; a resume rehydrates the SAME persisted
	// conversation for the same node, so nothing makes the next attempt
	// smaller. Redelivering it burns MaxDeliver pods on one verdict.
	store.FailureContextLengthExceeded: DispositionDeterministic,
	store.FailureIRUnloadable:          DispositionDeterministic, // the same image compiles the same IR
	// The bundle's manifest names an engine floor this build is below. The
	// verdict is a comparison between two constants — the manifest's
	// `requires.iterion` and the pod's own build — so every redelivery
	// reaches it identically. This is the exact shape of the incident the
	// table was written for: a bot pushed for a newer engine burned seven
	// pods on one arithmetic.
	store.FailureBotRequiresNewerEngine: DispositionDeterministic,
	store.FailureQueueSchemaMismatch:    DispositionDeterministic, // the same runner rejects the same envelope
	store.FailureLaunchFailed:           DispositionDeterministic, // the run never left the launch path — no checkpoint exists
	store.FailureDLQParked:              DispositionDeterministic, // the deliveries are already spent
	// The odd one out, and deliberately so: this code is here for its
	// BEHAVIOUR (never auto-resumed), not for the table's usual reason. A
	// second attempt would NOT reach the same verdict — it might duplicate a
	// mutation that already landed. Parking is right for the opposite reason:
	// not "nothing would change", but "no automatic attempt may decide".
	// Only an operator who reconciled the remote state can.
	store.FailureAmbiguousEffect: DispositionDeterministic,
	store.FailureCancelled:       DispositionDeterministic, // an operator's decision, not a fault
	// The provider will not serve this model to this caller. Nothing in
	// the request's content is at fault, so a different sample cannot
	// help either — this is the one provider rejection that does not
	// belong with the LLM-decided codes above. Measured on 2026-09-07
	// (run 01a07da6): a claw node on a model the ChatGPT backend gates
	// on the client release failed identically NINE times in 77 seconds
	// — eight pods, eight sandboxes — because the 400 wore
	// EXECUTION_FAILED, whose disposition promises that waiting helps.
	store.FailureModelUnavailable: DispositionDeterministic,
	// The DECLARED schema, not the output measured against it: it rides
	// the IR, the request is never built, and no sample exists to differ.
	// Measured on 2026-09-07 (run 01a07db7): a `json` field's type union
	// the serving backend read as a single string — five attempts, four
	// pods, one verdict. SchemaValidation stays re-executable: there, a
	// request WAS served and the next sample may conform.
	store.FailureSchemaUnusable: DispositionDeterministic,
}

// Classify reports what an automatic resume can achieve for this code.
func Classify(code store.FailureCode) Disposition { return classification[code] }

// IsDeterministic reports that an automatic resume CANNOT change the
// outcome — the run must stay parked for an operator. False for an unknown
// code: a classifier that guessed "hopeless" would strand runs a fleet has
// always brought back.
func IsDeterministic(code store.FailureCode) bool {
	return Classify(code) == DispositionDeterministic
}

// AutoResumable reports that a bounded, backing-off retry loop should
// re-drive the run itself — the question the CLI's opt-in `--auto-resume`
// asks. Deliberately narrower than "not deterministic": an infrastructure
// failure is the CLOUD redelivery's business, and a local `iterion run` that
// was drained has no pod to move to.
func AutoResumable(code store.FailureCode) bool {
	return Classify(code) == DispositionTransient
}
