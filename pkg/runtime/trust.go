package runtime

import "github.com/SocialGouv/iterion/pkg/store"

// workspaceTrustSetter carries who wrote the workspace down to the backend,
// beside workDirSetter — see (*Engine).pushWorkspaceTrust. Only ClawExecutor
// implements it.
type workspaceTrustSetter interface {
	SetWorkspaceTrust(store.RunTrust)
}

// adoptRunTrust takes the workspace provenance from the run document when this
// engine was built without it.
//
// WithTrust is supplied by the surfaces that launch with a known trust (the
// cloud runner's entry points, `iterion run` under a carried one); `iterion
// resume`, the dispatcher, the in-process studio path and every local sub-bot
// build their engine from the run document instead, replaying launch
// decisions field by field. Trust is not a launch decision that may be
// dropped on the way back in: the stores hold the marker write-once, so the
// document is its durable truth, and an engine that reads a `fork` document
// while believing itself trusted hands every floor below it — the sandbox
// credential admission, the skill channel, the executor's tasks — the trusted
// zero value.
//
// One-way: it can only make an engine LESS trusted (store.RunTrust.Lowest). An
// engine told by its launching surface that the workspace is untrusted keeps
// that verdict whatever the document says.
func (e *Engine) adoptRunTrust(run *store.Run) {
	if run == nil {
		return
	}
	e.trust = e.trust.Lowest(run.Trust)
}

// pushWorkspaceTrust hands the executor the provenance of the workspace it is
// about to read, at the seam and the moment SetWorkDir is pushed: the two
// answer questions about the same directory, and a backend that knows where
// the checkout is must know who wrote it. Pushed rather than passed at
// construction because the executor is built by many launch surfaces and the
// workspace seam is two calls, so the push is the one place a new launch path
// cannot forget.
func (e *Engine) pushWorkspaceTrust() {
	if s, ok := e.executor.(workspaceTrustSetter); ok {
		s.SetWorkspaceTrust(e.trust)
	}
}
