package runview

import "errors"

// NoLLMCredentialErrorCode is the stable API code a launch or resume answers
// with when the cloud publisher refused it for want of an LLM credential.
const NoLLMCredentialErrorCode = "NO_LLM_CREDENTIAL"

// ErrNoLLMCredential is the publish-time refusal of a run that cannot start:
// every LLM route it may take pins a provider the deployment knows, and no
// credential tier holds one for any of them. Wrapped by the cloud publisher
// (with the providers and the tiers consulted) when the deployment opts in;
// a run the runner could still fund from its pod's ambient env is never
// refused with it. A launch refusal, not a run failure: no run exists.
var ErrNoLLMCredential = errors.New("no LLM credential for this run")
