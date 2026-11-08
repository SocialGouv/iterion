package cloudpublisher

// credentialTier and walkCredentialTiers are the common plan for actual
// resolution and its metadata-only preview. In particular the pool requires
// an entirely empty credential bundle, and a donor grant excludes platform.
type credentialTier int

const (
	credentialTierBYOK credentialTier = iota
	credentialTierGeneric
	credentialTierOAuth
	credentialTierOrg
	credentialTierPool
	credentialTierPlatform
	credentialTierRestore
)

// walkCredentialTiers walks the plan. poolTierAdmits is the ADR-164
// attachment answer: a resolved attachment ACTIVATES the pool tier for a
// run that holds credentials of its own — the tier's own per-slot loop
// then fills only the slots tiers 1-4 left empty. Without an attachment
// the empty-bundle gate is byte-identical to before (the documented
// donation contract).
func walkCredentialTiers(hasCredentials, poolGranted, poolTierAdmits func() bool, visit func(credentialTier) error) error {
	return walkCredentialPlan(hasCredentials, poolGranted, poolTierAdmits, func(tier credentialTier, active bool) error {
		if !active {
			return nil
		}
		return visit(tier)
	})
}

// walkCredentialPlan also exposes inactive tiers to metadata-only observers.
// Its active bit is the exact short-circuit applied by live resolution.
func walkCredentialPlan(hasCredentials, poolGranted, poolTierAdmits func() bool, visit func(credentialTier, bool) error) error {
	for tier := credentialTierBYOK; tier <= credentialTierRestore; tier++ {
		active := true
		if tier == credentialTierPool && hasCredentials() && !poolTierAdmits() {
			active = false
		}
		if tier == credentialTierPlatform && poolGranted() {
			active = false
		}
		if err := visit(tier, active); err != nil {
			return err
		}
	}
	return nil
}
