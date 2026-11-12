package protocol

// Readiness is emitted after guest validation and frame consumption, before
// target exec. It is not an attestation or proof that the target exec succeeds.
const Readiness = "IENV-READY-V1\x00\x00\x00"

// ProfileReadiness acknowledges the v2 frame actually consumed by the guest.
const ProfileReadiness = "IENV-READY-V2\x00\x00\x00"

// InvocationReadiness must be shared by both ends; v1 readers can be deployed
// before v2 writers, while a v1 guest refuses v2 instead of ignoring overrides.
func InvocationReadiness(in Invocation) string {
	if in.Profile != nil {
		return ProfileReadiness
	}
	return Readiness
}
