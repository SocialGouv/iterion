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

func walkCredentialTiers(hasCredentials, poolGranted func() bool, visit func(credentialTier) error) error {
	return walkCredentialPlan(hasCredentials, poolGranted, func(tier credentialTier, active bool) error {
		if !active {
			return nil
		}
		return visit(tier)
	})
}

// walkCredentialPlan also exposes inactive tiers to metadata-only observers.
// Its active bit is the exact short-circuit applied by live resolution.
func walkCredentialPlan(hasCredentials, poolGranted func() bool, visit func(credentialTier, bool) error) error {
	for tier := credentialTierBYOK; tier <= credentialTierRestore; tier++ {
		active := true
		if tier == credentialTierPool && hasCredentials() {
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
