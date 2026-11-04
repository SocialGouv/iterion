package credroute

import "github.com/SocialGouv/iterion/pkg/secrets"

// KeySpend is the spend of the run key filling provider's slot: the run's
// own default key (run key) or, absent one, a key a shared tier sealed for
// the route (pinned key) — the order Credentials.APIKeyForRoute reads — with
// the slot's audit fingerprint.
func KeySpend(creds secrets.Credentials, provider secrets.Provider) Spend {
	channel := ChannelRunKey
	if creds.APIKey(provider) == "" && creds.PinnedAPIKey(provider) != "" {
		channel = ChannelPinnedKey
	}
	return Spend{Channel: channel, Slot: string(provider), Fingerprint: creds.Fingerprint(string(provider))}
}

// ForfaitSpend is the spend of the subscription of kind the run sealed.
func ForfaitSpend(creds secrets.Credentials, kind secrets.OAuthKind) Spend {
	return Spend{Channel: ChannelRunForfait, Slot: string(kind), Fingerprint: creds.Fingerprint(string(kind))}
}
